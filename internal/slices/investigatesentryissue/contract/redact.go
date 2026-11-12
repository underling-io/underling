package contract

import (
	"slices"
	"strings"
)

// Redacted is what Redact puts in a secret's place.
const Redacted = "[redacted]"

// Secrets returns what no log line, event or pull request may show of a
// run's URLs: each URL and each one's query, which is a credential when the
// URL is presigned. The longest comes first, so that Redact never leaves
// the rest of a longer secret behind. Empty URLs are skipped.
func Secrets(urls ...string) []string {
	var secrets []string
	for _, u := range urls {
		if u == "" {
			continue
		}
		secrets = append(secrets, u)
		if _, query, ok := strings.Cut(u, "?"); ok && query != "" {
			secrets = append(secrets, query)
		}
	}
	slices.SortStableFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(secrets)
}

// Redact returns text with each of secrets, as Secrets returned them,
// replaced by Redacted.
func Redact(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, Redacted)
	}
	return text
}
