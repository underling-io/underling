package investigatesentryissue_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"heinzel/internal/slices/investigatesentryissue"
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
	modeError = "error" // report an error result and exit 1
	modeHang  = "hang"  // block until killed
	// modeHostileGitConfig plants a .gitconfig in the child's HOME that
	// redirects the GitHub remote, then commits like modeHappy.
	modeHostileGitConfig = "hostile-gitconfig"
	// modeOtherBranch commits on a branch of its own, as auto-task would
	// in its own worktree.
	modeOtherBranch = "other-branch"
	otherBranch     = "claude-own"
	// modeRenameBranch renames the investigation branch, then commits.
	modeRenameBranch = "rename-branch"
	renamedBranch    = "claude-renamed"
	// modeDeleteBranch checks out the default branch, deletes the
	// investigation branch and reports success without committing.
	modeDeleteBranch = "delete-branch"
	// modeHelperBranch commits on a helper branch, then fast-forwards the
	// investigation branch to it and leaves the helper branch behind.
	modeHelperBranch = "helper-branch"
	// modeCIConfig commits a GitHub Actions workflow.
	modeCIConfig = "ci-config"
	// modeGrandchild starts a background process that holds stdout open,
	// records its pid in fakeGrandchildFile, reports success without
	// committing and exits.
	modeGrandchild     = "grandchild"
	fakeGrandchildFile = "fake-claude-grandchild"
	// Every mode emits a system/init event matching its args, except that
	// these modes distort it, then report success without committing.
	modeInitMissing        = "init-missing"         // no init event
	modeInitPermissionMode = "init-permission-mode" // another permission mode
	modeInitPluginMissing  = "init-plugin-missing"  // only the first plugin loaded
	modeInitPluginErrors   = "init-plugin-errors"   // a plugin error

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
	// token in any of its TokenForms.
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
	call := fakeCall{Args: os.Args[1:], Env: os.Environ(), Dir: dir, Prompt: string(prompt), TokenFiles: filesContaining(runDir, investigatesentryissue.TokenForms(githubToken))}
	if err := appendCall(filepath.Join(workDir, fakeCallsFile), call); err != nil {
		return fail("record call: %v", err)
	}

	m := strings.TrimSpace(string(mode))
	emitInit(m, dir, os.Args[1:])
	switch m {
	case modeHappy:
		if err := commitFix(dir, "fix.txt"); err != nil {
			return fail("%v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeSummary})
	case modeInitMissing, modeInitPermissionMode, modeInitPluginMissing, modeInitPluginErrors:
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Nothing to fix."})
	case modeHostileGitConfig:
		// Redirect the GitHub remote through the child's HOME, which
		// heinzel's own git must not read.
		out, err := exec.Command("git", "config", "--file", filepath.Join(runDir, "mirror.git", "config"), "remote.origin.url").Output()
		if err != nil {
			return fail("read remote url: %v", err)
		}
		hostile := fmt.Sprintf("[url %q]\n\tinsteadOf = %s\n", filepath.Join(runDir, "nowhere.git"), strings.TrimSpace(string(out)))
		if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".gitconfig"), []byte(hostile), 0o644); err != nil {
			return fail("write gitconfig: %v", err)
		}
		if err := commitFix(dir, "fix.txt"); err != nil {
			return fail("%v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeSummary})
	case modeGrandchild:
		// Leave a process behind that holds stdout and stderr open.
		sleeper := exec.Command("sleep", "60")
		sleeper.Stdout, sleeper.Stderr = os.Stdout, os.Stderr
		if err := sleeper.Start(); err != nil {
			return fail("start grandchild: %v", err)
		}
		if err := os.WriteFile(filepath.Join(workDir, fakeGrandchildFile), []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
			return fail("record grandchild: %v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Nothing to fix."})
	case modeCIConfig:
		if err := commitFix(dir, ".github/workflows/exfiltrate.yml"); err != nil {
			return fail("%v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeSummary})
	case modeOtherBranch:
		cmd := exec.Command("git", "checkout", "-q", "-b", otherBranch)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fail("git checkout: %v: %s", err, out)
		}
		if err := commitFix(dir, "fix.txt"); err != nil {
			return fail("%v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeSummary})
	case modeRenameBranch:
		if _, err := gitOutput(dir, "branch", "-m", renamedBranch); err != nil {
			return fail("%v", err)
		}
		if err := commitFix(dir, "fix.txt"); err != nil {
			return fail("%v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeSummary})
	case modeDeleteBranch:
		investigation, err := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return fail("%v", err)
		}
		for _, args := range [][]string{{"checkout", "-q", "main"}, {"branch", "-q", "-D", investigation}} {
			if _, err := gitOutput(dir, args...); err != nil {
				return fail("%v", err)
			}
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Nothing to fix."})
	case modeHelperBranch:
		investigation, err := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return fail("%v", err)
		}
		if _, err := gitOutput(dir, "checkout", "-q", "-b", otherBranch); err != nil {
			return fail("%v", err)
		}
		if err := commitFix(dir, "fix.txt"); err != nil {
			return fail("%v", err)
		}
		for _, args := range [][]string{{"checkout", "-q", investigation}, {"merge", "-q", "--ff-only", otherBranch}} {
			if _, err := gitOutput(dir, args...); err != nil {
				return fail("%v", err)
			}
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fakeSummary})
	case modeNoop:
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Nothing to fix."})
	case modeError:
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "model refused"})
		fmt.Fprintln(os.Stderr, "fake claude failed")
		return 1
	case modeHang:
		for {
			time.Sleep(time.Hour)
		}
	default:
		return fail("unknown mode %q", mode)
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

// gitOutput runs git in dir and returns its trimmed output.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// emitInit emits the system/init event claude would for args, as the init
// failure modes distort it.
func emitInit(mode, dir string, args []string) {
	if mode == modeInitMissing {
		return
	}
	var plugins []map[string]string
	for i, a := range args {
		if a == "--plugin-dir" && i+1 < len(args) {
			plugins = append(plugins, map[string]string{"name": filepath.Base(args[i+1]), "path": args[i+1]})
		}
	}
	event := map[string]any{"type": "system", "subtype": "init", "cwd": dir, "permissionMode": "acceptEdits", "plugins": plugins}
	switch mode {
	case modeInitPermissionMode:
		event["permissionMode"] = "default"
	case modeInitPluginMissing:
		event["plugins"] = plugins[:1]
	case modeInitPluginErrors:
		event["plugin_errors"] = []map[string]string{{"plugin": "auto-task", "type": "hooks", "message": "hooks.json is invalid"}}
	}
	emit(event)
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
