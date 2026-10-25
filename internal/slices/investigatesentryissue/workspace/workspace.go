// Package workspace holds the git side of an investigation run: a run dir
// with a mirror of the GitHub repo and a clone for Claude to work in, and
// the git commands that inspect Claude's commits and push them.
package workspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	gitUserName  = "heinzel"
	gitUserEmail = "heinzel@users.noreply.github.com"
	gitOutputMax = 2 << 10

	gitNetworkTimeout = 10 * time.Minute
	gitLocalTimeout   = time.Minute
	gitWaitDelay      = 10 * time.Second
	gitLowSpeedLimit  = "1000" // bytes per second
	gitLowSpeedTime   = "60"   // seconds

	defaultDirName = "heinzel-investigations"
)

// Workspace is the per-run directory layout, all under one temp run dir:
//
//   - Mirror: a bare clone of the GitHub repo. Only heinzel uses it, with the
//     GitHub token, to clone from and to push from.
//   - Clone: a clone of the mirror on the investigation branch, where Claude
//     works. It never holds the token or a GitHub remote.
//   - Home and ClaudeConfig: the child's HOME and CLAUDE_CONFIG_DIR.
//   - GitHome: the HOME of heinzel's own git commands. The child never
//     sees it and can't write to it, so it can't plant git config (such as
//     url.<x>.insteadOf) that heinzel's push with the token would honour.
//
// Pushing from the mirror rather than the clone keeps Claude's edits to the
// clone's .git (hooks, config, remotes) away from the token: heinzel only
// fetches the branch's commits out of the clone.
type Workspace struct {
	WorkDir string // the work dir, symlinks resolved
	// Siblings are the other entries in the work dir when the run began,
	// such as leftovers of earlier runs.
	Siblings      []string
	RunDir        string
	Mirror        string
	Clone         string
	Home          string
	ClaudeConfig  string
	GitHome       string
	Branch        string
	DefaultBranch string
	base          string // the default branch's commit the branch starts from
	// branchGone is set by StrayBranches when the clone no longer has the
	// branch.
	branchGone bool
	git        gitRunner
}

// gitRunner runs git with a fixed environment, adding the GitHub token as an
// HTTP header only for the commands that talk to GitHub. Every command runs
// under a timeout: networkTimeout for clone, fetch and push, localTimeout
// for the rest.
type gitRunner struct {
	path           string
	home           string
	token          string
	networkTimeout time.Duration
	localTimeout   time.Duration
	// waitDelay bounds how long a killed command's output is still read,
	// e.g. while a lingering git-remote-https holds the pipes.
	waitDelay time.Duration
}

func newGitRunner(path, home, token string) gitRunner {
	return gitRunner{path: path, home: home, token: token, networkTimeout: gitNetworkTimeout, localTimeout: gitLocalTimeout, waitDelay: gitWaitDelay}
}

// timeout is how long the git command args may run.
func (g gitRunner) timeout(args []string) time.Duration {
	switch subcommand(args) {
	case "clone", "fetch", "push":
		return g.networkTimeout
	}
	return g.localTimeout
}

// subcommand returns the git subcommand in args, skipping global options.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

