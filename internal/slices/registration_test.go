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

const agentPath = modulePath + "/internal/agent"

// registerFuncs are the agent funcs a slice calls, from init or a helper it
// calls, to wire itself in.
var registerFuncs = map[string]bool{"RegisterStateChange": true, "RegisterAutomation": true}

// registerCalls counts the calls to agent.RegisterStateChange and
// agent.RegisterAutomation inside any func body of the non-test .go files the
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
		agentName := ""
		for _, spec := range f.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); path == agentPath {
				agentName = "agent"
				if spec.Name != nil {
					agentName = spec.Name.Name
				}
			}
		}
		if agentName == "" {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isRegisterCall(call, agentName) {
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

// isRegisterCall reports whether call is a register func of the agent package,
// imported as agentName: agentName.RegisterX(...), or a bare RegisterX(...)
// when the agent package is dot-imported.
func isRegisterCall(call *ast.CallExpr, agentName string) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		return ok && pkg.Name == agentName && registerFuncs[fun.Sel.Name]
	case *ast.Ident:
		return agentName == "." && registerFuncs[fun.Name]
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
			t.Errorf("slice %s has no func init calling agent.RegisterStateChange or agent.RegisterAutomation, so it is never wired in; register it from init()", filepath.Join("internal/slices", dir))
		}
	}
}

func TestRegisterCallsCountsAgentRegistrations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		{"no init", `package s`, 0},
		{"registration without init", `package s
import "heinzel/internal/agent"
func Init() { agent.RegisterAutomation(nil) }`, 0},
		{"init without registration", `package s
func init() { println() }`, 0},
		{"registration through a helper called from init", `package s
import "heinzel/internal/agent"
func init() { register() }
func register() { agent.RegisterAutomation(nil) }`, 1},
		{"dot import", `package s
import . "heinzel/internal/agent"
func init() { RegisterAutomation(nil) }`, 1},
		{"bare call without dot import", `package s
func init() { RegisterAutomation(nil) }
func RegisterAutomation(any) {}`, 0},
		{"same name on another package", `package s
import agent "example.com/other"
func init() { agent.RegisterAutomation(nil) }`, 0},
		{"state change", `package s
import "heinzel/internal/agent"
func init() { agent.RegisterStateChange(nil) }`, 1},
		{"renamed import, both kinds", `package s
import a "heinzel/internal/agent"
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
			register := "package s\nimport \"heinzel/internal/agent\"\nfunc init() { agent.RegisterAutomation(nil) }"
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
