package investigatesentryissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"heinzel/internal/events"
	"heinzel/internal/eventstore"
)

// handlerName is the checkpoint key; never change it.
const handlerName = "sentry-issue-investigator"

// investigator investigates each new Sentry issue once.
type investigator struct {
	ctx    context.Context // cancelled when heinzel shuts down
	cfg    config
	store  *eventstore.Store
	logger *slog.Logger
	// staleBefore is the handler's start time minus the max age. Alerts
	// stored before it are past alerts, never investigated; alerts stored
	// later are live, however long the backlog delays them.
	staleBefore time.Time
	cloneBase   string
	github      *github
}

// Name implements eventhandler.Handler.
func (*investigator) Name() string { return handlerName }

// Handle implements eventhandler.Handler. It ignores other event types and,
// since it cannot fail, logs and skips payloads it cannot decode. Every
// investigation ends in a recorded outcome; failures never escape.
func (h *investigator) Handle(e eventstore.Event) {
	if e.Type != events.SentryIssueAlertReceivedType {
		return
	}
	var alert events.SentryIssueAlertReceived
	if err := json.Unmarshal(e.Payload, &alert); err != nil {
		h.logger.Error("decode sentry issue alert", "handler", handlerName, "position", e.Position, "error", err)
		return
	}
	log := h.logger.With("handler", handlerName, "position", e.Position, "issueId", alert.IssueID)
	stream := streamID(alert.IssueID)

	if e.Timestamp.Before(h.staleBefore) {
		h.recordFirst(log, stream, skippedOutcome(alert.IssueID, events.InvestigationSkippedStale))
		return
	}
	target, mapped := h.cfg.repos[alert.Project]
	if !mapped {
		h.recordFirst(log, stream, skippedOutcome(alert.IssueID, events.InvestigationSkippedUnmapped))
		return
	}

	branch := branchName(alert.IssueID)
	started, err := json.Marshal(events.SentryIssueInvestigationStarted{IssueID: alert.IssueID, Project: alert.Project, Repo: target.String(), Branch: branch})
	if err != nil {
		log.Error("encode investigation started", "error", err)
		return
	}
	if h.ctx.Err() != nil {
		// Record nothing: a Started now would only end in Failed shutdown,
		// which is final. Note that the runner still checkpoints past the
		// alert, so only a later alert for the issue starts it.
		log.Info("heinzel is shutting down; sentry issue alert left unhandled")
		return
	}
	if !h.append(log, stream, 0, eventstore.NewEvent{Type: events.SentryIssueInvestigationStartedType, Payload: started}) {
		return
	}
	log.Info("sentry issue investigation started", "repo", target.String(), "branch", branch)
	h.record(log, stream, 1, h.investigate(alert, target, branch))
}

// investigate clones target, lets Claude work on branch, and pushes and
// opens a draft PR if it committed anything.
func (h *investigator) investigate(alert events.SentryIssueAlertReceived, target repo, branch string) outcome {
	ctx := h.ctx
	fail := func(stage string, err error) outcome {
		if ctx.Err() != nil {
			stage = events.InvestigationFailedShutdown
		}
		return failedOutcome(alert.IssueID, &failed{stage: stage, reason: err.Error()})
	}

	ws, err := newWorkspace(ctx, h.cfg.workDir, h.cloneBase+target.owner+"/"+target.name+".git", branch, h.cfg)
	if err != nil {
		return fail(events.InvestigationFailedClone, err)
	}
	defer func() {
		if err := ws.remove(); err != nil {
			h.logger.Error("remove investigation workspace", "handler", handlerName, "issueId", alert.IssueID, "dir", ws.runDir, "error", err)
		}
	}()

	cmd, err := claudeCommand(h.cfg, *ws, investigationPrompt(alert, branch))
	if err != nil {
		return fail(events.InvestigationFailedClaude, err)
	}
	summary, failure := runClaude(ctx, cmd, h.cfg.timeout)
	if failure != nil {
		return failedOutcome(alert.IssueID, failure)
	}

	stray, err := ws.strayBranches(ctx)
	if err != nil {
		return fail(events.InvestigationFailedClaude, fmt.Errorf("collect claude's commits: %w", err))
	}
	if len(stray) > 0 {
		return fail(events.InvestigationFailedClaude, fmt.Errorf("claude committed on branches other than %s, which heinzel doesn't push: %s", branch, strings.Join(stray, ", ")))
	}
	commits, err := ws.newCommits(ctx)
	if err != nil {
		return fail(events.InvestigationFailedClaude, fmt.Errorf("collect claude's commits: %w", err))
	}
	if commits == 0 {
		return noChangesOutcome(alert.IssueID)
	}
	changed, err := ws.changedPaths(ctx)
	if err != nil {
		return fail(events.InvestigationFailedPush, err)
	}
	if ci := slices.DeleteFunc(changed, func(p string) bool { return !ciConfigPath(p) }); len(ci) > 0 {
		return fail(events.InvestigationFailedPush, fmt.Errorf("changes CI configuration, so heinzel won't push it: %s", strings.Join(firstN(ci, maxListedPaths), ", ")))
	}
	if err := ws.push(ctx); err != nil {
		return fail(events.InvestigationFailedPush, err)
	}
	url, err := h.github.openDraftPR(ctx, target, pullRequest{
		Title: prTitle(alert),
		Head:  branch,
		Base:  ws.defaultBranch,
		Body:  prBody(alert, summary),
	})
	if err != nil {
		return fail(events.InvestigationFailedPR, err)
	}
	return prOpenedOutcome(alert.IssueID, url)
}

