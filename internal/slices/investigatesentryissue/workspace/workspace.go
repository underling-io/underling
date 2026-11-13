// Package workspace is the git side of an investigation run: a run dir
// holding a mirror of the GitHub repo and a clone of it for Claude, and the
// git commands that inspect Claude's commits and push them. When Claude
// runs elsewhere (bundle.go), the clone is in the runner's own run dir and
// git bundles carry the commits between the two.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	gitUserName    = "underling"
	gitUserEmail   = "underling@users.noreply.github.com"
	defaultDirName = "underling-investigations"

	// The run dir's entries that Claude uses.
	cloneDirName        = "repo"
	homeDirName         = "home"
	claudeConfigDirName = "claude-config"

	// cloneRefs is where the mirror keeps the clone's branches for
	// inspection.
	cloneRefs = "refs/underling/clone/"
)

// Workspace is one run's dir and what is in it:
//
//   - Mirror, a bare clone of the GitHub repo. Only underling uses it, with
//     the token, to clone from and push from.
//   - Clone, a clone of the mirror on the investigation branch, where
//     Claude works. It holds neither the token nor a GitHub remote.
//   - Home and ClaudeConfig, the child's HOME and CLAUDE_CONFIG_DIR.
//   - GitHome, the HOME of underling's own git. The child can't touch it, so
//     it can't plant git config (say url.<x>.insteadOf) that the push with
//     the token would honour.
//
// Pushing from the mirror keeps whatever Claude does to the clone's .git
// (hooks, config, remotes) away from the token.
//
// A workspace made by NewMirror has only Mirror and GitHome: Claude works
// in a runner's clone, and Take names the bundle its branches come from.
type Workspace struct {
	WorkDir string // symlinks resolved
	// Siblings are the work dir's other entries when the run began, such
	// as leftovers of earlier runs.
	Siblings      []string
	RunDir        string
	Mirror        string
	Clone         string
	Home          string
	ClaudeConfig  string
	GitHome       string
	Branch        string
	DefaultBranch string

	base       string // the default branch's commit the branch starts from
	branchGone bool   // set by StrayBranches: the clone lost the branch
	// source is where Claude's branches are fetched from: Clone, or the
	// bundle Take accepted; "" before Take.
	source string
	git    gitRunner
}

// New makes a run dir under workDir, mirrors cloneURL into it and checks
// out branch from the default branch in a clone of the mirror. git runs
// with path as its PATH and gets token only to talk to GitHub. On error,
// New removes what it made.
func New(ctx context.Context, workDir, cloneURL, branch, path, token string) (*Workspace, error) {
	return newWorkspace(ctx, workDir, cloneURL, branch, path, token, true)
}

// NewMirror is New without the clone and the child's dirs, for a run whose
// Claude works in a runner: Pack bundles the mirror for it, and Take
// accepts the bundle it returns.
func NewMirror(ctx context.Context, workDir, cloneURL, branch, path, token string) (*Workspace, error) {
	return newWorkspace(ctx, workDir, cloneURL, branch, path, token, false)
}

