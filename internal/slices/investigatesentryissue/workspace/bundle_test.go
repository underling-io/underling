package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newTestMirror mirrors a fresh test remote, which also has a branch
// "other", into a private work dir.
func newTestMirror(t *testing.T) (*Workspace, string) {
	t.Helper()
	remote := testRemote(t)
	git(t, remote, "branch", "other", "main")
	ws, err := NewMirror(context.Background(), filepath.Join(t.TempDir(), "work"), remote, testBranch, os.Getenv("PATH"), testToken)
	if err != nil {
		t.Fatalf("NewMirror: %v", err)
	}
	t.Cleanup(func() { _ = ws.Remove() })
	return ws, remote
}

// packed packs ws's mirror and returns the bundle.
func packed(t *testing.T, ws *Workspace) string {
	t.Helper()
	bundle := filepath.Join(ws.RunDir, "input.bundle")
	if err := ws.Pack(context.Background(), bundle); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return bundle
}

// newTestWork clones bundle into a runner's run dir, on testBranch.
func newTestWork(t *testing.T, bundle string) *Work {
	t.Helper()
	w, err := NewWork(filepath.Join(t.TempDir(), "runner-work"), os.Getenv("PATH"))
	if err != nil {
		t.Fatalf("NewWork: %v", err)
	}
	t.Cleanup(func() { _ = w.Remove() })
	if err := w.CloneBundle(context.Background(), bundle, testBranch); err != nil {
		t.Fatalf("CloneBundle: %v", err)
	}
	return w
}

// returned bundles w's branches and has ws take them.
func returned(t *testing.T, ws *Workspace, w *Work) {
	t.Helper()
	bundle := filepath.Join(ws.RunDir, "output.bundle")
	if err := w.Bundle(context.Background(), bundle); err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if err := ws.Take(context.Background(), bundle); err != nil {
		t.Fatalf("Take: %v", err)
	}
}

func TestNewMirrorMakesNoCloneForClaude(t *testing.T) {
	ws, _ := newTestMirror(t)

	if ws.Clone != "" || ws.Home != "" || ws.ClaudeConfig != "" {
		t.Errorf("mirror-only workspace has Clone %q, Home %q, ClaudeConfig %q", ws.Clone, ws.Home, ws.ClaudeConfig)
	}
	entries, err := os.ReadDir(ws.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"git-home", "mirror.git"}; !slices.Equal(names, want) {
		t.Errorf("run dir holds %q, want %q", names, want)
	}
	if ws.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", ws.DefaultBranch)
	}
}

func TestInspectingBeforeAnythingWasTakenFails(t *testing.T) {
	ws, _ := newTestMirror(t)

	_, err := ws.StrayBranches(context.Background())

	if err == nil || !strings.Contains(err.Error(), "no bundle was taken") {
		t.Fatalf("got %v, want a no-bundle error", err)
	}
}

func TestPackBundlesOnlyTheDefaultBranchAndHEAD(t *testing.T) {
	ws, _ := newTestMirror(t)

	bundle := packed(t, ws)

	heads := strings.Fields(git(t, ws.RunDir, "bundle", "list-heads", bundle))
	var refs []string
	for i := 1; i < len(heads); i += 2 {
		refs = append(refs, heads[i])
	}
	if want := []string{"HEAD", "refs/heads/main"}; !slices.Equal(refs, want) {
		t.Fatalf("bundle holds %q, want %q", refs, want)
	}
}

func TestCloneBundleChecksOutTheBranchFromTheDefaultBranch(t *testing.T) {
	ws, _ := newTestMirror(t)

	w := newTestWork(t, packed(t, ws))

	if got := git(t, w.Clone, "rev-parse", "--abbrev-ref", "HEAD"); got != testBranch {
		t.Errorf("clone is on %s, want %s", got, testBranch)
	}
	if got, want := git(t, w.Clone, "rev-parse", "HEAD"), git(t, ws.Mirror, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("clone is at %s, want the default branch's %s", got, want)
	}
	if got := git(t, w.Clone, "branch", "--format=%(refname:short)"); got != "main\n"+testBranch {
		t.Errorf("clone has branches %q, want main and %s", got, testBranch)
	}
	if got := git(t, w.Clone, "config", "--local", "user.name") + " " + git(t, w.Clone, "config", "--local", "user.email") + " " + git(t, w.Clone, "config", "--local", "commit.gpgsign"); got != "underling underling@users.noreply.github.com false" {
		t.Errorf("clone's identity config is %q", got)
	}
	for _, dir := range []string{w.Clone, w.Home, w.ClaudeConfig} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || filepath.Dir(dir) != w.RunDir {
			t.Errorf("%s is not a dir in the run dir (error %v)", dir, err)
		}
	}
}

