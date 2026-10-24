// Package recordsentryissuealert is a state-change slice:
//
//	POST /sentry/callback → "Record Sentry issue alert" → SentryIssueAlertReceived
//
// It verifies a Sentry internal-integration webhook and records each issue
// alert. It also owns loading the integration's client secret.
package recordsentryissuealert

import (
	"log/slog"
	"net/http"

	"heinzel/internal/agent"
	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
)

// New returns the slice. secret is the internal integration's client secret,
// used to verify webhook signatures. The slice registers POST /sentry/callback
// and runs no event handlers.
func New(secret string, logger *slog.Logger) agent.Slice {
	return func(mux *http.ServeMux, store *eventstore.Store) []eventhandler.Handler {
		mux.Handle("POST /sentry/callback", &callback{secret: []byte(secret), store: store, logger: logger})
		return nil
	}
}