func newWorkspace(ctx context.Context, workDir, cloneURL, branch, path, token string, withClone bool) (_ *Workspace, err error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	if err := CheckDir(workDir); err != nil {
		return nil, err
	}
	created, err := os.MkdirTemp(workDir, "underling-investigation-*")
	if err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	defer func() {
		if err != nil {
			_ = removeTree(created)
		}
	}()
	// The sandbox must name the paths the child sees, e.g. /private/var
	// rather than /var on macOS.
	runDir, err := filepath.EvalSymlinks(created)
	if err != nil {
		return nil, err
	}
	ws := &Workspace{
		WorkDir: filepath.Dir(runDir),
		RunDir:  runDir,
		Mirror:  filepath.Join(runDir, "mirror.git"),
		GitHome: filepath.Join(runDir, "git-home"),
		Branch:  branch,
	}
	dirs := []string{ws.GitHome}
	if withClone {
		ws.Clone = filepath.Join(runDir, cloneDirName)
		ws.Home = filepath.Join(runDir, homeDirName)
		ws.ClaudeConfig = filepath.Join(runDir, claudeConfigDirName)
		ws.source = ws.Clone
		dirs = append(dirs, ws.Home, ws.ClaudeConfig)
	}
	ws.git = newGitRunner(path, ws.GitHome, token)
	if ws.Siblings, err = siblings(ws.WorkDir, runDir); err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := ws.mirrorRepo(ctx, cloneURL); err != nil {
		return nil, err
	}
	if withClone {
		if err := ws.cloneMirror(ctx); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

func siblings(workDir, runDir string) ([]string, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if p := filepath.Join(workDir, e.Name()); p != runDir {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// mirrorRepo mirrors cloneURL and notes its default branch and that
// branch's commit.
func (ws *Workspace) mirrorRepo(ctx context.Context, cloneURL string) error {
	g := ws.git
	if _, err := g.network(ctx, ws.RunDir, "clone", "--quiet", "--bare", cloneURL, ws.Mirror); err != nil {
		return err
	}
	var err error
	if ws.DefaultBranch, err = g.local(ctx, ws.Mirror, "symbolic-ref", "--short", "HEAD"); err != nil {
		return err
	}
	ws.base, err = g.local(ctx, ws.Mirror, "rev-parse", "--verify", "HEAD^{commit}")
	return err
}

// cloneMirror clones the mirror for Claude, on the investigation branch.
func (ws *Workspace) cloneMirror(ctx context.Context) error {
	// --no-hardlinks: Claude may chmod and rewrite the clone's objects,
	// which must not change the mirror's.
	if _, err := ws.git.local(ctx, ws.RunDir, "clone", "--quiet", "--no-hardlinks", ws.Mirror, ws.Clone); err != nil {
		return err
	}
	return startBranch(ctx, ws.git, ws.Clone, ws.Branch)
}

// startBranch gives clone underling's commit identity and checks out the
// new branch.
func startBranch(ctx context.Context, g gitRunner, clone, branch string) error {
	for _, args := range [][]string{
		{"config", "user.name", gitUserName},
		{"config", "user.email", gitUserEmail},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "--quiet", "-b", branch},
	} {
		if _, err := g.local(ctx, clone, args...); err != nil {
			return err
		}
	}
	return nil
}

// errNoSource is what inspecting a mirror-only workspace returns before
// Take accepted a bundle.
var errNoSource = errors.New("no bundle was taken into the mirror")

// StrayBranches returns the clone's branches, other than the investigation
// branch, holding commits that neither the mirror nor the investigation
// branch has: work underling won't push. It also notes whether Claude
// renamed or deleted the investigation branch. The branches come from the
// clone, or from the bundle Take accepted.
func (ws *Workspace) StrayBranches(ctx context.Context) ([]string, error) {
	if ws.source == "" {
		return nil, errNoSource
	}
	if _, err := ws.git.local(ctx, ws.Mirror, "fetch", "--quiet", "--no-tags", ws.source, "+refs/heads/*:"+cloneRefs+"*"); err != nil {
		return nil, err
	}
	out, err := ws.git.local(ctx, ws.Mirror, "for-each-ref", "--format=%(refname)", cloneRefs)
	if err != nil {
		return nil, err
	}
	refs := strings.Fields(out)
	ws.branchGone = !slices.Contains(refs, cloneRefs+ws.Branch)
	// Commits that also reached the investigation branch get pushed with
	// it, so they don't count.
	known := []string{"--not", "--branches"}
	if !ws.branchGone {
		known = append(known, cloneRefs+ws.Branch)
	}
	var stray []string
	for _, ref := range refs {
		name := strings.TrimPrefix(ref, cloneRefs)
		if name == ws.Branch {
			continue
		}
		count, err := ws.git.local(ctx, ws.Mirror, append([]string{"rev-list", "--count", ref}, known...)...)
		if err != nil {
			return nil, err
		}
		if count != "0" {
			stray = append(stray, name)
		}
	}
	return stray, nil
}

// NewCommits fetches the branch from the source into the mirror and counts
// its commits on top of the base. It runs after StrayBranches; a branch
// the clone lost has none.
func (ws *Workspace) NewCommits(ctx context.Context) (int, error) {
	if ws.branchGone {
		return 0, nil
	}
	ref := "refs/heads/" + ws.Branch
	if _, err := ws.git.local(ctx, ws.Mirror, "fetch", "--quiet", "--no-tags", ws.source, "+"+ref+":"+ref); err != nil {
		return 0, err
	}
	out, err := ws.git.local(ctx, ws.Mirror, "rev-list", "--count", ws.base+".."+ref)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// ChangedPaths lists the paths the branch changes on top of the base, a
// rename as a deletion and an addition. It runs after NewCommits.
func (ws *Workspace) ChangedPaths(ctx context.Context) ([]string, error) {
	out, err := ws.git.local(ctx, ws.Mirror, "diff", "--name-only", "--no-renames", "-z", ws.base, "refs/heads/"+ws.Branch)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(strings.Split(out, "\x00"), func(p string) bool { return p == "" }), nil
}

// Push pushes the branch from the mirror to GitHub, with hooks off.
func (ws *Workspace) Push(ctx context.Context) error {
	ref := "refs/heads/" + ws.Branch
	_, err := ws.git.network(ctx, ws.Mirror, "-c", "core.hooksPath=/dev/null", "push", "--quiet", "--no-verify", "origin", ref+":"+ref)
	return err
}

// Remove deletes the run dir.
func (ws *Workspace) Remove() error {
	return removeTree(ws.RunDir)
}

// removeTree deletes dir, first making every dir in it removable, so that
// read-only dirs (git's or the child's) don't stop it.
func removeTree(dir string) error {
	makeRemovable(dir)
	return os.RemoveAll(dir)
}

// makeRemovable sets every dir in the tree to 0700. WalkDir follows no
// symlinks, and the mode is fixed, so nothing outside the tree changes.
func makeRemovable(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
