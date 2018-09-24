package langserver

import (
	"context"
	"errors"
	"fmt"
	"github.com/saibing/bingo/langserver/internal/caches"
	"go/ast"
	"go/token"
	"go/types"
	"golang.org/x/tools/go/packages"
	"sort"

	"github.com/saibing/bingo/langserver/util"
	"github.com/saibing/bingo/pkg/lsp"
	"github.com/saibing/bingo/pkg/lspext"
	"github.com/sourcegraph/jsonrpc2"
	"golang.org/x/tools/go/types/typeutil"
)

func (h *LangHandler) handleTextDocumentImplementation(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]*lspext.ImplementationLocation, error) {
	if !util.IsURI(params.TextDocument.URI) {
		return nil, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInvalidParams,
			Message: fmt.Sprintf("textDocument/implementation not yet supported for out-of-workspace URI (%q)", params.TextDocument.URI),
		}
	}

	// Do initial cached, standard typecheck pass to get position arg.
	pkg, start, err := h.loadPackage(ctx, conn, params.TextDocument.URI, params.Position)
	if err != nil {
		// Invalid nodes means we tried to click on something which is
		// not an ident (eg comment/string/etc). Return no information.
		if _, ok := err.(*invalidNodeError); ok {
			return []*lspext.ImplementationLocation{}, nil
		}
		return nil, err
	}

	pos := posForFileOffset(pkg.Fset, pkg.Fset.Position(start).Filename, pkg.Fset.Position(start).Offset)
	path, _, _ := getPathNode(pkg, pos, pos)
	path, action := findInterestingNode(pkg, path)

	return implements(h.packageCache, pkg, path, action)
}

// Adapted from golang.org/x/tools/cmd/guru (Copyright (c) 2013 The Go Authors). All rights
// reserved. See NOTICE for full license.
func implements(packageCache *caches.PackageCache, pkg *packages.Package, path []ast.Node, action action) ([]*lspext.ImplementationLocation, error) {
	var method *types.Func
	var T types.Type // selected type (receiver if method != nil)

	switch action {
	case actionExpr:
		// method?
		if id, ok := path[0].(*ast.Ident); ok {
			if obj, ok := pkg.TypesInfo.ObjectOf(id).(*types.Func); ok {
				recv := obj.Type().(*types.Signature).Recv()
				if recv == nil {
					return nil, errors.New("this function is not a method")
				}
				method = obj
				T = recv.Type()
			}
		}

		// If not a method, use the expression's type.
		if T == nil {
			T = pkg.TypesInfo.TypeOf(path[0].(ast.Expr))
		}

	case actionType:
		T = pkg.TypesInfo.TypeOf(path[0].(ast.Expr))
	}
	if T == nil {
		return nil, errors.New("not a type, method, or value")
	}

	// Find all named types, even local types (which can have
	// methods due to promotion) and the built-in "error".
	// We ignore aliases 'type M = N' to avoid duplicate
	// reporting of the Named type N.
	var allNamed []*types.Named

	f := func(p *packages.Package) error {
		for _, obj := range p.TypesInfo.Defs {
			if obj, ok := obj.(*types.TypeName); ok && !isAlias(obj) {
				if named, ok := obj.Type().(*types.Named); ok {
					allNamed = append(allNamed, named)
				}
			}
		}

		return nil
	}

	err := packageCache.Iterate(f)
	if err != nil {
		return nil, err
	}

	allNamed = append(allNamed, types.Universe.Lookup("error").Type().(*types.Named))

	var msets typeutil.MethodSetCache

	// Test each named type.
	var to, from, fromPtr []types.Type
	for _, U := range allNamed {
		if isInterface(T) {
			if msets.MethodSet(T).Len() == 0 {
				continue // empty interface
			}
			if isInterface(U) {
				if msets.MethodSet(U).Len() == 0 {
					continue // empty interface
				}

				// T interface, U interface
				if !types.Identical(T, U) {
					if types.AssignableTo(U, T) {
						to = append(to, U)
					}
					if types.AssignableTo(T, U) {
						from = append(from, U)
					}
				}
			} else {
				// T interface, U concrete
				if types.AssignableTo(U, T) {
					to = append(to, U)
				} else if pU := types.NewPointer(U); types.AssignableTo(pU, T) {
					to = append(to, pU)
				}
			}
		} else if isInterface(U) {
			if msets.MethodSet(U).Len() == 0 {
				continue // empty interface
			}

			// T concrete, U interface
			if types.AssignableTo(T, U) {
				from = append(from, U)
			} else if pT := types.NewPointer(T); types.AssignableTo(pT, U) {
				fromPtr = append(fromPtr, U)
			}
		}
	}

	// Sort types (arbitrarily) to ensure test determinism.
	sort.Sort(typesByString(to))
	sort.Sort(typesByString(from))
	sort.Sort(typesByString(fromPtr))

	seen := map[types.Object]struct{}{}
	toLocation := func(t types.Type, method *types.Func) *lspext.ImplementationLocation {
		var obj types.Object
		if method == nil {
			// t is a type
			nt, ok := deref(t).(*types.Named)
			if !ok {
				return nil // t is non-named
			}
			obj = nt.Obj()
		} else {
			// t is a method
			tm := types.NewMethodSet(t).Lookup(method.Pkg(), method.Name())
			if tm == nil {
				return nil // method not found
			}
			obj = tm.Obj()
			if _, seen := seen[obj]; seen {
				return nil // already saw this method, via other embedding path
			}
			seen[obj] = struct{}{}
		}

		pos := obj.Pos()
		end := obj.Pos() + token.Pos(len(obj.Name()))
		return &lspext.ImplementationLocation{
			Location: goRangeToLSPLocation(pkg.Fset, pos, end),
			Method:   method != nil,
		}
	}

	locs := make([]*lspext.ImplementationLocation, 0, len(to)+len(from)+len(fromPtr))
	for _, t := range to {
		loc := toLocation(t, method)
		if loc == nil {
			continue
		}
		loc.Type = "to"
		locs = append(locs, loc)
	}
	for _, t := range from {
		loc := toLocation(t, method)
		if loc == nil {
			continue
		}
		loc.Type = "from"
		locs = append(locs, loc)
	}
	for _, t := range fromPtr {
		loc := toLocation(t, method)
		if loc == nil {
			continue
		}
		loc.Type = "from"
		loc.Ptr = true
		locs = append(locs, loc)
	}
	return locs, nil
}

func isInterface(T types.Type) bool { return types.IsInterface(T) }

type typesByString []types.Type

func (p typesByString) Len() int           { return len(p) }
func (p typesByString) Less(i, j int) bool { return p[i].String() < p[j].String() }
func (p typesByString) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
