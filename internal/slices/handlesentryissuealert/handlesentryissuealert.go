// Package handlesentryissuealert is an automation slice:
//
//	SentryIssueAlertReceived → sentry-issue-alert-logger
//
// For now its reaction is one structured log line per alert, a placeholder
// until the real reaction is defined.
package handlesentryissuealert

import (
	"log/slog"
	"net/http"

	"heinzel/internal/agent"
	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
)

// New returns the slice. It registers no routes and runs one event handler.
func New(logger *slog.Logger) agent.Slice {
	return func(*http.ServeMux, *eventstore.Store) []eventhandler.Handler {
		return []eventhandler.Handler{&alertLogger{logger: logger}}
	}
}
