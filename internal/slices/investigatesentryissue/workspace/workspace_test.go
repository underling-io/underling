package workspace

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testToken  = "ghp-test-token-4f2a9c"
	testBranch = "underling/sentry-1"
)

// git runs git in dir and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testRemote creates a bare repo whose main branch has one commit and
// returns its path.
func testRemote(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "web.git")
	seed := t.TempDir()
	git(t, seed, "init", "-q", "--bare", "-b", "main", remote)
	git(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "commit", "-q", "-m", "Initial commit")
	git(t, seed, "push", "-q", remote, "main")
	return remote
}

// newTestWorkspace clones a fresh test remote into a private work dir and
// returns the workspace and the remote.
func newTestWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	remote := testRemote(t)
	ws, err := New(context.Background(), filepath.Join(t.TempDir(), "work"), remote, testBranch, os.Getenv("PATH"), testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ws.Remove() })
	return ws, remote
}

// commit writes file in dir and commits it on the current branch, as
// Claude would.
func commit(t *testing.T, dir, file string) {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", file)
	git(t, dir, "commit", "-q", "-m", "Fix "+file)
}

// inspect runs StrayBranches and NewCommits, in the order the
// investigation does.
func inspect(t *testing.T, ws *Workspace) (stray []string, commits int) {
	t.Helper()
	ctx := context.Background()
	stray, err := ws.StrayBranches(ctx)
	if err != nil {
		t.Fatalf("StrayBranches: %v", err)
	}
	commits, err = ws.NewCommits(ctx)
	if err != nil {
		t.Fatalf("NewCommits: %v", err)
	}
	return stray, commits
}

func remoteHasBranch(remote string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "-q", "refs/heads/"+testBranch)
	cmd.Dir = remote
	return cmd.Run() == nil
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

func TestNewChecksOutTheBranchFromTheDefaultBranch(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	if got := git(t, ws.Clone, "rev-parse", "--abbrev-ref", "HEAD"); got != testBranch {
		t.Errorf("clone is on %s, want %s", got, testBranch)
	}
	if ws.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", ws.DefaultBranch)
	}
	for _, dir := range []string{ws.Home, ws.ClaudeConfig, ws.GitHome} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || filepath.Dir(dir) != ws.RunDir {
			t.Errorf("%s is not a dir in the run dir (error %v)", dir, err)
		}
	}
}

func TestNewFailsAndCleansUpWhenTheCloneFails(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")

	_, err := New(context.Background(), workDir, filepath.Join(t.TempDir(), "missing.git"), testBranch, os.Getenv("PATH"), testToken)

	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Fatalf("got %v, want a git clone error", err)
	}
	if entries, err := os.ReadDir(workDir); err != nil || len(entries) != 0 {
		t.Errorf("work dir holds %v (error %v), want nothing", entries, err)
	}
}

func TestNewListsTheWorkDirsOtherEntriesAsSiblings(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(filepath.Join(workDir, "old-run"), 0o700); err != nil {
		t.Fatal(err)
	}

	ws, err := New(context.Background(), workDir, testRemote(t), testBranch, os.Getenv("PATH"), testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ws.Remove() })

	if want := []string{filepath.Join(ws.WorkDir, "old-run")}; !slices.Equal(ws.Siblings, want) {
		t.Errorf("Siblings = %q, want %q", ws.Siblings, want)
	}
}

func TestRemoveDeletesTheRunDir(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	if err := ws.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if entries, err := os.ReadDir(ws.WorkDir); err != nil || len(entries) != 0 {
		t.Errorf("work dir holds %v (error %v), want nothing", entries, err)
	}
}

func TestWorkspaceCloneSharesNoObjectFilesWithTheMirror(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	objects := filepath.Join(ws.Mirror, "objects")
	compared := 0
	_ = filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(objects, path)
		mirrorInfo, err1 := os.Stat(path)
		cloneInfo, err2 := os.Stat(filepath.Join(ws.Clone, ".git", "objects", rel))
		if err1 == nil && err2 == nil {
			compared++
			if os.SameFile(mirrorInfo, cloneInfo) {
				t.Errorf("object %s is hard-linked between the mirror and the clone", rel)
			}
		}
		return nil
	})
	if compared == 0 {
		t.Fatal("found no object files to compare")
	}
}

