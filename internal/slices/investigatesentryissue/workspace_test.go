package investigatesentryissue

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testRemote creates a bare repo whose main branch has one commit and
// returns its path.
func testRemote(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "web.git")
	seed := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(seed, "init", "-q", "--bare", "-b", "main", remote)
	run(seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "README.md")
	run(seed, "commit", "-q", "-m", "Initial commit")
	run(seed, "push", "-q", remote, "main")
	return remote
}

// TokenForms returns the forms in which token could leak to disk: raw, as
// the base64 git's Authorization header carries, and as bare base64. It is
// exported for the external tests.
func TokenForms(token string) []string {
	return []string{
		token,
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)),
		base64.StdEncoding.EncodeToString([]byte(token)),
	}
}

func TestWorkspaceCloneSharesNoObjectFilesWithTheMirror(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work") // created private
	ws, err := newWorkspace(context.Background(), workDir, testRemote(t), "heinzel/sentry-1", config{path: os.Getenv("PATH")})
	if err != nil {
		t.Fatalf("newWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.remove() })

	objects := filepath.Join(ws.mirror, "objects")
	compared := 0
	_ = filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(objects, path)
		mirrorInfo, err1 := os.Stat(path)
		cloneInfo, err2 := os.Stat(filepath.Join(ws.clone, ".git", "objects", rel))
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

func TestTheTokenIsInNeitherGitConfigAfterAPush(t *testing.T) {
	const token = "ghp-test-token-4f2a9c"
	ctx := context.Background()
	remote := testRemote(t)
	ws, err := newWorkspace(ctx, filepath.Join(t.TempDir(), "work"), remote, "heinzel/sentry-1", config{path: os.Getenv("PATH"), githubToken: token})
	if err != nil {
		t.Fatalf("newWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.remove() })
	if err := os.WriteFile(filepath.Join(ws.clone, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "fix.txt"}, {"commit", "-q", "-m", "Fix"}} {
		if _, err := ws.git.run(ctx, ws.clone, false, args...); err != nil {
			t.Fatal(err)
		}
	}
	if stray, err := ws.strayBranches(ctx); err != nil || len(stray) != 0 {
		t.Fatalf("strayBranches: %v, %v", stray, err)
	}
	if n, err := ws.newCommits(ctx); err != nil || n != 1 {
		t.Fatalf("newCommits: %d, %v", n, err)
	}
	if err := ws.push(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}

	for _, file := range []string{filepath.Join(ws.clone, ".git", "config"), filepath.Join(ws.mirror, "config")} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, form := range TokenForms(token) {
			if strings.Contains(string(data), form) {
				t.Errorf("%s holds the token as %q:\n%s", file, form, data)
			}
		}
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
		{[]string{"-c", "x=y", "config", "user.name", "heinzel"}, time.Minute},
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

func TestCIConfigPath(t *testing.T) {
	tests := map[string]bool{
		".github/workflows/ci.yml":   true,
		".github/dependabot.yml":     true,
		".github/actions/x/a.yml":    true,
		".gitlab-ci.yml":             true,
		".circleci/config.yml":       true,
		".buildkite/pipeline.yml":    true,
		"Jenkinsfile":                true,
		"azure-pipelines.yml":        true,
		".travis.yml":                true,
		"bitbucket-pipelines.yml":    true,
		"src/app.js":                 false,
		"docs/.github/notes.md":      false,
		".githubx/file":              false,
		"README.md":                  false,
		"scripts/azure-pipelines.md": false,
	}
	for path, want := range tests {
		if got := ciConfigPath(path); got != want {
			t.Errorf("ciConfigPath(%q) = %v, want %v", path, got, want)
		}
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
