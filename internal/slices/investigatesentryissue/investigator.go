package investigatesentryissue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"underling/internal/events"
	"underling/internal/platform/eventstore"
)

// handlerName is the handler's checkpoint key; never change it.
const handlerName = "sentry-issue-investigator"

// investigator investigates each new Sentry issue once.
type investigator struct {
	ctx    context.Context // cancelled when underling shuts down
	store  *eventstore.Store
	logger *slog.Logger
	// staleBefore is the handler's start minus the max age. Alerts stored
	// before it are past alerts; alerts stored later are live, however long
	// a backlog delays them.
	staleBefore time.Time
	repos       map[int64]repo // by Sentry project id
	cloner      cloner
	prs         prOpener
}

// cloner gets each investigation a fresh clone to work in.
type cloner interface {
	clone(ctx context.Context, issueID string, target repo, branch string) (run, error)
}

// run is one investigation's clone, and Claude working in it.
type run interface {
	// claude runs Claude with prompt and returns its summary. If Claude ran
	// out of time, timedOut(err) holds.
	claude(ctx context.Context, prompt string) (summary string, err error)
	// strayBranches lists the branches besides the investigation branch
	// that hold commits underling won't push.
	strayBranches(ctx context.Context) ([]string, error)
	// newCommits counts the investigation branch's new commits. It runs
	// after strayBranches.
	newCommits(ctx context.Context) (int, error)
	// changedPaths lists the paths those commits change.
	changedPaths(ctx context.Context) ([]string, error)
	push(ctx context.Context) error
	// redact returns text without what the run keeps secret. Whatever of
	// the run's the investigator stores, logs or publishes goes through
	// it: the run's errors, and anything read out of the clone, whose
	// branch and path names Claude chose.
	redact(text string) string
	// defaultBranch is the branch the PR targets.
	defaultBranch() string
	// remove deletes the clone, logging any failure.
	remove()
}

// prOpener opens draft pull requests.
type prOpener interface {
	openDraft(ctx context.Context, target repo, pr pullRequest) (url string, err error)
}

type pullRequest struct {
	title, head, base, body string
}

// Name implements eventhandler.Handler.
func (*investigator) Name() string { return handlerName }

// Handle implements eventhandler.Handler. It ignores other events, and
// logs and skips an alert it can't decode. Every investigation it starts
// ends in a recorded outcome.
func (h *investigator) Handle(e eventstore.Event) {
	if e.Type != events.SentryIssueAlertReceivedType {
		return
	}
	decoded, err := events.Decode(e)
	if err != nil {
		h.logger.Error("decode sentry issue alert", "handler", handlerName, "position", e.Position, "error", err)
		return
	}
	alert := decoded.(events.SentryIssueAlertReceived)
	log := h.logger.With("handler", handlerName, "position", e.Position, "issueId", alert.IssueID)
	stream := streamID(alert.IssueID)

	if e.Timestamp.Before(h.staleBefore) {
		h.record(log, stream, 0, skippedOutcome(alert.IssueID, events.InvestigationSkippedStale))
		return
	}
	target, mapped := h.repos[alert.Project]
	if !mapped {
		h.record(log, stream, 0, skippedOutcome(alert.IssueID, events.InvestigationSkippedUnmapped))
		return
	}
	if h.ctx.Err() != nil {
		// A Started now could only end as Failed shutdown, which is final.
		// The runner still checkpoints past the alert, so only a later
		// alert for the issue starts it.
		log.Info("underling is shutting down; sentry issue alert left unhandled")
		return
	}
	branch := branchName(alert.IssueID)
	started := events.Encode(events.SentryIssueInvestigationStarted{
		IssueID: alert.IssueID, Project: alert.Project, Repo: target.String(), Branch: branch,
	})
	if !h.append(log, stream, 0, started) {
		return
	}
	log.Info("sentry issue investigation started", "repo", target.String(), "branch", branch)
	h.record(log, stream, 1, h.investigate(alert, target, branch))
}

// investigate clones target, lets Claude work on branch, and pushes it and
// opens a draft PR if Claude committed a fix. Once underling shuts down, any
// failure is a shutdown failure. No failure reason and no PR shows what the
// run keeps secret (run.redact).
func (h *investigator) investigate(alert events.SentryIssueAlertReceived, target repo, branch string) outcome {
	ctx := h.ctx
	redact := func(text string) string { return text } // until there is a run
	fail := func(stage string, err error) outcome {
		if ctx.Err() != nil {
			stage = events.InvestigationFailedShutdown
		}
		return failedOutcome(alert.IssueID, stage, redact(err.Error()))
	}

	run, err := h.cloner.clone(ctx, alert.IssueID, target, branch)
	if err != nil {
		return fail(events.InvestigationFailedClone, err)
	}
	defer run.remove()
	redact = run.redact

	summary, err := run.claude(ctx, investigationPrompt(alert, branch))
	if timedOut(err) {
		return fail(events.InvestigationFailedTimeout, err)
	}
	if err != nil {
		return fail(events.InvestigationFailedClaude, err)
	}
	// Before the PR body cuts the summary, which could cut a secret in two.
	summary = redact(summary)

	stray, err := run.strayBranches(ctx)
	if err != nil {
		return fail(events.InvestigationFailedClaude, fmt.Errorf("collect claude's commits: %w", err))
	}
	if len(stray) > 0 {
		return fail(events.InvestigationFailedClaude, fmt.Errorf("claude committed on branches other than %s, which underling doesn't push: %s", branch, strings.Join(stray, ", ")))
	}
	commits, err := run.newCommits(ctx)
	if err != nil {
		return fail(events.InvestigationFailedClaude, fmt.Errorf("collect claude's commits: %w", err))
	}
	if commits == 0 {
		return noChangesOutcome(alert.IssueID)
	}

	changed, err := run.changedPaths(ctx)
	if err != nil {
		return fail(events.InvestigationFailedPush, err)
	}
	if ci := ciConfigPaths(changed); len(ci) > 0 {
		return fail(events.InvestigationFailedPush, fmt.Errorf("changes CI configuration, so underling won't push it: %s", strings.Join(firstN(ci, maxListedPaths), ", ")))
	}
	if err := run.push(ctx); err != nil {
		return fail(events.InvestigationFailedPush, err)
	}

	url, err := h.prs.openDraft(ctx, target, pullRequest{
		title: redact(prTitle(alert)),
		head:  branch,
		base:  run.defaultBranch(),
		body:  redact(prBody(alert, summary)),
	})
	if err != nil {
		return fail(events.InvestigationFailedPR, err)
	}
	return prOpenedOutcome(alert.IssueID, url)
}

// timedOut reports whether err says Claude ran out of time: it has a
// Timeout method reporting true, as net and os timeout errors do.
func timedOut(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}
