//go:build unix

package investigatesentryissue_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"underling/internal/slices/investigatesentryissue"
)

func TestNewRejectsAnExistingWorkDirOthersCanUse(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workDir, 0o755); err != nil { // whatever the umask
		t.Fatal(err)
	}
	env := map[string]string{
		"SENTRY_GITHUB_REPOS":    "1=acme/web",
		"GITHUB_TOKEN":           githubToken,
		"ANTHROPIC_API_KEY":      "anthropic-test-key",
		"OPENAI_API_KEY":         "openai-test-key",
		"CLAUDE_PLUGIN_DIRS":     "/plugins/auto-task",
		"CLAUDE_BIN":             self,
		"INVESTIGATION_WORK_DIR": workDir,
		"PATH":                   os.Getenv("PATH"),
	}

	_, err = investigatesentryissue.New(investigatesentryissue.Params{
		Getenv: func(key string) string { return env[key] },
		Logger: slog.New(slog.DiscardHandler),
	})

	if err == nil || !strings.Contains(err.Error(), "INVESTIGATION_WORK_DIR") || !strings.Contains(err.Error(), "0755") {
		t.Fatalf("got %v, want an error naming INVESTIGATION_WORK_DIR and the mode", err)
	}
}

func TestAWorkDirOthersCanUseFailsTheRun(t *testing.T) {
	h := newHarness(t, modeHappy)
	if err := os.Chmod(h.workDir, 0o777); err != nil {
		t.Fatal(err)
	}

	h.handler.Handle(alert(5, 1, now))

	assertFailed(t, h, "clone", "work dir")
	if calls := fakeCalls(t, h.workDir); len(calls) != 0 {
		t.Errorf("claude ran %d times, want 0", len(calls))
	}
}
