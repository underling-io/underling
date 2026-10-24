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
	"heinzel/internal/eventstore"
)

// New returns the slice, serving POST /sentry/callback. secret is the
// internal integration's client secret, used to verify webhook signatures.
func New(secret string, logger *slog.Logger) agent.StateChange {
	return agent.StateChange{
		Pattern: "POST /sentry/callback",
		Handler: func(store *eventstore.Store) http.Handler {
			return &callback{secret: []byte(secret), store: store, logger: logger}
		},
	}
}
