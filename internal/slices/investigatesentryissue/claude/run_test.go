package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake claude: Run starts it with
// fakeModeVar set in its env, and it then acts out that mode instead of
// running the tests.
const (
	fakeModeVar = "FAKE_CLAUDE_MODE"

	modeSuccess = "success" // report success, with the prompt in the summary
	modeError   = "error"   // report an error result and exit 1
	modeHang    = "hang"    // write fakeStartedFile, then block until killed
	// modeGrandchild starts a background process that holds stdout open,
	// records its pid in fakeGrandchildFile, reports success and exits.
	modeGrandchild = "grandchild"
	// Every mode emits a system/init event matching its args, except that
	// these modes distort it, then report success.
	modeInitMissing        = "init-missing"         // no init event
	modeInitPermissionMode = "init-permission-mode" // another permission mode
	modeInitPluginMissing  = "init-plugin-missing"  // only the first plugin loaded
	modeInitPluginErrors   = "init-plugin-errors"   // a plugin error

	fakeStartedFile    = "fake-claude-started"
	fakeGrandchildFile = "fake-claude-grandchild"

	waitLimit = 10 * time.Second
)

var testPluginDirs = []string{"/plugins/codex", "/plugins/auto-task"}

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeModeVar); mode != "" {
		os.Exit(fakeClaude(mode))
	}
	os.Exit(m.Run())
}

func fakeClaude(mode string) int {
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("read stdin: %v", err)
	}
	emitInit(mode, os.Args[1:])
	switch mode {
	case modeSuccess:
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Summary of: " + string(prompt)})
	case modeInitMissing, modeInitPermissionMode, modeInitPluginMissing, modeInitPluginErrors:
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Nothing to fix."})
	case modeError:
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "model refused"})
		fmt.Fprintln(os.Stderr, "fake claude failed")
		return 1
	case modeHang:
		if err := os.WriteFile(fakeStartedFile, nil, 0o644); err != nil {
			return fail("record start: %v", err)
		}
		for {
			time.Sleep(time.Hour)
		}
	case modeGrandchild:
		// Leave a process behind that holds stdout and stderr open.
		sleeper := exec.Command("sleep", "60")
		sleeper.Stdout, sleeper.Stderr = os.Stdout, os.Stderr
		if err := sleeper.Start(); err != nil {
			return fail("start grandchild: %v", err)
		}
		if err := os.WriteFile(fakeGrandchildFile, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
			return fail("record grandchild: %v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Nothing to fix."})
	default:
		return fail("unknown mode %q", mode)
	}
	return 0
}

// emitInit emits the system/init event claude would for args, as the init
// failure modes distort it.
func emitInit(mode string, args []string) {
	if mode == modeInitMissing {
		return
	}
	var plugins []map[string]string
	for i, a := range args {
		if a == "--plugin-dir" && i+1 < len(args) {
			plugins = append(plugins, map[string]string{"name": filepath.Base(args[i+1]), "path": args[i+1]})
		}
	}
	event := map[string]any{"type": "system", "subtype": "init", "permissionMode": "acceptEdits", "plugins": plugins}
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

// fakeCmd returns a command that runs the fake claude in mode, in a fresh
// dir, with the prompt on stdin.
func fakeCmd(t *testing.T, mode string) Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--bare", "-p"}
	for _, dir := range testPluginDirs {
		args = append(args, "--plugin-dir", dir)
	}
	return Cmd{
		bin:        self,
		args:       args,
		env:        []string{fakeModeVar + "=" + mode, "PATH=" + os.Getenv("PATH")},
		dir:        t.TempDir(),
		stdin:      "the prompt",
		pluginDirs: testPluginDirs,

		permissionMode: "acceptEdits",
	}
}

// runWithin runs Run in the background and fails the test if it doesn't
// return within waitLimit.
func runWithin(t *testing.T, ctx context.Context, cmd Cmd, timeout time.Duration) (string, error) {
	t.Helper()
	type result struct {
		summary string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := Run(ctx, cmd, timeout)
		done <- result{summary, err}
	}()
	select {
	case r := <-done:
		return r.summary, r.err
	case <-time.After(waitLimit):
		t.Fatal("Run did not return in time")
		return "", nil
	}
}

func TestRunHandsThePromptToClaudeAndReturnsItsSummary(t *testing.T) {
	summary, err := Run(context.Background(), fakeCmd(t, modeSuccess), time.Minute)

	if err != nil || summary != "Summary of: the prompt" {
		t.Fatalf("got (%q, %v), want the summary of the prompt", summary, err)
	}
}

func TestRunFailsWithClaudesResultAndStderrWhenClaudeFails(t *testing.T) {
	_, err := Run(context.Background(), fakeCmd(t, modeError), time.Minute)

	if err == nil || !strings.Contains(err.Error(), "model refused") || !strings.Contains(err.Error(), "fake claude failed") {
		t.Fatalf("got %v, want an error with claude's result and stderr", err)
	}
}

func TestRunFailsWhenClaudeDoesNotStart(t *testing.T) {
	cmd := fakeCmd(t, modeSuccess)
	cmd.bin = filepath.Join(t.TempDir(), "no-claude")

	_, err := Run(context.Background(), cmd, time.Minute)

	if err == nil || !strings.HasPrefix(err.Error(), "start claude: ") {
		t.Fatalf("got %v, want a start error", err)
	}
}

func TestRunFailsWhenTheSessionDidNotStartAsConfigured(t *testing.T) {
	tests := []struct {
		mode, reason string
	}{
		{modeInitMissing, "no system/init event"},
		{modeInitPermissionMode, `permission mode "default"`},
		{modeInitPluginMissing, "no plugin loaded from /plugins/auto-task"},
		{modeInitPluginErrors, "hooks.json is invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			_, err := Run(context.Background(), fakeCmd(t, tt.mode), time.Minute)

			if err == nil || !strings.Contains(err.Error(), "claude's session did not start as configured: ") || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("got %v, want a session error containing %q", err, tt.reason)
			}
		})
	}
}

func TestRunKillsClaudeAfterTheTimeout(t *testing.T) {
	_, err := runWithin(t, context.Background(), fakeCmd(t, modeHang), 500*time.Millisecond)

	var timeout *TimeoutError
	if !errors.As(err, &timeout) || err.Error() != "claude did not finish within 500ms" {
		t.Fatalf("got %v, want a timeout error", err)
	}
	// The investigator spots a timeout by this method, not by the type.
	var withTimeout interface{ Timeout() bool }
	if !errors.As(err, &withTimeout) || !withTimeout.Timeout() {
		t.Fatalf("got %v, want an error whose Timeout() reports true", err)
	}
}

func TestRunKillsClaudeOnShutdown(t *testing.T) {
	cmd := fakeCmd(t, modeHang)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Shut down once the fake is running.
		for {
			if _, err := os.Stat(filepath.Join(cmd.dir, fakeStartedFile)); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	_, err := runWithin(t, ctx, cmd, time.Hour)

	var timeout *TimeoutError
	if err == nil || err.Error() != "underling shut down while claude was running" || errors.As(err, &timeout) {
		t.Fatalf("got %v, want a shutdown error", err)
	}
}
