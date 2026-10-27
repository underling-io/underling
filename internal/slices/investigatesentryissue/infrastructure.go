package investigatesentryissue

import (
	"context"
	"log/slog"

	"heinzel/internal/slices/investigatesentryissue/claude"
	"heinzel/internal/slices/investigatesentryissue/github"
	"heinzel/internal/slices/investigatesentryissue/workspace"
)

// workspaceCloner gives each investigation a workspace of its own, where
// Claude runs in a sandboxed child process.
type workspaceCloner struct {
	cfg       config
	cloneBase string // repos are cloned from <cloneBase><owner>/<repo>.git
	logger    *slog.Logger
}

func (c *workspaceCloner) clone(ctx context.Context, issueID string, target repo, branch string) (run, error) {
	url := c.cloneBase + target.owner + "/" + target.name + ".git"
	ws, err := workspace.New(ctx, c.cfg.workDir, url, branch, c.cfg.path, c.cfg.githubToken)
	if err != nil {
		return nil, err
	}
	return &workspaceRun{ws: ws, cfg: c.cfg, issueID: issueID, logger: c.logger}, nil
}

// workspaceRun is an investigation in a workspace.
type workspaceRun struct {
	ws      *workspace.Workspace
	cfg     config
	issueID string
	logger  *slog.Logger
}

func (r *workspaceRun) claude(ctx context.Context, prompt string) (string, error) {
	cmd, err := claude.Command(claudeConfig(r.cfg), claudeDirs(r.ws), prompt)
	if err != nil {
		return "", err
	}
	return claude.Run(ctx, cmd, r.cfg.timeout)
}

func (r *workspaceRun) strayBranches(ctx context.Context) ([]string, error) {
	return r.ws.StrayBranches(ctx)
}

func (r *workspaceRun) newCommits(ctx context.Context) (int, error) { return r.ws.NewCommits(ctx) }

func (r *workspaceRun) changedPaths(ctx context.Context) ([]string, error) {
	return r.ws.ChangedPaths(ctx)
}

func (r *workspaceRun) push(ctx context.Context) error { return r.ws.Push(ctx) }

func (r *workspaceRun) defaultBranch() string { return r.ws.DefaultBranch }

func (r *workspaceRun) remove() {
	if err := r.ws.Remove(); err != nil {
		r.logger.Error("remove investigation workspace", "handler", handlerName, "issueId", r.issueID, "dir", r.ws.RunDir, "error", err)
	}
}

func claudeConfig(cfg config) claude.Config {
	return claude.Config{
		Bin:          cfg.claudeBin,
		PluginDirs:   cfg.pluginDirs,
		Path:         cfg.path,
		AnthropicKey: cfg.anthropicKey,
		OpenAIKey:    cfg.openAIKey,
		ParentHome:   cfg.parentHome,
		DenyRead:     cfg.denyRead,
		Readable:     cfg.readable,
	}
}

func claudeDirs(ws *workspace.Workspace) claude.Dirs {
	return claude.Dirs{
		WorkDir:      ws.WorkDir,
		Siblings:     ws.Siblings,
		Clone:        ws.Clone,
		Home:         ws.Home,
		ClaudeConfig: ws.ClaudeConfig,
		Mirror:       ws.Mirror,
		GitHome:      ws.GitHome,
	}
}

// githubPROpener opens draft pull requests through the GitHub REST API.
type githubPROpener struct {
	client *github.Client
}

func (g githubPROpener) openDraft(ctx context.Context, target repo, pr pullRequest) (string, error) {
	return g.client.OpenDraftPR(ctx, target.owner, target.name, github.PullRequest{Title: pr.title, Head: pr.head, Base: pr.base, Body: pr.body})
}
