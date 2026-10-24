package slices_test

import (
	"bytes"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// isSliceDir reports whether entry, found in parent, is a slice package: a
// directory not named testdata, not starting with "." or "_", that holds at
// least one non-test .go file the current build includes (build constraints
// such as //go:build ignore or a _windows.go suffix apply). gen.go has an
// identical copy, since it is a standalone program;
// TestGenUsesTheSameSliceDirRule keeps the two equal.
func isSliceDir(parent string, entry fs.DirEntry) (bool, error) {
	name := entry.Name()
	if !entry.IsDir() || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return false, nil
	}
	pkg, err := build.Default.ImportDir(filepath.Join(parent, name), 0)
	var noGo *build.NoGoError
	if errors.As(err, &noGo) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(pkg.GoFiles) > 0, nil
}

// sliceDirs returns the names of the slice packages directly under root,
// sorted.
func sliceDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		ok, err := isSliceDir(root, entry)
		if err != nil {
			return nil, err
		}
		if ok {
			dirs = append(dirs, entry.Name())
		}
	}
	slices.Sort(dirs)
	return dirs, nil
}

// mustSliceDirs returns the slice packages in the current directory and fails
// the test if there are none.
func mustSliceDirs(t *testing.T) []string {
	t.Helper()
	dirs, err := sliceDirs(".")
	if err != nil {
		t.Fatalf("list slice dirs: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("found no slice packages")
	}
	return dirs
}

// otherGOOS returns an OS other than the one the tests run on, so that a
// file named x_<otherGOOS>.go is excluded from the build on any host.
func otherGOOS() string {
	if runtime.GOOS == "windows" {
		return "linux"
	}
	return "windows"
}

func TestSliceDirsSkipsWhatIsNotASlicePackage(t *testing.T) {
	root := t.TempDir()
	const pkg = "package p\n"
	files := map[string]string{
		"good/good.go":                     pkg,
		"good/good_test.go":                pkg,
		"testdata/fixture.go":              pkg,
		".hidden/hidden.go":                pkg,
		"_old/old.go":                      pkg,
		"onlytests/onlytests_test.go":      pkg,
		"nogo/README.md":                   "",
		"otheros/x_" + otherGOOS() + ".go": pkg,
		"ignored/x.go":                     "//go:build ignore\n\n" + pkg,
		"root.go":                          pkg,
	}
	for file, src := range files {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := sliceDirs(root)
	if err != nil {
		t.Fatalf("sliceDirs: %v", err)
	}
	if want := []string{"good"}; !slices.Equal(got, want) {
		t.Errorf("sliceDirs = %v, want %v", got, want)
	}
}

// funcSource returns the printed source of the top-level func name in file,
// without its doc comment.
func funcSource(t *testing.T, file, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, fn); err != nil {
				t.Fatalf("print %s in %s: %v", name, file, err)
			}
			return buf.String()
		}
	}
	t.Fatalf("%s declares no func %s", file, name)
	return ""
}

func TestGenUsesTheSameSliceDirRule(t *testing.T) {
	gen := funcSource(t, "gen.go", "isSliceDir")
	test := funcSource(t, "slicedirs_test.go", "isSliceDir")
	if gen != test {
		t.Errorf("isSliceDir in gen.go differs from slicedirs_test.go; keep them identical\ngen.go:\n%s\nslicedirs_test.go:\n%s", gen, test)
	}
}
