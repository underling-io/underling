package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Pack writes a git bundle of the mirror's default branch to path, for a
// runner to clone. HEAD is in the bundle so that a plain clone of it
// checks out the default branch; no other branch is.
func (ws *Workspace) Pack(ctx context.Context, path string) error {
	ref := "refs/heads/" + ws.DefaultBranch
	if _, err := ws.git.local(ctx, ws.Mirror, "bundle", "create", "--quiet", path, "HEAD", ref); err != nil {
		return err
	}
	out, err := ws.git.local(ctx, ws.Mirror, "bundle", "list-heads", path)
	if err != nil {
		return err
	}
	var refs []string
	for line := range strings.Lines(out) {
		if _, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			refs = append(refs, name)
		}
	}
	slices.Sort(refs)
	if want := []string{"HEAD", ref}; !slices.Equal(refs, want) {
		return fmt.Errorf("the input bundle holds %s, not just %s", strings.Join(refs, ", "), strings.Join(want, ", "))
	}
	return nil
}

// Take makes the bundle at path, a runner's output, the source of
// Claude's branches, once git verified it against the mirror: it is a
// bundle, and the mirror has every commit it builds on. StrayBranches and
// NewCommits then fetch its branches, and nothing else in it, the way
// they fetch a clone's.
func (ws *Workspace) Take(ctx context.Context, path string) error {
	ws.source = ""
	if _, err := ws.git.local(ctx, ws.Mirror, "bundle", "verify", "--quiet", path); err != nil {
		return err
	}
	ws.source = path
	return nil
}

// Work is the runner's run dir: Clone, a clone of underling's input
// bundle on the investigation branch, where Claude works, and the child's
// Home and ClaudeConfig. It holds no token and no GitHub remote.
type Work struct {
	WorkDir      string // symlinks resolved
	RunDir       string
	Clone        string
	Home         string
	ClaudeConfig string

	git gitRunner
	// cloned is set once git cloned the bundle into Clone.
	cloned bool
}

// NewWork makes a run dir under workDir with the child's dirs in it. git
// runs with path as its PATH.
func NewWork(workDir, path string) (_ *Work, err error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	if err := CheckDir(workDir); err != nil {
		return nil, err
	}
	created, err := os.MkdirTemp(workDir, "underling-run-*")
	if err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	defer func() {
		if err != nil {
			_ = removeTree(created)
		}
	}()
	runDir, err := filepath.EvalSymlinks(created)
	if err != nil {
		return nil, err
	}
	w := &Work{
		WorkDir:      filepath.Dir(runDir),
		RunDir:       runDir,
		Clone:        filepath.Join(runDir, cloneDirName),
		Home:         filepath.Join(runDir, homeDirName),
		ClaudeConfig: filepath.Join(runDir, claudeConfigDirName),
	}
	gitHome := filepath.Join(runDir, "git-home")
	w.git = newGitRunner(path, gitHome, "")
	for _, dir := range []string{w.Home, w.ClaudeConfig, gitHome} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// CloneBundle clones the bundle at path into Clone and checks out the new
// branch from the bundle's HEAD, with underling's commit identity. It
// rejects a branch git wouldn't take as a branch name.
func (w *Work) CloneBundle(ctx context.Context, path, branch string) error {
	if name, err := w.git.local(ctx, w.RunDir, "check-ref-format", "--branch", branch); err != nil || name != branch {
		return fmt.Errorf("%q is not a branch name", branch)
	}
	if _, err := w.git.local(ctx, w.RunDir, "clone", "--quiet", path, w.Clone); err != nil {
		return err
	}
	w.cloned = true
	return startBranch(ctx, w.git, w.Clone, branch)
}

// Cloned reports whether CloneBundle got as far as making the clone. It
// is a record of that, not a look at the clone, which Claude may have
// changed or deleted since.
func (w *Work) Cloned() bool { return w.cloned }

// Bundle writes a git bundle of every branch of the clone to path. git
// refuses to make an empty bundle, so a clone without branches fails.
func (w *Work) Bundle(ctx context.Context, path string) error {
	_, err := w.git.local(ctx, w.Clone, "bundle", "create", "--quiet", path, "--branches")
	return err
}

// Remove deletes the run dir.
func (w *Work) Remove() error {
	return removeTree(w.RunDir)
}
