package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/saibing/bingo/langserver/internal/util"

	"golang.org/x/tools/go/packages"
)

type gopath struct {
	mu         sync.RWMutex
	project    *Project
	rootDir    string
	importPath string
}

func newGopath(gc *Project, rootDir string) *gopath {
	return &gopath{project: gc, rootDir: rootDir}
}

func (p *gopath) init() (err error) {
	err = p.doInit()
	if err != nil {
		return err
	}

	_, err = p.buildCache()
	return err
}

func (p *gopath) doInit() error {
	if strings.HasPrefix(p.rootDir, util.LowerDriver(filepath.ToSlash(p.project.goroot))) {
		p.importPath = ""
		return nil
	}

	gopath := os.Getenv(gopathEnv)
	if gopath == "" {
		gopath = filepath.Join(os.Getenv("HOME"), "go")
	}

	paths := strings.Split(gopath, string(os.PathListSeparator))

	for _, path := range paths {
		path = util.LowerDriver(filepath.ToSlash(path))
		if strings.HasPrefix(p.rootDir, path) && p.rootDir != path {
			srcDir := filepath.Join(path, "src")
			if p.rootDir == srcDir {
				continue
			}

			p.importPath = filepath.ToSlash(p.rootDir[len(srcDir)+1:])
			return nil
		}
	}

	return fmt.Errorf("%s is out of GOPATH workspace %v, but not a go module project", p.rootDir, paths)
}

func (p *gopath) rebuildCache() (bool, error) {
	_, err := p.buildCache()
	return err == nil, err
}

func (p *gopath) buildCache() ([]*packages.Package, error) {
	p.project.view.mu.Lock()
	defer p.project.view.mu.Unlock()

	cfg := p.project.view.Config
	cfg.Dir = p.rootDir
	cfg.ParseFile = nil

	var pattern string
	if filepath.Join(p.project.goroot, BuiltinPkg) == p.rootDir {
		pattern = cfg.Dir
	} else {
		pattern = p.importPath + "/..."
	}

	return packages.Load(cfg, pattern)
}

