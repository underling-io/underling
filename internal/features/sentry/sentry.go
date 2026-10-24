// Package sentry is the Sentry feature. It has two slices:
//
//   - state change: POST /sentry/callback verifies an internal-integration
//     webhook and records each issue alert as SentryIssueAlertReceived;
//   - automation: the sentry-issue-alert-logger handler logs each recorded alert.
package sentry

import (
	"log/slog"
	"net/http"

	"heinzel/internal/agent"
	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
)

// New returns the Sentry feature. secret is the internal integration's client
// secret, used to verify webhook signatures.
func New(secret string, logger *slog.Logger) agent.Feature {
	return func(mux *http.ServeMux, store *eventstore.Store) []eventhandler.Handler {
		mux.Handle("POST /sentry/callback", &callback{secret: []byte(secret), store: store, logger: logger})
		return []eventhandler.Handler{&alertLogger{logger: logger}}
	}
}
