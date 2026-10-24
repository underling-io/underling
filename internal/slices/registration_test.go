package slices_test

import (
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

const appPath = modulePath + "/internal/app"

// registerFuncs are the app funcs a slice calls, from init or a helper it
// calls, to wire itself in.
var registerFuncs = map[string]bool{"RegisterStateChange": true, "RegisterAutomation": true}

// registerCalls counts the calls to app.RegisterStateChange and
// app.RegisterAutomation inside any func body of the non-test .go files the
// current build includes in dir, so registering through a helper called from
// init counts too. Without a func init nothing runs them, so it counts zero.
func registerCalls(dir string) (int, error) {
	pkg, err := build.Default.ImportDir(dir, 0)
	var noGo *build.NoGoError
	if errors.As(err, &noGo) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count, hasInit := 0, false
	for _, name := range pkg.GoFiles {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			return 0, err
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
				hasInit = true
			}
		}
		appName := ""
		for _, spec := range f.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); path == appPath {
				appName = "app"
				if spec.Name != nil {
					appName = spec.Name.Name
				}
			}
		}
		if appName == "" {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isRegisterCall(call, appName) {
					count++
				}
				return true
			})
		}
	}
	if !hasInit {
		return 0, nil
	}
	return count, nil
}

// isRegisterCall reports whether call is a register func of the app package,
// imported as appName: appName.RegisterX(...), or a bare RegisterX(...)
// when the app package is dot-imported.
func isRegisterCall(call *ast.CallExpr, appName string) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		return ok && pkg.Name == appName && registerFuncs[fun.Sel.Name]
	case *ast.Ident:
		return appName == "." && registerFuncs[fun.Name]
	}
	return false
}

func TestEverySliceRegisters(t *testing.T) {
	for _, dir := range mustSliceDirs(t) {
		n, err := registerCalls(dir)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		if n == 0 {
			t.Errorf("slice %s has no func init calling app.RegisterStateChange or app.RegisterAutomation, so it is never wired in; register it from init()", filepath.Join("internal/slices", dir))
		}
	}
}

func TestRegisterCallsCountsAppRegistrations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		{"no init", `package s`, 0},
		{"registration without init", `package s
import "heinzel/internal/app"
func Init() { app.RegisterAutomation(nil) }`, 0},
		{"init without registration", `package s
func init() { println() }`, 0},
		{"registration through a helper called from init", `package s
import "heinzel/internal/app"
func init() { register() }
func register() { app.RegisterAutomation(nil) }`, 1},
		{"dot import", `package s
import . "heinzel/internal/app"
func init() { RegisterAutomation(nil) }`, 1},
		{"bare call without dot import", `package s
func init() { RegisterAutomation(nil) }
func RegisterAutomation(any) {}`, 0},
		{"same name on another package", `package s
import app "example.com/other"
func init() { app.RegisterAutomation(nil) }`, 0},
		{"state change", `package s
import "heinzel/internal/app"
func init() { app.RegisterStateChange(nil) }`, 1},
		{"renamed import, both kinds", `package s
import a "heinzel/internal/app"
func init() {
	a.RegisterStateChange(nil)
	a.RegisterAutomation(nil)
}`, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "s.go"), []byte(tt.src), 0o644); err != nil {
				t.Fatal(err)
			}
			// Registrations in a test file, or in a file the build excludes,
			// don't wire the slice in.
			register := "package s\nimport \"heinzel/internal/app\"\nfunc init() { app.RegisterAutomation(nil) }"
			for _, name := range []string{"s_test.go", "s_" + otherGOOS() + ".go"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(register), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := registerCalls(dir)
			if err != nil {
				t.Fatalf("registerCalls: %v", err)
			}
			if got != tt.want {
				t.Errorf("registerCalls = %d, want %d", got, tt.want)
			}
		})
	}
}
