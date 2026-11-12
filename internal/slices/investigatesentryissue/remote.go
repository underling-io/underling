package investigatesentryissue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"underling/internal/slices/investigatesentryissue/claude"
	"underling/internal/slices/investigatesentryissue/contract"
	"underling/internal/slices/investigatesentryissue/workspace"
)

// RunnerLauncher runs an investigation's Claude step outside underling's
// process, in a runner (docs/event-model.md § Runner).
type RunnerLauncher interface {
	// Launch hands the runner the file at inputBundle and prompt, starts
	// it as run describes and waits for it to end. An error means there is
	// no output to read; the launcher has then released what it held, and
	// of the output only Secrets counts.
	Launch(ctx context.Context, run contract.Run, inputBundle, prompt string) (RunnerOutput, error)
}

// RunnerOutput is what a runner left behind. Neither file is trusted:
// Claude can write both.
type RunnerOutput struct {
	// Secrets are the URLs the launcher gave the runner, or signed for
	// itself, to move the run's files. Claude can read the runner's, so a
	// result may name them, and so may a branch or a path of the bundle.
	// underling replaces each, and each one's query, in whatever the
	// investigation logs, stores or publishes from then on: every failure
	// reason, the PR and the run's own log lines. The launcher sets them
	// with an error too.
	Secrets []string
	// Result opens the result file.
	Result func(ctx context.Context) (io.ReadCloser, error)
	// Bundle opens the output bundle. underling calls it only after an ok
	// result; otherwise there may be none.
	Bundle func(ctx context.Context) (io.ReadCloser, error)
	// Release, if set, frees what the launcher holds for the run. The
	// run's cleanup calls it once.
	Release func()
}

// remoteCloner gives each investigation a mirror of its repo and a bundle
// of the default branch, for Claude to work on in a runner.
type remoteCloner struct {
	cfg      config
	launcher RunnerLauncher
	logger   *slog.Logger
}

func (c *remoteCloner) clone(ctx context.Context, issueID string, target repo, branch string) (run, error) {
	runID, err := contract.NewRunID()
	if err != nil {
		return nil, err
	}
	url := c.cfg.cloneBase + target.owner + "/" + target.name + ".git"
	ws, err := workspace.NewMirror(ctx, c.cfg.workDir, url, branch, c.cfg.path, c.cfg.githubToken)
	if err != nil {
		return nil, err
	}
	r := &remoteRun{ws: ws, cfg: c.cfg, launcher: c.launcher, runID: runID, issueID: issueID, logger: c.logger}
	if err := ws.Pack(ctx, r.inputBundle()); err != nil {
		r.remove()
		return nil, err
	}
	return r, nil
}

// remoteRun is an investigation whose Claude step runs in a runner.
type remoteRun struct {
	ws       *workspace.Workspace
	cfg      config
	launcher RunnerLauncher
	runID    string
	issueID  string
	logger   *slog.Logger
	release  func() // the launched runner's, or nil
	// secrets are the launched runner's (RunnerOutput.Secrets), as
	// contract.Secrets returned them. They last as long as the run:
	// what git reads out of the runner's bundle can name them too.
	secrets []string
}

func (r *remoteRun) inputBundle() string  { return filepath.Join(r.ws.RunDir, "input.bundle") }
func (r *remoteRun) outputBundle() string { return filepath.Join(r.ws.RunDir, "output.bundle") }

// claude runs the runner and takes its output into the mirror. The summary
// and the errors may show one of the run's URLs (RunnerOutput.Secrets);
// the caller passes them through redact, like everything else of the run.
//
// Every error but the runner's own timeout result is flattened to text: a
// git or net error that reports Timeout would otherwise pass for Claude
// timing out.
func (r *remoteRun) claude(ctx context.Context, prompt string) (string, error) {
	b := r.cfg.bedrock
	out, err := r.launcher.Launch(ctx, contract.Run{
		RunID:   r.runID,
		Branch:  r.ws.Branch,
		Timeout: r.cfg.timeout,
		Bedrock: contract.Bedrock{Region: b.region, Model: b.model, SmallFastModel: b.smallFastModel, CodexModel: b.codexModel, CodexRegion: b.codexRegion},
	}, r.inputBundle(), prompt)
	r.secrets = contract.Secrets(out.Secrets...)
	if err != nil {
		return "", fmt.Errorf("run the runner: %s", err.Error())
	}
	r.release = out.Release

	result, err := readResult(ctx, out)
	if err != nil {
		return "", errors.New(err.Error())
	}
	switch result.Status {
	case contract.StatusTimeout:
		return "", &claude.TimeoutError{After: r.cfg.timeout}
	case contract.StatusFailed:
		return "", errors.New(result.Reason)
	}
	if err := r.takeBundle(ctx, out); err != nil {
		return "", errors.New(err.Error())
	}
	return result.Summary, nil
}

// readResult reads and checks the runner's result, at most
// contract.MaxResultBytes of it.
func readResult(ctx context.Context, out RunnerOutput) (contract.Result, error) {
	body, err := out.Result(ctx)
	if err != nil {
		return contract.Result{}, fmt.Errorf("download the runner's result: %w", err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, contract.MaxResultBytes+1))
	if err != nil {
		return contract.Result{}, fmt.Errorf("download the runner's result: %w", err)
	}
	if len(data) > contract.MaxResultBytes {
		return contract.Result{}, fmt.Errorf("the runner's result is larger than %d bytes", contract.MaxResultBytes)
	}
	result, err := contract.DecodeResult(data)
	if err != nil {
		return contract.Result{}, fmt.Errorf("the runner's %w", err)
	}
	return result, nil
}

// takeBundle copies the runner's bundle into the run dir, at most the
// configured size of it, and has the mirror take it once git verified it.
func (r *remoteRun) takeBundle(ctx context.Context, out RunnerOutput) error {
	body, err := out.Bundle(ctx)
	if err != nil {
		return fmt.Errorf("download the runner's bundle: %w", err)
	}
	defer body.Close()
	file, err := os.OpenFile(r.outputBundle(), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(file, io.LimitReader(body, r.cfg.maxBundle+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download the runner's bundle: %w", err)
	}
	if n > r.cfg.maxBundle {
		return fmt.Errorf("the runner's bundle is larger than %d bytes (%s)", r.cfg.maxBundle, maxBundleEnvVar)
	}
	return r.ws.Take(ctx, r.outputBundle())
}

func (r *remoteRun) strayBranches(ctx context.Context) ([]string, error) {
	return r.ws.StrayBranches(ctx)
}

func (r *remoteRun) newCommits(ctx context.Context) (int, error) { return r.ws.NewCommits(ctx) }

func (r *remoteRun) changedPaths(ctx context.Context) ([]string, error) {
	return r.ws.ChangedPaths(ctx)
}

func (r *remoteRun) push(ctx context.Context) error { return r.ws.Push(ctx) }

// redact returns text with the launched runner's secrets replaced.
func (r *remoteRun) redact(text string) string { return contract.Redact(text, r.secrets) }

func (r *remoteRun) defaultBranch() string { return r.ws.DefaultBranch }

func (r *remoteRun) remove() {
	if r.release != nil {
		r.release()
	}
	if err := r.ws.Remove(); err != nil {
		r.logger.Error("remove investigation workspace", "handler", handlerName, "issueId", r.issueID, "dir", r.ws.RunDir, "error", r.redact(err.Error()))
	}
}
