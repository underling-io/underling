// Package recordsentryissuealert is a state-change slice:
//
//	POST /sentry/callback → "Record Sentry issue alert" → SentryIssueAlertReceived
//
// It verifies a Sentry internal-integration webhook and records each issue
// alert once. It reads the integration's client secret from
// SENTRY_CLIENT_SECRET.
package recordsentryissuealert

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"underling/internal/platform/app"
	"underling/internal/platform/eventstore"
)

const secretEnvVar = "SENTRY_CLIENT_SECRET"

func init() {
	app.RegisterStateChange(func(d app.Deps) (app.StateChange, error) {
		return New(d.Getenv, d.Logger)
	})
}

// New returns the slice. It reads the client secret that signs the
// webhooks through getenv and fails if it is missing or empty.
func New(getenv func(string) string, logger *slog.Logger) (app.StateChange, error) {
	secret := getenv(secretEnvVar)
	if secret == "" {
		return app.StateChange{}, errors.New(secretEnvVar + " must be set")
	}
	return app.StateChange{
		Method: http.MethodPost,
		Path:   "/sentry/callback",
		Handler: func(store *eventstore.Store) gin.HandlerFunc {
			cb := &callback{secret: []byte(secret), store: store, logger: logger}
			return cb.handle
		},
	}, nil
}
