// Package investigatesentryissue is an automation slice:
//
//	SentryIssueAlertReceived → sentry-issue-investigator →
//	  SentryIssueInvestigationStarted, then PullRequestOpened, NoChanges or Failed
//	  (or SentryIssueInvestigationSkipped)
//
// For each new Sentry issue it clones the GitHub repo mapped to the issue's
// project, lets headless Claude Code fix the issue in a sandboxed child
// process, then pushes the branch and opens a draft PR itself, so Claude
// never sees the GitHub token.
//
// investigator.go holds the decision path from alert to recorded outcome.
// It reaches git, Claude and GitHub through small interfaces, which
// infrastructure.go implements with the sub-packages workspace (the clone
// and git), claude (the sandboxed child) and github (the draft PR).
//
// With Params.Runner set, Claude runs in a runner instead, a process of
// its own elsewhere: remote.go packs the repo into a git bundle for it and
// takes its result and its bundle back, as the contract sub-package
// defines them. The runner sub-package is that process's program
// (cmd/underling-runner); underling's half never imports it.
package investigatesentryissue

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"underling/internal/platform/app"
	"underling/internal/platform/eventhandler"
	"underling/internal/platform/eventstore"
	"underling/internal/slices/investigatesentryissue/bedrock"
	"underling/internal/slices/investigatesentryissue/github"
	"underling/internal/slices/investigatesentryissue/workspace"
)

func init() {
	app.RegisterAutomation(func(d app.Deps) (app.Automation, error) {
		return New(Params{
			Getenv:     d.Getenv,
			Logger:     d.Logger,
			HTTPClient: &http.Client{Timeout: 30 * time.Second},
			Now:        time.Now,
		})
	})
}

// Params is what New needs.
type Params struct {
	Getenv     func(string) string // the slice's config, like os.Getenv
	Logger     *slog.Logger
	HTTPClient *http.Client
	// Now is read once, when the handler is built: alerts stored more than
	// the max age before then are stale.
	Now func() time.Time
	// AWS, in Bedrock mode, is where runs get their AWS credentials; New
	// caches them. nil means the AWS SDK's default chain.
	AWS aws.CredentialsProvider
	// Runner, if set, runs each investigation's Claude step in a runner
	// instead of in a child process of underling's. It needs Bedrock mode.
	Runner RunnerLauncher
}

// New returns the slice, or an error if its config is invalid.
func New(p Params) (app.Automation, error) {
	cfg, err := loadConfig(p.Getenv)
	if err != nil {
		return app.Automation{}, err
	}
	if cfg.workDir == "" {
		// A dir of its own rather than the temp dir: the child may not read
		// the work dir, and the sandbox needs the temp dir.
		cfg.workDir = workspace.DefaultDir()
	}
	// Each run checks the work dir again; checking one that exists now
	// fails startup rather than every run.
	if _, err := os.Lstat(cfg.workDir); !errors.Is(err, fs.ErrNotExist) {
		if err := workspace.CheckDir(cfg.workDir); err != nil {
			return app.Automation{}, fmt.Errorf("%s: %w", workDirEnvVar, err)
		}
	}
	cloner, err := newCloner(p, cfg)
	if err != nil {
		return app.Automation{}, err
	}
	return app.Automation{
		Handler: func(ctx context.Context, store *eventstore.Store) eventhandler.Handler {
			return &investigator{
				ctx:         ctx,
				store:       store,
				logger:      p.Logger,
				staleBefore: p.Now().Add(-cfg.maxAge),
				repos:       cfg.repos,
				cloner:      cloner,
				prs:         githubPROpener{&github.Client{APIURL: cfg.apiURL, Token: cfg.githubToken, HTTP: p.HTTPClient}},
			}
		},
	}, nil
}

// newCloner returns what runs the investigations' Claude step: a runner
// through p.Runner, or a sandboxed child process of underling's.
func newCloner(p Params, cfg config) (cloner, error) {
	if p.Runner != nil {
		// The runner gets its own AWS credentials and has no sandbox, so
		// neither underling's credentials nor the read paths matter here.
		if cfg.bedrock == nil {
			return nil, fmt.Errorf("%s: must be \"1\" to run investigations in a runner", useBedrockEnvVar)
		}
		return &remoteCloner{cfg: cfg, launcher: p.Runner, logger: p.Logger}, nil
	}
	if err := resolveReadPaths(&cfg); err != nil {
		return nil, err
	}
	var credentials aws.CredentialsProvider
	if cfg.bedrock != nil {
		var err error
		if credentials, err = bedrock.NewCredentialsCache(context.Background(), p.AWS, cfg.bedrock.region, cfg.timeout); err != nil {
			return nil, err
		}
	}
	for _, d := range deniedPathDirs(cfg) {
		p.Logger.Warn("PATH entry lies under a path the child may not read; tools there won't run inside the sandbox",
			"handler", handlerName, "dir", d.dir, "denied", d.denied)
	}
	return &workspaceCloner{cfg: cfg, aws: credentials, logger: p.Logger}, nil
}
