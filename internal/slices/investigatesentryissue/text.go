package investigatesentryissue

import (
	"strings"
	"unicode/utf8"
)

// truncateBytes returns s cut to at most n bytes, at a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// neutraliseMentions puts a zero-width space after every "@", so text that
// came from an alert or from Claude can't @mention anyone on GitHub.
func neutraliseMentions(s string) string {
	return strings.ReplaceAll(s, "@", "@​")
}

// truncateRunes returns s cut to at most n runes.
func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