func TestCloneBundleRejectsABranchGitWould(t *testing.T) {
	ws, _ := newTestMirror(t)
	bundle := packed(t, ws)
	for _, branch := range []string{"-b", "a..b", "with space", "HEAD"} {
		w, err := NewWork(filepath.Join(t.TempDir(), "runner-work"), os.Getenv("PATH"))
		if err != nil {
			t.Fatalf("NewWork: %v", err)
		}
		t.Cleanup(func() { _ = w.Remove() })

		err = w.CloneBundle(context.Background(), bundle, branch)

		if err == nil || !strings.Contains(err.Error(), "is not a branch name") {
			t.Errorf("branch %q: got %v, want a branch name error", branch, err)
		}
		if _, statErr := os.Stat(w.Clone); statErr == nil {
			t.Errorf("branch %q: the bundle was cloned", branch)
		}
	}
}

func TestATakenCommitOnTheBranchIsCountedAndPushed(t *testing.T) {
	ws, remote := newTestMirror(t)
	w := newTestWork(t, packed(t, ws))
	commit(t, w.Clone, "fix.txt")
	returned(t, ws, w)

	stray, commits := inspect(t, ws)
	if len(stray) != 0 || commits != 1 {
		t.Fatalf("got stray %v and %d commits, want none and 1", stray, commits)
	}
	changed, err := ws.ChangedPaths(context.Background())
	if err != nil || !slices.Equal(changed, []string{"fix.txt"}) {
		t.Fatalf("ChangedPaths = %q, %v; want [fix.txt]", changed, err)
	}
	if err := ws.Push(context.Background()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	if got := git(t, remote, "log", "-1", "--format=%s", "refs/heads/"+testBranch); got != "Fix fix.txt" {
		t.Errorf("remote branch tip is %q, want the commit", got)
	}
}

func TestATakenBundleWithoutCommitsHasNone(t *testing.T) {
	ws, _ := newTestMirror(t)
	w := newTestWork(t, packed(t, ws))
	returned(t, ws, w)

	stray, commits := inspect(t, ws)

	if len(stray) != 0 || commits != 0 {
		t.Fatalf("got stray %v and %d commits, want none", stray, commits)
	}
}

func TestTakenCommitsOnAnotherBranchAreStray(t *testing.T) {
	ws, _ := newTestMirror(t)
	w := newTestWork(t, packed(t, ws))
	git(t, w.Clone, "checkout", "-q", "-b", "claude-own")
	commit(t, w.Clone, "fix.txt")
	git(t, w.Clone, "checkout", "-q", "main")
	commit(t, w.Clone, "on-main.txt")
	returned(t, ws, w)

	stray, _ := inspect(t, ws)

	if want := []string{"claude-own", "main"}; !slices.Equal(stray, want) {
		t.Fatalf("stray = %q, want %q", stray, want)
	}
}

func TestTakeFetchesNothingButBranches(t *testing.T) {
	ws, _ := newTestMirror(t)
	w := newTestWork(t, packed(t, ws))
	commit(t, w.Clone, "fix.txt")
	git(t, w.Clone, "tag", "v9")
	git(t, w.Clone, "update-ref", "refs/underling/clone/planted", "HEAD")
	bundle := filepath.Join(ws.RunDir, "output.bundle")
	git(t, w.Clone, "bundle", "create", "-q", bundle, "--all")
	before := git(t, ws.Mirror, "for-each-ref", "--format=%(refname)")

	if err := ws.Take(context.Background(), bundle); err != nil {
		t.Fatalf("Take: %v", err)
	}
	inspect(t, ws)

	got := strings.Fields(git(t, ws.Mirror, "for-each-ref", "--format=%(refname)"))
	want := append(strings.Fields(before), "refs/heads/"+testBranch, "refs/underling/clone/main", "refs/underling/clone/"+testBranch)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("mirror refs = %q, want %q", got, want)
	}
}

func TestTakeRejectsWhatGitCannotVerify(t *testing.T) {
	ws, _ := newTestMirror(t)
	w := newTestWork(t, packed(t, ws))
	commit(t, w.Clone, "one.txt")
	commit(t, w.Clone, "two.txt")
	// A bundle that needs a commit the mirror doesn't have.
	thin := filepath.Join(ws.RunDir, "thin.bundle")
	git(t, w.Clone, "bundle", "create", "-q", thin, "HEAD~1..HEAD")
	garbage := filepath.Join(ws.RunDir, "garbage.bundle")
	if err := os.WriteFile(garbage, []byte("not a bundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, bundle := range map[string]string{"garbage": garbage, "missing prerequisite": thin, "missing file": filepath.Join(ws.RunDir, "none.bundle")} {
		err := ws.Take(context.Background(), bundle)

		if err == nil || !strings.Contains(err.Error(), "git bundle") {
			t.Errorf("%s: got %v, want a git bundle error", name, err)
		}
		if _, err := ws.StrayBranches(context.Background()); err == nil {
			t.Errorf("%s: the rejected bundle became the source", name)
		}
	}
}

func TestBundleFailsWhenTheCloneHasNoBranch(t *testing.T) {
	ws, _ := newTestMirror(t)
	w := newTestWork(t, packed(t, ws))
	git(t, w.Clone, "checkout", "-q", "--detach")
	git(t, w.Clone, "branch", "-q", "-D", "main", testBranch)

	err := w.Bundle(context.Background(), filepath.Join(w.RunDir, "out.bundle"))

	if err == nil || !strings.Contains(err.Error(), "git bundle") {
		t.Fatalf("got %v, want a git bundle error", err)
	}
}
