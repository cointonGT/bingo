package langserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"golang.org/x/tools/imports"

	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/go-lsp/lspext"
	"github.com/sourcegraph/jsonrpc2"

	"github.com/saibing/bingo/langserver/internal/cache"
	"github.com/saibing/bingo/langserver/internal/util"
)

// NewHandler creates a Go language server handler.
func NewHandler(defaultCfg Config) jsonrpc2.Handler {
	langHandler := &LangHandler{
		DefaultConfig: defaultCfg,
		HandlerShared: &HandlerShared{},
	}
	handleFunc := langHandler.handle
	return lspHandler{jsonrpc2.HandlerWithError(handleFunc)}
}

// lspHandler wraps LangHandler to correctly handle requests in the correct
// order.
//
// The LSP spec dictates a strict ordering that requests should only be
// processed serially in the order they are received. However, implementations
// are allowed to do concurrent computation if it doesn't affect the
// result. We actually can return responses out of order, since vscode does
// not seem to have issues with that. We also do everything concurrently,
// except methods which could mutate the state used by our typecheckers (ie
// textDocument/didOpen, etc). Those are done serially since applying them out
// of order could result in a different textDocument.
type lspHandler struct {
	jsonrpc2.Handler
}

// Handle implements jsonrpc2.Handler
func (h lspHandler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if isFileSystemRequest(req.Method) {
		h.Handler.Handle(ctx, conn, req)
		return
	}
	go h.Handler.Handle(ctx, conn, req)
}

// LangHandler is a Go language server LSP/JSON-RPC handler.
type LangHandler struct {
	HandlerCommon
	*HandlerShared
	mu      sync.Mutex
	init    *InitializeParams // set by "initialize" request
	project *cache.Project
	cancel  *cancel
	// DefaultConfig is the default values used for configuration. It is
	// combined with InitializationOptions after initialize. This should be
	// set by LangHandler creators. Please read config instead.
	DefaultConfig Config
	// config is the language handler configuration. It is a combination of
	// DefaultConfig and InitializationOptions.
	config *Config // pointer so we panic if someone reads before we set it.
}

// doInit clears all internal state in h.
func (h *LangHandler) doInit(ctx context.Context, conn *jsonrpc2.Conn, init *InitializeParams) error {
	if util.IsURI(lsp.DocumentURI(init.InitializeParams.RootPath)) {
		log.Printf("Passing an initialize rootPath URI (%q) is deprecated. Use rootUri instead.", init.InitializeParams.RootPath)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	config := h.DefaultConfig.Apply(init.InitializationOptions)
	h.config = &config
	imports.LocalPrefix = h.config.GoimportsLocalPrefix
	h.init = init
	h.cancel = NewCancel()

	rootPath := h.FilePath(init.Root())
	buildFlags := []string{}
	if len(h.config.BuildTags) > 0 {
		buildFlags = append(buildFlags, "-tags", strings.Join(h.config.BuildTags, " "))
	}
	h.project = cache.NewProject(ctx, conn, rootPath, buildFlags)
	h.overlay = newOverlay(conn, h.project, DiagnosticsStyleEnum(h.DefaultConfig.DiagnosticsStyle))
	if err := h.project.Init(ctx, cache.CacheStyle(h.DefaultConfig.GlobalCacheStyle)); err != nil {
		return err
	}
	return nil
}

// handle implements jsonrpc2.Handler.
func (h *LangHandler) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	return h.Handle(ctx, conn, req)
}

