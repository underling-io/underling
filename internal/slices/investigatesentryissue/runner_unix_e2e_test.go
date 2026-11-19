//go:build unix

package investigatesentryissue_test

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestAClaudeTimeoutInTheRunnerFailsTheInvestigationAndKillsItsProcesses(t *testing.T) {
	h, l := newRunnerHarness(t, modeHang, withEnv("INVESTIGATION_TIMEOUT", "3s"))

	h.handler.Handle(alert(5, 1, now))

	assertFailed(t, h, "timeout", "claude did not finish within 3s")
	l.assertRan()
	assertNothingLeftGitHub(t, h)
	h.assertNoLeftovers()

	data, err := os.ReadFile(filepath.Join(l.workDir, fakeDescendantFile))
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("claude's descendant %d outlived the run", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
