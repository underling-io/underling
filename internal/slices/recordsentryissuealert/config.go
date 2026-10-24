package recordsentryissuealert

import "errors"

// secretEnvVar names the environment variable holding the client secret.
const secretEnvVar = "SENTRY_CLIENT_SECRET"

// secretFromEnv reads the internal integration's client secret through
// getenv. It fails if the variable is missing or empty.
func secretFromEnv(getenv func(string) string) (string, error) {
	secret := getenv(secretEnvVar)
	if secret == "" {
		return "", errors.New(secretEnvVar + " must be set")
	}
	return secret, nil
}
