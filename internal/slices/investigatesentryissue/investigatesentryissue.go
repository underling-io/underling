// Package investigatesentryissue is an automation slice:
//
//	SentryIssueAlertReceived → sentry-issue-investigator →
//	  SentryIssueInvestigationStarted, then PullRequestOpened, NoChanges or Failed
//	  (or SentryIssueInvestigationSkipped)
//
// For each new Sentry issue it clones the GitHub repo mapped to the issue's
// project, runs headless Claude Code in a sandboxed child process to
// investigate and fix the issue, then pushes the branch and opens a draft PR
// itself. Claude never sees the GitHub token. It loads its config from the
// environment; see config.go.
//
// investigator.go holds the whole decision path, from alert to recorded
// outcome. It reaches git, Claude and GitHub only through small interfaces,
// which infrastructure.go implements with the sub-packages: workspace (the
// clone and git commands), claude (the sandboxed child) and github (the
// draft PR).
package investigatesentryissue

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"heinzel/internal/app"
	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
	"heinzel/internal/slices/investigatesentryissue/github"
	"heinzel/internal/slices/investigatesentryissue/workspace"
)

const (
	defaultGitHubAPIURL   = "https://api.github.com"
	defaultGitHubCloneURL = "https://github.com/"
	githubRequestTimeout  = 30 * time.Second
)

func init() {
	app.RegisterAutomation(func(d app.Deps) (app.Automation, error) {
		return New(Params{
			Getenv:         d.Getenv,
			Logger:         d.Logger,
			GitHubAPIURL:   defaultGitHubAPIURL,
			GitHubCloneURL: defaultGitHubCloneURL,
			HTTPClient:     &http.Client{Timeout: githubRequestTimeout},
			Now:            time.Now,
		})
	})
}

// Params is what New needs.
type Params struct {
	// Getenv looks up the slice's config, like os.Getenv.
	Getenv func(string) string
	Logger *slog.Logger
	// GitHubAPIURL is the GitHub REST API base, e.g. https://api.github.com.
	GitHubAPIURL string
	// GitHubCloneURL is the clone URL base: a repo is cloned from
	// <GitHubCloneURL><owner>/<repo>.git, e.g. https://github.com/.
	GitHubCloneURL string
	// HTTPClient sends the GitHub API requests.
	HTTPClient *http.Client
	// Now is the clock read once when the handler is built: alerts stored
	// more than the max age before that are stale.
	Now func() time.Time
}

// New returns the slice. It loads its config through p.Getenv and fails if
// any value is invalid.
func New(p Params) (app.Automation, error) {
	cfg, err := loadConfig(p.Getenv)
	if err != nil {
		return app.Automation{}, err
	}
	if cfg.workDir == "" {
		// A dir of its own, not the temp dir itself: the child is denied
		// reads of the whole work dir, and the sandbox needs the temp dir.
		cfg.workDir = workspace.DefaultDir()
	}
	// Each run checks the work dir again, since it may change or appear
	// later; checking an existing one now fails startup instead of every
	// alert.
	if _, err := os.Lstat(cfg.workDir); !os.IsNotExist(err) {
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
				cloner:      &workspaceCloner{cfg: cfg, cloneBase: p.GitHubCloneURL, logger: p.Logger},
				prs:         githubPROpener{client: &github.Client{APIURL: p.GitHubAPIURL, Token: cfg.githubToken, HTTP: p.HTTPClient}},
			}
		},
	}, nil
}
