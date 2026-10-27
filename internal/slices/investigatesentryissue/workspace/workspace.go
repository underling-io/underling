// Package workspace is the git side of an investigation run: a run dir
// holding a mirror of the GitHub repo and a clone of it for Claude, and the
// git commands that inspect Claude's commits and push them.
package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	gitUserName    = "heinzel"
	gitUserEmail   = "heinzel@users.noreply.github.com"
	defaultDirName = "heinzel-investigations"

	// cloneRefs is where the mirror keeps the clone's branches for
	// inspection.
	cloneRefs = "refs/heinzel/clone/"
)

// Workspace is one run's dir and what is in it:
//
//   - Mirror, a bare clone of the GitHub repo. Only heinzel uses it, with
//     the token, to clone from and push from.
//   - Clone, a clone of the mirror on the investigation branch, where
//     Claude works. It holds neither the token nor a GitHub remote.
//   - Home and ClaudeConfig, the child's HOME and CLAUDE_CONFIG_DIR.
//   - GitHome, the HOME of heinzel's own git. The child can't touch it, so
//     it can't plant git config (say url.<x>.insteadOf) that the push with
//     the token would honour.
//
// Pushing from the mirror keeps whatever Claude does to the clone's .git
// (hooks, config, remotes) away from the token.
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
	git        gitRunner
}

// New makes a run dir under workDir, mirrors cloneURL into it and checks
// out branch from the default branch in a clone of the mirror. git runs
// with path as its PATH and gets token only to talk to GitHub. On error,
// New removes what it made.
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
	// The sandbox must name the paths the child sees, e.g. /private/var
	// rather than /var on macOS.
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
	ws.git = newGitRunner(path, ws.GitHome, token)
	if ws.Siblings, err = siblings(ws.WorkDir, runDir); err != nil {
		return nil, err
	}
	for _, dir := range []string{ws.Home, ws.ClaudeConfig, ws.GitHome} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := ws.cloneRepo(ctx, cloneURL); err != nil {
		return nil, err
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

func (ws *Workspace) cloneRepo(ctx context.Context, cloneURL string) error {
	g := ws.git
	if _, err := g.network(ctx, ws.RunDir, "clone", "--quiet", "--bare", cloneURL, ws.Mirror); err != nil {
		return err
	}
	var err error
	if ws.DefaultBranch, err = g.local(ctx, ws.Mirror, "symbolic-ref", "--short", "HEAD"); err != nil {
		return err
	}
	if ws.base, err = g.local(ctx, ws.Mirror, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return err
	}
	// --no-hardlinks: Claude may chmod and rewrite the clone's objects,
	// which must not change the mirror's.
	if _, err := g.local(ctx, ws.RunDir, "clone", "--quiet", "--no-hardlinks", ws.Mirror, ws.Clone); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"config", "user.name", gitUserName},
		{"config", "user.email", gitUserEmail},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "--quiet", "-b", ws.Branch},
	} {
		if _, err := g.local(ctx, ws.Clone, args...); err != nil {
			return err
		}
	}
	return nil
}

// StrayBranches returns the clone's branches, other than the investigation
// branch, holding commits that neither the mirror nor the investigation
// branch has: work heinzel won't push. It also notes whether Claude
// renamed or deleted the investigation branch.
func (ws *Workspace) StrayBranches(ctx context.Context) ([]string, error) {
	if _, err := ws.git.local(ctx, ws.Mirror, "fetch", "--quiet", "--no-tags", ws.Clone, "+refs/heads/*:"+cloneRefs+"*"); err != nil {
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

// NewCommits fetches the branch from the clone into the mirror and counts
// its commits on top of the base. It runs after StrayBranches; a branch
// the clone lost has none.
func (ws *Workspace) NewCommits(ctx context.Context) (int, error) {
	if ws.branchGone {
		return 0, nil
	}
	ref := "refs/heads/" + ws.Branch
	if _, err := ws.git.local(ctx, ws.Mirror, "fetch", "--quiet", "--no-tags", ws.Clone, "+"+ref+":"+ref); err != nil {
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