// New creates a run dir under workDir, mirrors cloneURL into it and checks
// out branch from the default branch in a clone of the mirror. Git runs
// with path as its PATH, and with token for the commands that talk to
// GitHub. On error it removes what it created.
func New(ctx context.Context, workDir, cloneURL, branch, path, token string) (_ *Workspace, err error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	if err := CheckDir(workDir); err != nil {
		return nil, err
	}
	created, err := os.MkdirTemp(workDir, "heinzel-investigation-*")
	if err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	defer func() {
		if err != nil {
			_ = removeTree(created)
		}
	}()
	// Resolve symlinks (e.g. macOS /var -> /private/var) so the sandbox's
	// write allowlist names the path the child actually sees.
	runDir, err := filepath.EvalSymlinks(created)
	if err != nil {
		return nil, err
	}
	ws := &Workspace{
		WorkDir:      filepath.Dir(runDir),
		RunDir:       runDir,
		Mirror:       filepath.Join(runDir, "mirror.git"),
		Clone:        filepath.Join(runDir, "repo"),
		Home:         filepath.Join(runDir, "home"),
		ClaudeConfig: filepath.Join(runDir, "claude-config"),
		GitHome:      filepath.Join(runDir, "git-home"),
		Branch:       branch,
	}
	entries, err := os.ReadDir(ws.WorkDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(runDir) {
			ws.Siblings = append(ws.Siblings, filepath.Join(ws.WorkDir, e.Name()))
		}
	}
	ws.git = newGitRunner(path, ws.GitHome, token)
	for _, dir := range []string{ws.Home, ws.ClaudeConfig, ws.GitHome} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
	}
	g := ws.git
	if _, err := g.run(ctx, runDir, true, "clone", "--quiet", "--bare", cloneURL, ws.Mirror); err != nil {
		return nil, err
	}
	if ws.DefaultBranch, err = g.run(ctx, ws.Mirror, false, "symbolic-ref", "--short", "HEAD"); err != nil {
		return nil, err
	}
	if ws.base, err = g.run(ctx, ws.Mirror, false, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return nil, err
	}
	// --no-hardlinks: Claude may chmod and rewrite the clone's object files,
	// which must not change the mirror's.
	if _, err := g.run(ctx, runDir, false, "clone", "--quiet", "--no-hardlinks", ws.Mirror, ws.Clone); err != nil {
		return nil, err
	}
	for _, args := range [][]string{
		{"config", "user.name", gitUserName},
		{"config", "user.email", gitUserEmail},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "--quiet", "-b", branch},
	} {
		if _, err := g.run(ctx, ws.Clone, false, args...); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// Remove deletes the run dir.
func (ws *Workspace) Remove() error {
	return removeTree(ws.RunDir)
}

// removeTree deletes dir and everything in it. It first makes every
// directory in the tree removable, so that read-only directories (git's or
// the child's) don't stop the removal.
func removeTree(dir string) error {
	makeRemovable(dir)
	return os.RemoveAll(dir)
}

// makeRemovable sets every directory in the tree to 0700. WalkDir doesn't
// follow symlinks, and the mode is fixed rather than derived from the
// entry, so nothing outside the tree gains permissions.
func makeRemovable(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}

// cloneRefs is where the mirror keeps the clone's branches for inspection.
const cloneRefs = "refs/heinzel/clone/"

// StrayBranches returns the clone's branches other than the investigation
// branch that hold commits neither the mirror nor the investigation branch
// has: work Claude committed somewhere heinzel won't push. It also notes
// whether the clone still has the investigation branch, which Claude may
// have renamed or deleted.
func (ws *Workspace) StrayBranches(ctx context.Context) ([]string, error) {
	if _, err := ws.git.run(ctx, ws.Mirror, false, "fetch", "--quiet", "--no-tags", ws.Clone, "+refs/heads/*:"+cloneRefs+"*"); err != nil {
		return nil, err
	}
	out, err := ws.git.run(ctx, ws.Mirror, false, "for-each-ref", "--format=%(refname)", cloneRefs)
	if err != nil {
		return nil, err
	}
	refs := slices.DeleteFunc(strings.Split(out, "\n"), func(ref string) bool { return ref == "" })
	ws.branchGone = !slices.Contains(refs, cloneRefs+ws.Branch)
	not := []string{"--not", "--branches"}
	if !ws.branchGone {
		not = append(not, cloneRefs+ws.Branch)
	}
	var stray []string
	for _, ref := range refs {
		name := strings.TrimPrefix(ref, cloneRefs)
		if name == ws.Branch {
			continue
		}
		// Commits that also reached the investigation branch are pushed
		// with it, so they don't count.
		count, err := ws.git.run(ctx, ws.Mirror, false, append([]string{"rev-list", "--count", ref}, not...)...)
		if err != nil {
			return nil, err
		}
		if count != "0" {
			stray = append(stray, name)
		}
	}
	return stray, nil
}

// NewCommits fetches the branch from the clone into the mirror and counts
// its commits on top of the base. It runs after StrayBranches; a branch
// that is gone from the clone has none.
func (ws *Workspace) NewCommits(ctx context.Context) (int, error) {
	if ws.branchGone {
		return 0, nil
	}
	ref := "refs/heads/" + ws.Branch
	if _, err := ws.git.run(ctx, ws.Mirror, false, "fetch", "--quiet", "--no-tags", ws.Clone, "+"+ref+":"+ref); err != nil {
		return 0, err
	}
	out, err := ws.git.run(ctx, ws.Mirror, false, "rev-list", "--count", ws.base+".."+ref)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// ChangedPaths lists the paths the branch changes on top of the base,
// renames listed as a deletion and an addition. It runs after NewCommits.
func (ws *Workspace) ChangedPaths(ctx context.Context) ([]string, error) {
	out, err := ws.git.run(ctx, ws.Mirror, false, "diff", "--name-only", "--no-renames", "-z", ws.base, "refs/heads/"+ws.Branch)
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// Push pushes the branch from the mirror to GitHub, with hooks off.
func (ws *Workspace) Push(ctx context.Context) error {
	ref := "refs/heads/" + ws.Branch
	_, err := ws.git.run(ctx, ws.Mirror, true, "-c", "core.hooksPath=/dev/null", "push", "--quiet", "--no-verify", "origin", ref+":"+ref)
	return err
}

// run runs git in dir and returns its trimmed output. With auth, the token
// reaches git only through the environment (GIT_CONFIG_*), never argv, a
// remote URL or a config file. Errors carry git's output, with the token
// redacted.
func (g gitRunner) run(ctx context.Context, dir string, auth bool, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout(args))
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.env(auth)
	cmd.WaitDelay = g.waitDelay
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
		if len(text) > gitOutputMax {
			text = text[len(text)-gitOutputMax:]
		}
		if g.token != "" {
			text = strings.ReplaceAll(text, g.token, "[redacted]")
		}
		return "", fmt.Errorf("git %s: %w: %s", subcommand(args), err, text)
	}
	return text, nil
}

// env is git's whole environment. With auth (the commands that talk to
// GitHub), it adds the token as an HTTP header and aborts transfers that
// stay below gitLowSpeedLimit bytes/s for gitLowSpeedTime seconds.
func (g gitRunner) env(auth bool) []string {
	env := []string{
		"PATH=" + g.path,
		"HOME=" + g.home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	if !auth {
		return env
	}
	credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.token))
	config := [][2]string{
		{"http.extraHeader", "Authorization: Basic " + credentials},
		{"http.lowSpeedLimit", gitLowSpeedLimit},
		{"http.lowSpeedTime", gitLowSpeedTime},
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]),
		)
	}
	return env
}