// Handle creates a response for a JSONRPC2 LSP request. Note: LSP has strict
// ordering requirements, so this should not just be wrapped in an
// jsonrpc2.AsyncHandler. Ensure you have the same ordering as used in the
// NewHandler implementation.
func (h *LangHandler) Handle(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request) (result interface{}, err error) {
	// Prevent any uncaught panics from taking the entire server down.
	defer func() {
		if perr := util.Panicf(recover(), "%v", req.Method); perr != nil {
			err = perr
		}
	}()

	h.mu.Lock()
	cancelManager := h.cancel
	if req.Method != "initialize" && h.init == nil {
		h.mu.Unlock()
		return nil, errors.New("server must be initialized")
	}
	h.mu.Unlock()
	if err := h.CheckReady(); err != nil {
		if req.Method == "exit" {
			err = nil
		}
		return nil, err
	}

	// Notifications don't have an ID, so they can't be cancelled
	if cancelManager != nil && !req.Notif {
		var cancel func()
		ctx, cancel = cancelManager.WithCancel(ctx, req.ID)
		defer cancel()
	}

	switch req.Method {
	case "initialize":
		if h.init != nil {
			return nil, errors.New("language server is already initialized")
		}
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params InitializeParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}

		// HACK: RootPath is not a URI, but historically we treated it
		// as such. Convert it to a file URI
		if params.RootPath != "" && !util.IsURI(lsp.DocumentURI(params.RootPath)) {
			params.RootPath = string(util.PathToURI(params.RootPath))
		}

		if err := h.doInit(ctx, conn.(*jsonrpc2.Conn), &params); err != nil {
			return nil, err
		}

		kind := lsp.TDSKIncremental
		completionOp := &lsp.CompletionOptions{TriggerCharacters: []string{"."}}
		signatureHelpProvider := &lsp.SignatureHelpOptions{TriggerCharacters: []string{"(", ","}}
		return lsp.InitializeResult{
			Capabilities: lsp.ServerCapabilities{
				TextDocumentSync: &lsp.TextDocumentSyncOptionsOrKind{
					Kind:    &kind,
					Options: &lsp.TextDocumentSyncOptions{OpenClose: true},
				},
				CodeActionProvider:              false,
				CompletionProvider:              completionOp,
				DefinitionProvider:              true,
				TypeDefinitionProvider:          true,
				DocumentFormattingProvider:      true,
				DocumentRangeFormattingProvider: true,
				DocumentSymbolProvider:          true,
				HoverProvider:                   true,
				ReferencesProvider:              true,
				RenameProvider:                  true,
				WorkspaceSymbolProvider:         true,
				ImplementationProvider:          true,
				XWorkspaceReferencesProvider:    true,
				XDefinitionProvider:             true,
				XWorkspaceSymbolByProperties:    true,
				SignatureHelpProvider:           signatureHelpProvider,
			},
		}, nil

	case "initialized":
		// A notification that the client is ready to receive requests. Ignore
		return nil, nil

	case "shutdown":
		h.ShutDown()
		return nil, nil

	case "exit":
		if c, ok := conn.(*jsonrpc2.Conn); ok {
			c.Close()
		}
		return nil, nil

	case "$/cancelRequest":
		// notification, don't send back results/errors
		if req.Params == nil {
			return nil, nil
		}
		var params lsp.CancelParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, nil
		}
		if cancelManager == nil {
			return nil, nil
		}
		cancelManager.Cancel(jsonrpc2.ID{
			Num:      params.ID.Num,
			Str:      params.ID.Str,
			IsString: params.ID.IsString,
		})
		return nil, nil

	case "textDocument/hover":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.TextDocumentPositionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleHover(ctx, conn, req, params)

	case "textDocument/definition":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.TextDocumentPositionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleDefinition(ctx, conn, req, params)

	case "textDocument/typeDefinition":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.TextDocumentPositionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTypeDefinition(ctx, conn, req, params)

	case "textDocument/xdefinition":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.TextDocumentPositionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleXDefinition(ctx, conn, req, params)

	case "textDocument/completion":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.CompletionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTextDocumentCompletion(ctx, conn, req, params)

	case "textDocument/references":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.ReferenceParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTextDocumentReferences(ctx, conn, req, params)

	case "textDocument/implementation":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.TextDocumentPositionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTextDocumentImplementation(ctx, conn, req, params)

	case "textDocument/documentSymbol":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.DocumentSymbolParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTextDocumentSymbol(ctx, conn, req, params)

	case "textDocument/signatureHelp":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.TextDocumentPositionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTextDocumentSignatureHelp(ctx, conn, req, params)

	case "textDocument/formatting":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.DocumentFormattingParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTextDocumentFormatting(ctx, conn, req, params)

	case "textDocument/rangeFormatting":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.DocumentRangeFormattingParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleTextDocumentRangeFormatting(ctx, conn, req, params)

	case "workspace/symbol":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lspext.WorkspaceSymbolParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleWorkspaceSymbol(ctx, conn, req, params)

	case "workspace/xreferences":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lspext.WorkspaceReferencesParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleWorkspaceReferences(ctx, conn, req, params)

	case "textDocument/rename":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.RenameParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}
		return h.handleRename(ctx, conn, req, params)

	case "textDocument/codeAction":
		if req.Params == nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
		}
		var params lsp.CodeActionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, err
		}

		return h.handleCodeAction(ctx, conn, req, params)

	default:
		if isFileSystemRequest(req.Method) {
			err := h.handleFileSystemRequest(ctx, req)
			return nil, err
		}

		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: fmt.Sprintf("method not supported: %s", req.Method)}
	}
}
