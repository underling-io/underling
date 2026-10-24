package handlesentryissuealert

import (
	"encoding/json"
	"log/slog"

	"heinzel/internal/events"
	"heinzel/internal/eventstore"
)

// alertLogger logs every recorded issue alert.
type alertLogger struct {
	logger *slog.Logger
}

// Name implements eventhandler.Handler. It is the checkpoint key; never change it.
func (*alertLogger) Name() string { return "sentry-issue-alert-logger" }

// Handle implements eventhandler.Handler. It ignores other event types and,
// since it cannot fail, logs and skips payloads it cannot decode.
func (h *alertLogger) Handle(e eventstore.Event) {
	if e.Type != events.SentryIssueAlertReceivedType {
		return
	}
	var alert events.SentryIssueAlertReceived
	if err := json.Unmarshal(e.Payload, &alert); err != nil {
		h.logger.Error("decode sentry issue alert", "handler", h.Name(), "position", e.Position, "error", err)
		return
	}
	h.logger.Info("sentry issue alert received",
		"handler", h.Name(),
		"position", e.Position,
		"issueId", alert.IssueID,
		"title", alert.Title,
		"sentryLevel", alert.Level,
	)
}