func TestACommitOnTheBranchIsCountedAndPushed(t *testing.T) {
	ws, remote := newTestWorkspace(t)
	commit(t, ws.Clone, "fix.txt")

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

func TestNoCommitsCountAsNone(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	stray, commits := inspect(t, ws)

	if len(stray) != 0 || commits != 0 {
		t.Fatalf("got stray %v and %d commits, want none", stray, commits)
	}
}

func TestChangedPathsListARenameAsADeletionAndAnAddition(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	commit(t, ws.Clone, ".github/workflows/ci.yml")
	git(t, ws.Clone, "mv", "README.md", "MOVED.md")
	git(t, ws.Clone, "commit", "-q", "-m", "Move")
	inspect(t, ws)

	changed, err := ws.ChangedPaths(context.Background())

	if want := []string{".github/workflows/ci.yml", "MOVED.md", "README.md"}; err != nil || !slices.Equal(changed, want) {
		t.Fatalf("ChangedPaths = %q, %v; want %q", changed, err, want)
	}
}

func TestCommitsOnAnotherBranchAreStray(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	git(t, ws.Clone, "checkout", "-q", "-b", "claude-own")
	commit(t, ws.Clone, "fix.txt")

	stray, _ := inspect(t, ws)

	if want := []string{"claude-own"}; !slices.Equal(stray, want) {
		t.Fatalf("stray = %q, want %q", stray, want)
	}
}

func TestCommitsOnARenamedBranchAreStray(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	git(t, ws.Clone, "branch", "-m", "claude-renamed")
	commit(t, ws.Clone, "fix.txt")

	stray, _ := inspect(t, ws)

	if want := []string{"claude-renamed"}; !slices.Equal(stray, want) {
		t.Fatalf("stray = %q, want %q", stray, want)
	}
}

func TestADeletedBranchWithoutCommitsHasNone(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	git(t, ws.Clone, "checkout", "-q", "main")
	git(t, ws.Clone, "branch", "-q", "-D", testBranch)

	stray, commits := inspect(t, ws)

	if len(stray) != 0 || commits != 0 {
		t.Fatalf("got stray %v and %d commits, want none", stray, commits)
	}
}

func TestAHelperBranchMergedIntoTheBranchIsNotStray(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	git(t, ws.Clone, "checkout", "-q", "-b", "claude-own")
	commit(t, ws.Clone, "fix.txt")
	git(t, ws.Clone, "checkout", "-q", testBranch)
	git(t, ws.Clone, "merge", "-q", "--ff-only", "claude-own")

	stray, commits := inspect(t, ws)

	if len(stray) != 0 || commits != 1 {
		t.Fatalf("got stray %v and %d commits, want none and 1", stray, commits)
	}
}

func TestTheChildsGitConfigDoesNotAffectThePush(t *testing.T) {
	ws, remote := newTestWorkspace(t)
	// Redirect the GitHub remote through the child's HOME, which underling's
	// own git must not read.
	hostile := "[url \"" + filepath.Join(ws.RunDir, "nowhere.git") + "\"]\n\tinsteadOf = " + remote + "\n"
	if err := os.WriteFile(filepath.Join(ws.Home, ".gitconfig"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, ws.Clone, "fix.txt")
	inspect(t, ws)

	if err := ws.Push(context.Background()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !remoteHasBranch(remote) {
		t.Errorf("the push did not reach the remote")
	}
}

func TestPushFailsWhenTheRemoteRejectsIt(t *testing.T) {
	ws, remote := newTestWorkspace(t)
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho rejected by test >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	commit(t, ws.Clone, "fix.txt")
	inspect(t, ws)

	err := ws.Push(context.Background())

	if err == nil || !strings.Contains(err.Error(), "rejected by test") {
		t.Fatalf("got %v, want the remote's rejection", err)
	}
}

func TestTheTokenIsInNeitherGitConfigAfterAPush(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	commit(t, ws.Clone, "fix.txt")
	inspect(t, ws)
	if err := ws.Push(context.Background()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	for _, file := range []string{filepath.Join(ws.Clone, ".git", "config"), filepath.Join(ws.Mirror, "config")} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, form := range tokenForms(testToken) {
			if strings.Contains(string(data), form) {
				t.Errorf("%s holds the token as %q:\n%s", file, form, data)
			}
		}
	}
}

func TestGitErrorsRedactTheToken(t *testing.T) {
	g := newGitRunner(os.Getenv("PATH"), t.TempDir(), testToken)

	_, err := g.run(context.Background(), t.TempDir(), false, "clone", "--quiet", filepath.Join(t.TempDir(), testToken+".git"), "x")

	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("got %v, want an error with the token redacted", err)
	}
}

func TestGitTimeouts(t *testing.T) {
	g := newGitRunner("/usr/bin", "/w/run/git-home", "token")
	tests := []struct {
		args []string
		want time.Duration
	}{
		{[]string{"clone", "--quiet", "--bare", "https://github.com/acme/web.git", "/w/run/mirror.git"}, 10 * time.Minute},
		{[]string{"fetch", "--quiet", "/w/run/repo", "+refs/heads/x:refs/heads/x"}, 10 * time.Minute},
		{[]string{"-c", "core.hooksPath=/dev/null", "push", "--quiet", "origin", "x"}, 10 * time.Minute},
		{[]string{"rev-list", "--count", "a..b"}, time.Minute},
		{[]string{"-c", "x=y", "config", "user.name", "underling"}, time.Minute},
	}
	for _, tt := range tests {
		if got := g.timeout(tt.args); got != tt.want {
			t.Errorf("timeout(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestGitRunnerKillsCommandsAfterTheirTimeout(t *testing.T) {
	g := newGitRunner(os.Getenv("PATH"), t.TempDir(), "token")
	g.localTimeout = time.Nanosecond

	_, err := g.run(context.Background(), t.TempDir(), false, "version")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a deadline error", err)
	}
}

func TestGitNetworkCommandsAbortSlowTransfers(t *testing.T) {
	g := newGitRunner("/usr/bin", "/w/run/git-home", "token")

	env := strings.Join(g.env(true), "\n")
	local := strings.Join(g.env(false), "\n")

	for _, want := range []string{"=http.lowSpeedLimit\n", "=1000\n", "=http.lowSpeedTime\n", "=60"} {
		if !strings.Contains(env+"\n", want) {
			t.Errorf("network env lacks %q:\n%s", want, env)
		}
	}
	for _, e := range []string{env, local} {
		if !strings.Contains(e, "GIT_CONFIG_NOSYSTEM=1") || !strings.Contains(e, "GIT_CONFIG_GLOBAL="+os.DevNull) || !strings.Contains(e, "HOME=/w/run/git-home") {
			t.Errorf("git env reads system or global config:\n%s", e)
		}
	}
	if !strings.Contains(env, "Authorization: Basic ") || strings.Contains(local, "Authorization") {
		t.Errorf("only network commands get the token:\nnetwork:\n%s\nlocal:\n%s", env, local)
	}
}

func TestMakeRemovableSetsDirsToOwnerOnlyAndFollowsNoSymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	open := filepath.Join(root, "repo", "open")
	if err := os.MkdirAll(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o755) })
	if err := os.Symlink(outside, filepath.Join(root, "repo", "link")); err != nil {
		t.Fatal(err)
	}

	makeRemovable(root)

	for _, dir := range []string{root, filepath.Join(root, "repo"), open} {
		if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v (error %v), want 0700", dir, info.Mode().Perm(), err)
		}
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o555 {
		t.Errorf("the symlink's target changed: mode %v (error %v), want 0555", info.Mode().Perm(), err)
	}
}

func TestRemoveTreeRemovesReadOnlyDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	locked := filepath.Join(root, "repo", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}

	if err := removeTree(root); err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("%s still exists (stat error %v)", root, err)
	}
}
