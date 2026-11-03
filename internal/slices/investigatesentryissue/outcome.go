package investigatesentryissue

import (
	"context"
	"errors"
	"log/slog"

	"underling/internal/events"
	"underling/internal/platform/eventstore"
)

// outcome is how an investigation (or a skip) ended: the event to record
// and how to log it.
type outcome struct {
	event eventstore.NewEvent
	name  string // pr_opened, no_changes, skipped or failed
	level slog.Level
	attrs []any
}

func skippedOutcome(issueID, reason string) outcome {
	return outcome{
		event: events.Encode(events.SentryIssueInvestigationSkipped{IssueID: issueID, Reason: reason}),
		name:  "skipped",
		level: slog.LevelInfo,
		attrs: []any{"reason", reason},
	}
}

func failedOutcome(issueID, stage, reason string) outcome {
	return outcome{
		event: events.Encode(events.SentryIssueInvestigationFailed{IssueID: issueID, Stage: stage, Reason: reason}),
		name:  "failed",
		level: slog.LevelWarn,
		attrs: []any{"stage", stage, "reason", reason},
	}
}

func noChangesOutcome(issueID string) outcome {
	return outcome{
		event: events.Encode(events.SentryIssueInvestigationNoChanges{IssueID: issueID}),
		name:  "no_changes",
		level: slog.LevelInfo,
	}
}

func prOpenedOutcome(issueID, url string) outcome {
	return outcome{
		event: events.Encode(events.SentryIssueInvestigationPullRequestOpened{IssueID: issueID, PRURL: url}),
		name:  "pr_opened",
		level: slog.LevelInfo,
		attrs: []any{"prUrl", url},
	}
}

// record appends o's event at expectedVersion and logs the outcome.
func (h *investigator) record(log *slog.Logger, stream string, expectedVersion uint64, o outcome) {
	if !h.append(log, stream, expectedVersion, o.event) {
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
