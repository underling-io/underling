// Package runner is the runner command (docs/event-model.md § Runner): one
// investigation's Claude step, run in a process of its own. It reads the
// contract from its environment, downloads underling's input bundle and
// prompt, lets Claude work in a clone of the bundle, and uploads a bundle
// of the clone's branches and a result file. It never sees the GitHub
// token.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"underling/internal/slices/investigatesentryissue/bedrock"
	"underling/internal/slices/investigatesentryissue/claude"
	"underling/internal/slices/investigatesentryissue/contract"
	"underling/internal/slices/investigatesentryissue/workspace"
)

// The runner's settings besides the contract. They come from the image's
// environment, like underling's own.
const (
	pluginDirsEnvVar = "CLAUDE_PLUGIN_DIRS"
	claudeBinEnvVar  = "CLAUDE_BIN"
	workDirEnvVar    = "INVESTIGATION_WORK_DIR"
)

const (
	// stopGrace is how long the result upload may still take once the
	// runner was told to stop. ECS kills a task 30s after SIGTERM.
	stopGrace = 20 * time.Second
	// maxPromptBytes is the largest prompt the runner downloads.
	maxPromptBytes = 1 << 20
)

// errStopped is why a run ended whose runner was told to stop.
var errStopped = errors.New("the runner was stopped before it finished")

// Params is what Run needs.
type Params struct {
	Getenv func(string) string // like os.Getenv
	Logger *slog.Logger
	// AWS is where the run gets its AWS credentials; nil means the AWS
	// SDK's default chain.
	AWS aws.CredentialsProvider
	// HTTPClient moves the run's files. It must not have a Timeout: each
	// transfer has its own.
	HTTPClient *http.Client

	retryDelay time.Duration // between transfer attempts; 0 means the default
}

// Run runs one investigation as the environment describes it and uploads
// its result. Cancelling ctx stops it. It returns the process's exit code:
// 0 only if the result was uploaded, whatever the result says.
func Run(ctx context.Context, p Params) int {
	r := &run{
		getenv:    p.Getenv,
		aws:       p.AWS,
		transfers: transfers{client: p.HTTPClient, retryDelay: p.retryDelay},
		secrets:   urlSecrets(p.Getenv),
	}
	r.log = p.Logger.With("runId", r.redact(p.Getenv(contract.RunIDEnvVar)))

	resultURL, err := contract.ParseResultURL(p.Getenv)
	if err != nil {
		// Without the result's URL nothing can report the rest either.
		if _, envErr := loadEnv(p.Getenv); envErr != nil {
			err = envErr
		}
		r.log.Error("invalid runner environment; there is nowhere to upload a result to", "error", r.redact(err.Error()))
		return 2
	}

	result := r.investigate(ctx)
	if r.work != nil {
		defer func() {
			if err := r.work.Remove(); err != nil {
				r.log.Error("remove the run dir", "dir", r.work.RunDir, "error", r.redact(err.Error()))
			}
		}()
	}
	r.log.Info("investigation ended", "status", result.Status, "reason", result.Reason)

	data, err := result.Encode()
	if err != nil {
		r.log.Error("encode the result", "error", err)
		return 1
	}
	// The result must still go out when ctx is done: after a stop signal
	// it is all the runner has left to say.
	uploadCtx, cancel := outliving(ctx, stopGrace)
	defer cancel()
	if err := r.transfers.uploadBytes(uploadCtx, resultURL, data); err != nil {
		r.log.Error("upload the result", "error", r.redact(err.Error()))
		return 1
	}
	return 0
}

// outliving returns a context that is cancelled grace after ctx is.
func outliving(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	out, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() {
		timer := time.AfterFunc(grace, cancel)
		context.AfterFunc(out, func() { timer.Stop() })
	})
	return out, func() {
		stop()
		cancel()
	}
}

// env is the runner's whole configuration.
type env struct {
	contract.Env
	path       string // git's and the child's PATH: the runner's own
	claudeBin  string
	pluginDirs []string
	workDir    string
}

// loadEnv reads the configuration through getenv. It reports every
// missing or invalid variable, each by name.
func loadEnv(getenv func(string) string) (env, error) {
	e := env{path: getenv("PATH"), claudeBin: getenv(claudeBinEnvVar), workDir: getenv(workDirEnvVar)}
	if e.claudeBin == "" {
		e.claudeBin = "claude"
	}
	if e.workDir == "" {
		e.workDir = workspace.DefaultDir()
	}
	var errs []error
	var err error
	if e.Env, err = contract.ParseEnv(getenv); err != nil {
		errs = append(errs, err)
	}
	for dir := range strings.SplitSeq(getenv(pluginDirsEnvVar), ",") {
		if dir = strings.TrimSpace(dir); dir != "" {
			e.pluginDirs = append(e.pluginDirs, dir)
		}
	}
	if len(e.pluginDirs) == 0 {
		errs = append(errs, fmt.Errorf("%s: must be set", pluginDirsEnvVar))
	}
	if len(errs) > 0 {
		return env{}, errors.Join(errs...)
	}
	return e, nil
}

