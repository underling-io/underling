// Package recordsentryissuealert is a state-change slice:
//
//	POST /sentry/callback → "Record Sentry issue alert" → SentryIssueAlertReceived
//
// It verifies a Sentry internal-integration webhook and records each issue
// alert. It loads the integration's client secret from SENTRY_CLIENT_SECRET.
package recordsentryissuealert

import (
	"log/slog"
	"net/http"

	"heinzel/internal/app"
	"heinzel/internal/eventstore"
)

func init() {
	app.RegisterStateChange(func(d app.Deps) (app.StateChange, error) {
		return New(d.Getenv, d.Logger)
	})
}

// New returns the slice, serving POST /sentry/callback. It reads the internal
// integration's client secret, used to verify webhook signatures, from
// SENTRY_CLIENT_SECRET through getenv, and fails if it is missing or empty.
func New(getenv func(string) string, logger *slog.Logger) (app.StateChange, error) {
	secret, err := secretFromEnv(getenv)
	if err != nil {
		return app.StateChange{}, err
	}
	return app.StateChange{
		Pattern: "POST /sentry/callback",
		Handler: func(store *eventstore.Store) http.Handler {
			return &callback{secret: []byte(secret), store: store, logger: logger}
		},
	}, nil
}
