//go:build unix

package claude

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestALeftoverGrandchildNeitherDelaysNorOutlivesTheRun(t *testing.T) {
	cmd := fakeCmd(t, modeGrandchild)

	start := time.Now()
	summary, err := Run(context.Background(), cmd, time.Minute)
	elapsed := time.Since(start)

	if err != nil || summary != "Nothing to fix." {
		t.Fatalf("got (%q, %v), want the fake's summary", summary, err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Run took %v; the grandchild's open stdout held it up", elapsed)
	}
	data, err := os.ReadFile(filepath.Join(cmd.dir, fakeGrandchildFile))
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitLimit)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d outlived the run", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
