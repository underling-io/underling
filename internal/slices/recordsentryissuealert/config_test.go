package recordsentryissuealert_test

import (
	"log/slog"
	"strings"
	"testing"

	"underling/internal/slices/recordsentryissuealert"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestNewAcceptsClientSecret(t *testing.T) {
	_, err := recordsentryissuealert.New(env(map[string]string{"SENTRY_CLIENT_SECRET": "s3cret"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("got error %v, want none", err)
	}
}

func TestNewRejectsMissingOrEmptySecret(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"missing": {},
		"empty":   {"SENTRY_CLIENT_SECRET": ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := recordsentryissuealert.New(env(vars), slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), "SENTRY_CLIENT_SECRET") {
				t.Fatalf("got error %v, want one naming SENTRY_CLIENT_SECRET", err)
			}
		})
	}
}
