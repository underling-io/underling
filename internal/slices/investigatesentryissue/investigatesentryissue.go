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

	"heinzel/internal/platform/app"
	"heinzel/internal/platform/eventhandler"
	"heinzel/internal/platform/eventstore"
	"heinzel/internal/slices/investigatesentryissue/github"
	"heinzel/internal/slices/investigatesentryissue/workspace"
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
	if err := resolveReadPaths(&cfg); err != nil {
		return app.Automation{}, err
	}
	for _, d := range deniedPathDirs(cfg) {
		p.Logger.Warn("PATH entry lies under a path the child may not read; tools there won't run inside the sandbox",
			"handler", handlerName, "dir", d.dir, "denied", d.denied)
	}
	return app.Automation{
		Handler: func(ctx context.Context, store *eventstore.Store) eventhandler.Handler {
			return &investigator{
				ctx:         ctx,
				store:       store,
				logger:      p.Logger,
				staleBefore: p.Now().Add(-cfg.maxAge),
				repos:       cfg.repos,
				cloner:      &workspaceCloner{cfg: cfg, logger: p.Logger},
				prs:         githubPROpener{&github.Client{APIURL: cfg.apiURL, Token: cfg.githubToken, HTTP: p.HTTPClient}},
			}
		},
	}, nil
}
