package sentry_test

import (
	"testing"

	"heinzel/internal/features/sentry"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestSecretFromEnvReadsClientSecret(t *testing.T) {
	got, err := sentry.SecretFromEnv(env(map[string]string{"SENTRY_CLIENT_SECRET": "s3cret"}))
	if err != nil || got != "s3cret" {
		t.Fatalf("got %q err %v, want s3cret", got, err)
	}
}

func TestSecretFromEnvRejectsMissingOrEmptySecret(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"missing": {},
		"empty":   {"SENTRY_CLIENT_SECRET": ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sentry.SecretFromEnv(env(vars)); err == nil {
				t.Fatal("got no error, want one")
			}
		})
	}
}
