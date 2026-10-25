package investigatesentryissue_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The test binary doubles as a fake claude: the slice runs it as CLAUDE_BIN,
// and the fake recognises itself by its first argument, which the real
// test runner never passes. Its env holds only what the slice hands the
// child, so it reads its mode from, and records each call to, files in the
// work dir: two levels above its working directory, the clone.
const (
	fakeModeFile  = "fake-claude-mode"
	fakeCallsFile = "fake-claude-calls.jsonl"

	// Fake modes.
	modeHappy = "happy" // commit a file, then report success
	modeNoop  = "noop"  // report success without committing

	fakeSummary = "Root cause: blooopy was never declared. Fix: declare it."
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--bare" {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

// fakeCall is what the fake records about one invocation.
type fakeCall struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
	// Prompt is what the fake read on stdin.
	Prompt string `json:"prompt"`
	// TokenFiles lists the files in the run dir that contain the GitHub
	// token in any of its tokenForms.
	TokenFiles []string `json:"tokenFiles"`
}

func fakeClaude() int {
	dir, err := os.Getwd()
	if err != nil {
		return fail("getwd: %v", err)
	}
	runDir := filepath.Dir(dir)
	workDir := filepath.Dir(runDir)
	mode, err := os.ReadFile(filepath.Join(workDir, fakeModeFile))
	if err != nil {
		return fail("read mode: %v", err)
	}
	if err := checkSettings(os.Args[1:], dir); err != nil {
		return fail("%v", err)
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("read stdin: %v", err)
	}
	call := fakeCall{Args: os.Args[1:], Env: os.Environ(), Dir: dir, Prompt: string(prompt), TokenFiles: filesContaining(runDir, tokenForms(githubToken))}
	if err := appendCall(filepath.Join(workDir, fakeCallsFile), call); err != nil {
		return fail("record call: %v", err)
	}

	emitInit(dir, os.Args[1:])
	switch m := strings.TrimSpace(string(mode)); m {
	case modeHappy:
		if err := commitFix(dir, "fix.txt"); err != nil {
			return fail("%v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeSummary})
	case modeNoop:
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Nothing to fix."})
	default:
		return fail("unknown mode %q", m)
	}
	return 0
}

// commitFix writes file in the clone dir and commits it on the current
// branch.
func commitFix(dir, file string) error {
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("fixed\n"), 0o644); err != nil {
		return fmt.Errorf("write fix: %v", err)
	}
	for _, args := range [][]string{{"add", file}, {"commit", "-q", "-m", "Fix blooopy"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
	}
	return nil
}

// emitInit emits the system/init event claude would for args.
func emitInit(dir string, args []string) {
	var plugins []map[string]string
	for i, a := range args {
		if a == "--plugin-dir" && i+1 < len(args) {
			plugins = append(plugins, map[string]string{"name": filepath.Base(args[i+1]), "path": args[i+1]})
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "cwd": dir, "permissionMode": "acceptEdits", "plugins": plugins})
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "fake claude: "+format+"\n", args...)
	return 3
}

func emit(event map[string]any) {
	line, _ := json.Marshal(event)
	fmt.Println(string(line))
}

// checkSettings asserts that the --settings JSON turns the sandbox on, lets
// sandboxed commands write only to clone and the child's HOME, and denies
// them reads of the work dir except for those two.
func checkSettings(args []string, clone string) error {
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) {
		return fmt.Errorf("no --settings in %q", args)
	}
	var s struct {
		Sandbox struct {
			Enabled                  bool `json:"enabled"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			Filesystem               struct {
				AllowWrite []string `json:"allowWrite"`
				DenyRead   []string `json:"denyRead"`
				AllowRead  []string `json:"allowRead"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &s); err != nil {
		return fmt.Errorf("decode settings: %v", err)
	}
	home := os.Getenv("HOME")
	claudeConfig := os.Getenv("CLAUDE_CONFIG_DIR")
	workDir := filepath.Dir(filepath.Dir(clone))
	fs := s.Sandbox.Filesystem
	if !s.Sandbox.Enabled || s.Sandbox.AllowUnsandboxedCommands || !slices.Equal(fs.AllowWrite, []string{clone, home}) {
		return fmt.Errorf("settings %s don't sandbox writes to %s and %s", args[i+1], clone, home)
	}
	if !slices.Contains(fs.DenyRead, workDir) || len(fs.AllowRead) < 3 || !slices.Equal(fs.AllowRead[:3], []string{clone, home, claudeConfig}) {
		return fmt.Errorf("settings %s don't limit reads in %s to %s, %s and %s", args[i+1], workDir, clone, home, claudeConfig)
	}
	return nil
}

// tokenForms returns the forms in which token could leak to disk: raw, as
// the base64 git's Authorization header carries, and as bare base64.
func tokenForms(token string) []string {
	return []string{
		token,
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)),
		base64.StdEncoding.EncodeToString([]byte(token)),
	}
}

// filesContaining lists the files under root that contain any of needles.
func filesContaining(root string, needles []string) []string {
	var found []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil && containsAny(data, needles) {
			found = append(found, path)
		}
		return nil
	})
	return found
}

func containsAny(data []byte, needles []string) bool {
	return slices.ContainsFunc(needles, func(n string) bool { return bytes.Contains(data, []byte(n)) })
}

func appendCall(path string, call fakeCall) error {
	line, err := json.Marshal(call)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// fakeCalls returns the calls the fake recorded in workDir.
func fakeCalls(t *testing.T, workDir string) []fakeCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, fakeCallsFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read fake calls: %v", err)
	}
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var c fakeCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("decode fake call %q: %v", line, err)
		}
		calls = append(calls, c)
	}
	return calls
}
