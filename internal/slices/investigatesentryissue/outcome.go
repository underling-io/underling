package investigatesentryissue

import (
	"encoding/json"
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

// failed is why an investigation failed: one of the
// events.InvestigationFailed* stages and a human-readable reason.
type failed struct {
	stage  string
	reason string
}

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

func failedOutcome(issueID string, f *failed) outcome {
	return outcome{
		eventType: events.SentryIssueInvestigationFailedType,
		payload:   events.SentryIssueInvestigationFailed{IssueID: issueID, Stage: f.stage, Reason: f.reason},
		name:      outcomeFailed,
		level:     slog.LevelWarn,
		attrs:     []any{"stage", f.stage, "reason", f.reason},
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
