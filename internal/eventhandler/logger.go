package eventhandler

import (
	"log/slog"

	"heinzel/internal/eventstore"
)

// EventLogger is the placeholder handler: it logs every event it sees.
type EventLogger struct {
	logger *slog.Logger
}

// NewEventLogger returns an EventLogger writing to logger.
func NewEventLogger(logger *slog.Logger) *EventLogger {
	return &EventLogger{logger: logger}
}

// Name implements Handler.
func (*EventLogger) Name() string { return "event-logger" }

// Handle implements Handler.
func (h *EventLogger) Handle(e eventstore.Event) {
	h.logger.Info("event handled",
		"handler", h.Name(),
		"position", e.Position,
		"streamId", e.StreamID,
		"version", e.Version,
		"type", e.Type,
	)
}
