//go:build unix

package investigatesentryissue_test

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"heinzel/internal/events"
)

func TestALeftoverGrandchildNeitherDelaysNorOutlivesTheRun(t *testing.T) {
	h := newHarness(t, modeGrandchild)

	start := time.Now()
	h.handler.Handle(alert(5, 1, now))
	elapsed := time.Since(start)

	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationNoChangesType), nil)
	if elapsed > 5*time.Second {
		t.Errorf("Handle took %v; the grandchild's open stdout held it up", elapsed)
	}
	data, err := os.ReadFile(filepath.Join(h.workDir, fakeGrandchildFile))
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return syscall.Kill(pid, 0) != nil })
}