// maxListedPaths caps how many paths a failure reason lists.
const maxListedPaths = 5

// firstN returns at most the first n items, noting how many it left out.
func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(slices.Clone(items[:n]), fmt.Sprintf("and %d more", len(items)-n))
}

// GitHub limits; the body cap leaves headroom below GitHub's 65536.
const (
	maxPRTitleRunes = 200
	maxPRBodyBytes  = 60000
)

// prTitle names the Sentry issue. Alert text can't @mention anyone.
func prTitle(alert events.SentryIssueAlertReceived) string {
	title := fmt.Sprintf("Fix Sentry issue %s: %s", alert.IssueID, alert.Title)
	return truncateRunes(neutraliseMentions(title), maxPRTitleRunes)
}

// prBody links the Sentry issue and carries Claude's summary. Neither the
// alert's text nor Claude's can @mention anyone. Only the summary is cut to
// keep the body within maxPRBodyBytes, so the footer always survives.
func prBody(alert events.SentryIssueAlertReceived, summary string) string {
	header := fmt.Sprintf("Sentry issue [%s](%s): %s\n\n## Claude's summary\n\n", neutraliseMentions(alert.IssueID), alert.WebURL, neutraliseMentions(alert.Title))
	header = truncateBytes(header, maxPRBodyBytes-len(prFooter))
	summary = truncateSummary(neutraliseMentions(strings.TrimSpace(summary)), maxPRBodyBytes-len(header)-len(prFooter))
	return header + summary + prFooter
}

// prFooter ends every PR body.
const prFooter = "\n\n---\nOpened by heinzel. Claude Code investigated the issue in a sandbox; review before merging.\n"

// truncateSummary cuts s to at most n bytes at a rune boundary. If the cut
// leaves a code fence open, it closes it with the fence's own characters,
// within the n bytes.
func truncateSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := truncateBytes(s, n)
	for reserve := 0; ; {
		fence := openFence(cut)
		if fence == "" {
			return cut
		}
		closing := "\n" + fence
		if len(closing) <= reserve {
			return cut + closing
		}
		// Make room for the closing fence and look again: the shorter cut
		// may end in another fence, or in none.
		reserve = len(closing)
		cut = truncateBytes(s, max(n-reserve, 0))
	}
}

// openFence returns the opening run of the Markdown code fence (``` or ~~~,
// possibly longer) that s ends inside, or "" if s ends outside any fence. As
// in CommonMark, only a run of the same character, at least as long and
// with nothing after it, closes a fence.
func openFence(s string) string {
	open := ""
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimLeft(line, " ")
		run := fenceRun(line)
		switch {
		case run == "":
		case open == "":
			open = run
		case run[0] == open[0] && len(run) >= len(open) && strings.TrimSpace(line[len(run):]) == "":
			open = ""
		}
	}
	return open
}

// fenceRun returns line's leading run of three or more backticks or tildes,
// or "".
func fenceRun(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := len(line) - len(strings.TrimLeft(line, line[:1]))
	if n < 3 {
		return ""
	}
	return line[:n]
}

// recordFirst records o as the stream's first event; a conflict means the
// issue was already handled.
func (h *investigator) recordFirst(log *slog.Logger, stream string, o outcome) {
	h.record(log, stream, 0, o)
}

// record appends o at expectedVersion and logs the outcome line.
func (h *investigator) record(log *slog.Logger, stream string, expectedVersion uint64, o outcome) {
	e, err := o.event()
	if err != nil {
		log.Error("encode investigation outcome", "error", err)
		return
	}
	if !h.append(log, stream, expectedVersion, e) {
		return
	}
	log.Log(context.Background(), o.level, "sentry issue investigation finished", append([]any{"outcome", o.name}, o.attrs...)...)
}

// append appends e and reports whether it was stored. A version conflict
// means another alert for the issue got there first.
func (h *investigator) append(log *slog.Logger, stream string, expectedVersion uint64, e eventstore.NewEvent) bool {
	_, _, err := h.store.Append(stream, expectedVersion, []eventstore.NewEvent{e})
	switch {
	case errors.Is(err, eventstore.ErrConflict):
		log.Info("sentry issue already handled", "outcome", "duplicate")
		return false
	case err != nil:
		log.Error("record sentry issue investigation", "type", e.Type, "error", err)
		return false
	}
	return true
}
