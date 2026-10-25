package investigatesentryissue

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
)

// workspace is the per-run directory layout, all under one temp run dir:
//
//   - mirror: a bare clone of the GitHub repo. Only heinzel uses it, with the
//     GitHub token, to clone from and to push from.
//   - clone: a clone of the mirror on the investigation branch, where Claude
//     works. It never holds the token or a GitHub remote.
//   - home and claudeConfig: the child's HOME and CLAUDE_CONFIG_DIR.
//   - gitHome: the HOME of heinzel's own git commands. The child never
//     sees it and can't write to it, so it can't plant git config (such as
//     url.<x>.insteadOf) that heinzel's push with the token would honour.
//
// Pushing from the mirror rather than the clone keeps Claude's edits to the
// clone's .git (hooks, config, remotes) away from the token: heinzel only
// fetches the branch's commits out of the clone.
type workspace struct {
	workDir string // the work dir, symlinks resolved
	// siblings are the other entries in the work dir when the run began,
	// such as leftovers of earlier runs.
	siblings      []string
	runDir        string
	mirror        string
	clone         string
	home          string
	claudeConfig  string
	gitHome       string
	branch        string
	defaultBranch string
	base          string // the default branch's commit the branch starts from
	// branchGone is set by strayBranches when the clone no longer has the
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

// newWorkspace creates a run dir under workDir, mirrors cloneURL into it and
// checks out branch from the default branch in a clone of the mirror. On
// error it removes what it created.
func newWorkspace(ctx context.Context, workDir, cloneURL, branch string, cfg config) (_ *workspace, err error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	if err := checkWorkDir(workDir); err != nil {
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
	ws := &workspace{
		workDir:      filepath.Dir(runDir),
		runDir:       runDir,
		mirror:       filepath.Join(runDir, "mirror.git"),
		clone:        filepath.Join(runDir, "repo"),
		home:         filepath.Join(runDir, "home"),
		claudeConfig: filepath.Join(runDir, "claude-config"),
		gitHome:      filepath.Join(runDir, "git-home"),
		branch:       branch,
	}
	entries, err := os.ReadDir(ws.workDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(runDir) {
			ws.siblings = append(ws.siblings, filepath.Join(ws.workDir, e.Name()))
		}
	}
	ws.git = newGitRunner(cfg.path, ws.gitHome, cfg.githubToken)
	for _, dir := range []string{ws.home, ws.claudeConfig, ws.gitHome} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
	}
	g := ws.git
	if _, err := g.run(ctx, runDir, true, "clone", "--quiet", "--bare", cloneURL, ws.mirror); err != nil {
		return nil, err
	}
	if ws.defaultBranch, err = g.run(ctx, ws.mirror, false, "symbolic-ref", "--short", "HEAD"); err != nil {
		return nil, err
	}
	if ws.base, err = g.run(ctx, ws.mirror, false, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return nil, err
	}
	// --no-hardlinks: Claude may chmod and rewrite the clone's object files,
	// which must not change the mirror's.
	if _, err := g.run(ctx, runDir, false, "clone", "--quiet", "--no-hardlinks", ws.mirror, ws.clone); err != nil {
		return nil, err
	}
	for _, args := range [][]string{
		{"config", "user.name", gitUserName},
		{"config", "user.email", gitUserEmail},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "--quiet", "-b", branch},
	} {
		if _, err := g.run(ctx, ws.clone, false, args...); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// remove deletes the run dir.
func (ws *workspace) remove() error {
	return removeTree(ws.runDir)
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

// strayBranches returns the clone's branches other than the investigation
// branch that hold commits neither the mirror nor the investigation branch
// has: work Claude committed somewhere heinzel won't push. It also notes
// whether the clone still has the investigation branch, which Claude may
// have renamed or deleted.
func (ws *workspace) strayBranches(ctx context.Context) ([]string, error) {
	if _, err := ws.git.run(ctx, ws.mirror, false, "fetch", "--quiet", "--no-tags", ws.clone, "+refs/heads/*:"+cloneRefs+"*"); err != nil {
		return nil, err
	}
	out, err := ws.git.run(ctx, ws.mirror, false, "for-each-ref", "--format=%(refname)", cloneRefs)
	if err != nil {
		return nil, err
	}
	refs := slices.DeleteFunc(strings.Split(out, "\n"), func(ref string) bool { return ref == "" })
	ws.branchGone = !slices.Contains(refs, cloneRefs+ws.branch)
	not := []string{"--not", "--branches"}
	if !ws.branchGone {
		not = append(not, cloneRefs+ws.branch)
	}
	var stray []string
	for _, ref := range refs {
		name := strings.TrimPrefix(ref, cloneRefs)
		if name == ws.branch {
			continue
		}
		// Commits that also reached the investigation branch are pushed
		// with it, so they don't count.
		count, err := ws.git.run(ctx, ws.mirror, false, append([]string{"rev-list", "--count", ref}, not...)...)
		if err != nil {
			return nil, err
		}
		if count != "0" {
			stray = append(stray, name)
		}
	}
	return stray, nil
}

// newCommits fetches the branch from the clone into the mirror and counts
// its commits on top of the base. It runs after strayBranches; a branch
// that is gone from the clone has none.
func (ws *workspace) newCommits(ctx context.Context) (int, error) {
	if ws.branchGone {
		return 0, nil
	}
	ref := "refs/heads/" + ws.branch
	if _, err := ws.git.run(ctx, ws.mirror, false, "fetch", "--quiet", "--no-tags", ws.clone, "+"+ref+":"+ref); err != nil {
		return 0, err
	}
	out, err := ws.git.run(ctx, ws.mirror, false, "rev-list", "--count", ws.base+".."+ref)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// changedPaths lists the paths the branch changes on top of the base,
// renames listed as a deletion and an addition.
func (ws *workspace) changedPaths(ctx context.Context) ([]string, error) {
	out, err := ws.git.run(ctx, ws.mirror, false, "diff", "--name-only", "--no-renames", "-z", ws.base, "refs/heads/"+ws.branch)
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

// ciConfigFiles and ciConfigDirs are the repo paths that CI services read
// their pipeline definitions from.
var (
	ciConfigFiles = []string{".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml", ".travis.yml", "bitbucket-pipelines.yml"}
	ciConfigDirs  = []string{".github/", ".circleci/", ".buildkite/"}
)

// ciConfigPath reports whether path is CI configuration. A pushed branch
// that changes it could run a pipeline of Claude's making with the repo's
// CI secrets.
func ciConfigPath(path string) bool {
	if slices.Contains(ciConfigFiles, path) {
		return true
	}
	for _, dir := range ciConfigDirs {
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return false
}

// push pushes the branch from the mirror to GitHub, with hooks off.
func (ws *workspace) push(ctx context.Context) error {
	ref := "refs/heads/" + ws.branch
	_, err := ws.git.run(ctx, ws.mirror, true, "-c", "core.hooksPath=/dev/null", "push", "--quiet", "--no-verify", "origin", ref+":"+ref)
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
