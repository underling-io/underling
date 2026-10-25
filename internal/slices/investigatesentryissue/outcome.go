package investigatesentryissue

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"heinzel/internal/events"
	"heinzel/internal/eventstore"
)

// Outcome names in the log line that ends each investigation.
const (
	outcomePROpened  = "pr_opened"
	outcomeNoChanges = "no_changes"
	outcomeSkipped   = "skipped"
	outcomeFailed    = "failed"
)

// outcome is how an investigation (or a skip) ended: the event to record
// and the log attributes describing it.
type outcome struct {
	eventType string
	payload   any
	name      string
	level     slog.Level
	attrs     []any
}

func skippedOutcome(issueID, reason string) outcome {
	return outcome{
		eventType: events.SentryIssueInvestigationSkippedType,
		payload:   events.SentryIssueInvestigationSkipped{IssueID: issueID, Reason: reason},
		name:      outcomeSkipped,
		level:     slog.LevelInfo,
		attrs:     []any{"reason", reason},
	}
}

// failedOutcome is a failure at stage, one of the events.InvestigationFailed*
// stages, with a human-readable reason.
func failedOutcome(issueID, stage, reason string) outcome {
	return outcome{
		eventType: events.SentryIssueInvestigationFailedType,
		payload:   events.SentryIssueInvestigationFailed{IssueID: issueID, Stage: stage, Reason: reason},
		name:      outcomeFailed,
		level:     slog.LevelWarn,
		attrs:     []any{"stage", stage, "reason", reason},
	}
}

func noChangesOutcome(issueID string) outcome {
	return outcome{
		eventType: events.SentryIssueInvestigationNoChangesType,
		payload:   events.SentryIssueInvestigationNoChanges{IssueID: issueID},
		name:      outcomeNoChanges,
		level:     slog.LevelInfo,
	}
}

func prOpenedOutcome(issueID, url string) outcome {
	return outcome{
		eventType: events.SentryIssueInvestigationPullRequestOpenedType,
		payload:   events.SentryIssueInvestigationPullRequestOpened{IssueID: issueID, PRURL: url},
		name:      outcomePROpened,
		level:     slog.LevelInfo,
		attrs:     []any{"prUrl", url},
	}
}

func (o outcome) event() (eventstore.NewEvent, error) {
	payload, err := json.Marshal(o.payload)
	if err != nil {
		return eventstore.NewEvent{}, err
	}
	return eventstore.NewEvent{Type: o.eventType, Payload: payload}, nil
}

// startedEvent is the event that starts an investigation of alert's issue in
// target, on branch.
func startedEvent(alert events.SentryIssueAlertReceived, target repo, branch string) (eventstore.NewEvent, error) {
	payload, err := json.Marshal(events.SentryIssueInvestigationStarted{IssueID: alert.IssueID, Project: alert.Project, Repo: target.String(), Branch: branch})
	if err != nil {
		return eventstore.NewEvent{}, err
	}
	return eventstore.NewEvent{Type: events.SentryIssueInvestigationStartedType, Payload: payload}, nil
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
