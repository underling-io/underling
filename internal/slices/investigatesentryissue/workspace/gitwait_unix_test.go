//go:build unix

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGitRunnerWaitsOnlyBrieflyForPipesAChildStillHolds(t *testing.T) {
	// A fake git that leaves a child holding its output pipe, as a
	// lingering git-remote-https would, then hangs.
	bin := t.TempDir()
	script := "#!/bin/sh\n(sleep 10; echo late) &\nexec sleep 10\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := bin + string(filepath.ListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	g := newGitRunner(path, t.TempDir(), "token")
	if g.waitDelay != 10*time.Second {
		t.Errorf("waitDelay = %v, want 10s", g.waitDelay)
	}
	// Long enough for the script to start and fork.
	g.localTimeout, g.waitDelay = time.Second, 100*time.Millisecond

	start := time.Now()
	_, err := g.run(context.Background(), t.TempDir(), false, "version")

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("run took %v after its timeout, want it to stop waiting for the pipes", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want a deadline error", err)
	}
}
