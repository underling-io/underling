// Package handlesentryissuealert is an automation slice:
//
//	SentryIssueAlertReceived → sentry-issue-alert-logger
//
// For now its reaction is one structured log line per alert, a placeholder
// until the real reaction is defined.
package handlesentryissuealert

import (
	"log/slog"

	"heinzel/internal/app"
)

func init() {
	app.RegisterAutomation(func(d app.Deps) (app.Automation, error) {
		return New(d.Logger), nil
	})
}

// New returns the slice, which logs each alert to logger.
func New(logger *slog.Logger) app.Automation {
	return &alertLogger{logger: logger}
}
