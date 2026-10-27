package slices_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath   = "heinzel"
	slicesPath   = modulePath + "/internal/slices"
	eventsPath   = modulePath + "/internal/events"
	platformPath = modulePath + "/internal/platform"
	// eventStorePath is the one module package the events package may
	// import, for the NewEvent and Event its codec works on.
	eventStorePath = platformPath + "/eventstore"
)

// eventsCodecFuncs are the only funcs the events package may declare.
var eventsCodecFuncs = map[string]bool{"Encode": true, "Decode": true, "decodeAs": true}

func within(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }

// sliceMayImport reports whether the slice at import path self (or one of
// its sub-packages) may import path: anything outside this module (the
// stdlib and third-party libraries), the shared events, the shared
// platform packages, and the slice's own packages. Never another slice.
func sliceMayImport(self string) func(path string) bool {
	return func(path string) bool {
		return !within(path, modulePath) || path == eventsPath || within(path, platformPath) || within(path, self)
	}
}

// platformMayImport reports whether a shared platform package may import
// path: anything but a slice, so that slices cannot share code through it.
func platformMayImport(path string) bool { return !within(path, slicesPath) }

// importViolations lists the imports in the .go files under dir (tests and
// sub-packages included) that allowed rejects.
func importViolations(dir string, allowed func(path string) bool) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range f.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if !allowed(imported) {
				rel, _ := filepath.Rel(dir, path)
				violations = append(violations, fmt.Sprintf("%s imports %s", filepath.ToSlash(rel), imported))
			}
		}
		return nil
	})
	return violations, err
}

func TestSlicesImportOnlySharedEventsPlatformAndTheirOwnPackages(t *testing.T) {
	for _, dir := range mustSliceDirs(t) {
		violations, err := importViolations(dir, sliceMayImport(slicesPath+"/"+dir))
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		for _, v := range violations {
			t.Errorf("slice %s: %s; a slice may import only the stdlib, third-party libraries, %s, %s/... and its own packages", dir, v, eventsPath, platformPath)
		}
	}
}

func TestThePlatformImportsNoSlice(t *testing.T) {
	violations, err := importViolations("../platform", platformMayImport)
	if err != nil {
		t.Fatalf("scan platform: %v", err)
	}
	for _, v := range violations {
		t.Errorf("platform: %s; the platform may not import %s/...", v, slicesPath)
	}
}

func TestPlatformMayImport(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"net/http", true},
		{"github.com/gin-gonic/gin", true},
		{"heinzel/internal/events", true},
		{"heinzel/internal/platform/eventstore", true},
		{"heinzel/internal/slicesx", true},
		{"heinzel/internal/slices", false},
		{"heinzel/internal/slices/alpha", false},
		{"heinzel/internal/slices/alpha/git", false},
	}
	for _, tt := range tests {
		if got := platformMayImport(tt.path); got != tt.want {
			t.Errorf("platformMayImport(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestImportViolations(t *testing.T) {
	const self = slicesPath + "/alpha"
	tests := []struct {
		name  string
		files map[string]string // path in the slice dir -> its imports
		want  []string
	}{
		{"allowed", map[string]string{
			"alpha.go":      `"context"; "github.com/gin-gonic/gin"; "heinzel/internal/events"; "heinzel/internal/platform/app"; "heinzel/internal/slices/alpha/git"`,
			"alpha_test.go": `"testing"; "heinzel/internal/platform/eventstore"; "heinzel/internal/slices/alpha"`,
			"git/git.go":    `"os/exec"; "heinzel/internal/slices/alpha/git/internal"`,
		}, nil},
		{"another slice", map[string]string{"alpha.go": `"heinzel/internal/slices/beta"`}, []string{"alpha.go imports heinzel/internal/slices/beta"}},
		{"another slice in a test", map[string]string{"alpha_test.go": `"heinzel/internal/slices/beta"`}, []string{"alpha_test.go imports heinzel/internal/slices/beta"}},
		{"a sibling slice from a sub-package", map[string]string{"git/git.go": `"heinzel/internal/slices/beta/git"`}, []string{"git/git.go imports heinzel/internal/slices/beta/git"}},
		{"a slice sharing its name prefix", map[string]string{"alpha.go": `"heinzel/internal/slices/alphabet"`}, []string{"alpha.go imports heinzel/internal/slices/alphabet"}},
		{"the slices parent", map[string]string{"alpha.go": `"heinzel/internal/slices"`}, []string{"alpha.go imports heinzel/internal/slices"}},
		{"a non-shared package", map[string]string{"alpha.go": `"heinzel/internal/util"`}, []string{"alpha.go imports heinzel/internal/util"}},
		{"a package under events", map[string]string{"alpha.go": `"heinzel/internal/events/helpers"`}, []string{"alpha.go imports heinzel/internal/events/helpers"}},
		{"a package posing as platform", map[string]string{"alpha.go": `"heinzel/internal/platformx"`}, []string{"alpha.go imports heinzel/internal/platformx"}},
		{"the main package", map[string]string{"alpha.go": `"heinzel/cmd/heinzel"`}, []string{"alpha.go imports heinzel/cmd/heinzel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for file, imports := range tt.files {
				path := filepath.Join(dir, filepath.FromSlash(file))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				src := "package p\n\nimport (" + strings.ReplaceAll(imports, ";", "\n") + ")\n"
				if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got, err := importViolations(dir, sliceMayImport(self))

			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestTheEventsPackageHoldsOnlyContracts(t *testing.T) {
	files, err := filepath.Glob("../events/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("found no events files (err %v)", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, spec := range f.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if (strings.Contains(first, ".") || first == modulePath) && path != eventStorePath {
				t.Errorf("%s imports %s; the events package may import only the stdlib and %s", file, path, eventStorePath)
			}
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			switch {
			case !ok:
			case fn.Recv != nil && fn.Name.Name != "EventType":
				t.Errorf("%s declares method %s; a contract's only method is EventType", file, fn.Name.Name)
			case fn.Recv == nil && !eventsCodecFuncs[fn.Name.Name]:
				t.Errorf("%s declares func %s; the events package holds contracts and their codec only", file, fn.Name.Name)
			}
		}
	}
}