// run is one run of the runner.
type run struct {
	getenv    func(string) string
	log       *slog.Logger
	aws       aws.CredentialsProvider
	transfers transfers
	// secrets are the texts no log line or result may show: the URLs and
	// their queries.
	secrets []string
	// work is the run dir, once there is one. Run removes it.
	work *workspace.Work
}

// urlSecrets returns the contract's URLs as getenv has them, and each
// one's query.
func urlSecrets(getenv func(string) string) []string {
	return contract.Secrets(getenv(contract.InputBundleURLEnvVar), getenv(contract.PromptURLEnvVar), getenv(contract.OutputBundleURLEnvVar), getenv(contract.ResultURLEnvVar))
}

// redact returns text without the run's URLs and their queries.
func (r *run) redact(text string) string {
	return contract.Redact(text, r.secrets)
}

// failed is the result of a run that err ended.
func (r *run) failed(err error) contract.Result {
	return contract.Result{Status: contract.StatusFailed, Reason: r.redact(err.Error())}
}

// investigate runs the investigation up to its result. Whatever fails
// becomes a failed result with the error's text; only Claude running out
// of time is a timeout.
func (r *run) investigate(ctx context.Context) contract.Result {
	e, err := loadEnv(r.getenv)
	if err != nil {
		return r.failed(fmt.Errorf("invalid runner environment: %w", err))
	}
	if r.work, err = workspace.NewWork(e.workDir, e.path); err != nil {
		return r.failed(err)
	}

	summary, err := r.claude(ctx, e)
	var result contract.Result
	var timeout *claude.TimeoutError
	switch {
	case ctx.Err() != nil:
		// Stopped: there is no time left to bundle the clone.
		return r.failed(errStopped)
	case err == nil:
		result = contract.Result{Status: contract.StatusOK, Summary: r.redact(summary)}
	case errors.As(err, &timeout):
		result = contract.Result{Status: contract.StatusTimeout, Reason: err.Error()}
	default:
		result = r.failed(err)
	}

	if !r.work.Cloned() {
		return result
	}
	// The bundle goes out for every status, and before the result: an ok
	// result promises the bundle is there.
	if err := r.returnBundle(ctx, e); err != nil {
		switch {
		case ctx.Err() != nil:
			return r.failed(errStopped)
		case result.Status == contract.StatusOK:
			return r.failed(err)
		}
		r.log.Error("return the work clone's branches", "error", r.redact(err.Error()))
	}
	return result
}

// claude downloads the inputs, clones the bundle and runs Claude in the
// clone. Its only *claude.TimeoutError is Claude's own.
func (r *run) claude(ctx context.Context, e env) (string, error) {
	w := r.work
	bundle := filepath.Join(w.RunDir, "input.bundle")
	if err := r.transfers.download(ctx, e.URLs.InputBundle, bundle); err != nil {
		return "", fmt.Errorf("download the input bundle: %w", err)
	}
	prompt, err := r.transfers.downloadBytes(ctx, e.URLs.Prompt, maxPromptBytes)
	if err != nil {
		return "", fmt.Errorf("download the prompt: %w", err)
	}
	if err := w.CloneBundle(ctx, bundle, e.Branch); err != nil {
		return "", err
	}

	b := e.Bedrock
	cache, err := bedrock.NewCredentialsCache(ctx, r.aws, b.Region, e.Timeout)
	if err != nil {
		return "", err
	}
	credentials, err := bedrock.RunCredentials(ctx, cache, e.Timeout)
	if err != nil {
		return "", err
	}
	if err := bedrock.WriteCodexConfig(w.Home, b.CodexModel, b.CodexRegion); err != nil {
		return "", err
	}
	cmd, err := claude.UnsandboxedCommand(claude.Config{
		Bin:        e.claudeBin,
		PluginDirs: e.pluginDirs,
		Path:       e.path,
		Bedrock:    &claude.Bedrock{Region: b.Region, Model: b.Model, SmallFastModel: b.SmallFastModel, CodexRegion: b.CodexRegion, Credentials: credentials},
	}, claude.Dirs{WorkDir: w.WorkDir, Clone: w.Clone, Home: w.Home, ClaudeConfig: w.ClaudeConfig}, string(prompt))
	if err != nil {
		return "", err
	}
	r.log.Info("claude starts", "branch", e.Branch, "timeout", e.Timeout.String())
	return claude.Run(ctx, cmd, e.Timeout)
}

// returnBundle bundles the clone's branches and uploads the bundle.
func (r *run) returnBundle(ctx context.Context, e env) error {
	bundle := filepath.Join(r.work.RunDir, "output.bundle")
	if err := r.work.Bundle(ctx, bundle); err != nil {
		return fmt.Errorf("bundle the work clone's branches: %s", err.Error())
	}
	if err := r.transfers.upload(ctx, e.URLs.OutputBundle, bundle); err != nil {
		return fmt.Errorf("upload the output bundle: %w", err)
	}
	return nil
}
