package slices_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath = "heinzel"
	slicesPath = modulePath + "/internal/slices"
)

// allowedShared are the only non-stdlib packages a slice may import: the
// shared event contracts and generic infrastructure.
var allowedShared = map[string]bool{
	modulePath + "/internal/events":       true,
	modulePath + "/internal/eventstore":   true,
	modulePath + "/internal/eventhandler": true,
	modulePath + "/internal/agent":        true,
}

// isStdlib applies the go command's rule (no dot in the first path element),
// but first excludes this module, whose path has no dot either.
func isStdlib(path string) bool {
	if path == modulePath || strings.HasPrefix(path, modulePath+"/") {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// imports returns the import paths of every .go file under dir, tests included,
// keyed by file.
func imports(t *testing.T, dir string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range f.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			found[path] = append(found[path], importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	return found
}

func TestSlicesImportOnlyStdlibSharedEventsAndInfrastructure(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read slices dir: %v", err)
	}
	sliceCount := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sliceCount++
		self := slicesPath + "/" + entry.Name()
		for file, paths := range imports(t, entry.Name()) {
			for _, path := range paths {
				ownPackage := path == self || strings.HasPrefix(path, self+"/")
				if !isStdlib(path) && !allowedShared[path] && !ownPackage {
					t.Errorf("%s imports %s; slices may only import the stdlib, their own package and %v", file, path, slices.Sorted(maps.Keys(allowedShared)))
				}
			}
		}
	}
	if sliceCount == 0 {
		t.Fatal("found no slice packages")
	}
}

func TestEventsPackageHoldsOnlyContracts(t *testing.T) {
	const dir = "../events"
	for file, paths := range imports(t, dir) {
		for _, path := range paths {
			if !isStdlib(path) {
				t.Errorf("%s imports %s; the events package may only import the stdlib", file, path)
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("found no events files (err %v)", err)
	}
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				t.Errorf("%s declares func %s; the events package holds contracts only", file, fn.Name.Name)
			}
		}
	}
}
