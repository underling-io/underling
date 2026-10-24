package sentry

import "errors"

// SecretEnvVar names the environment variable holding the client secret.
const SecretEnvVar = "SENTRY_CLIENT_SECRET"

// SecretFromEnv reads the internal integration's client secret through
// getenv. It fails if the variable is missing or empty.
func SecretFromEnv(getenv func(string) string) (string, error) {
	secret := getenv(SecretEnvVar)
	if secret == "" {
		return "", errors.New(SecretEnvVar + " must be set")
	}
	return secret, nil
}
