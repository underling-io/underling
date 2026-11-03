package investigatesentryissue

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"underling/internal/events"
)

// GitHub's limits; the body cap leaves headroom below GitHub's 65536.
const (
	maxPRTitleRunes = 200
	maxPRBodyBytes  = 60000
)

// prFooter ends every PR body.
const prFooter = "\n\n---\nOpened by underling. Claude Code investigated the issue in a sandbox; review before merging.\n"

// prTitle names the Sentry issue.
func prTitle(alert events.SentryIssueAlertReceived) string {
	title := fmt.Sprintf("Fix Sentry issue %s: %s", alert.IssueID, alert.Title)
	return truncateRunes(neutraliseMentions(title), maxPRTitleRunes)
}

// prBody links the Sentry issue and holds Claude's summary. Only the
// summary is cut to fit maxPRBodyBytes, so the footer always survives.
func prBody(alert events.SentryIssueAlertReceived, summary string) string {
	header := fmt.Sprintf("Sentry issue [%s](%s): %s\n\n## Claude's summary\n\n", neutraliseMentions(alert.IssueID), alert.WebURL, neutraliseMentions(alert.Title))
	header = truncateBytes(header, maxPRBodyBytes-len(prFooter))
	room := maxPRBodyBytes - len(header) - len(prFooter)
	return header + truncateSummary(neutraliseMentions(strings.TrimSpace(summary)), room) + prFooter
}

// truncateSummary cuts s to at most n bytes at a rune boundary. A code
// fence the cut leaves open is closed, within the n bytes, with the
// fence's own characters.
func truncateSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := truncateBytes(s, n)
	for reserved := 0; ; {
		fence := openFence(cut)
		if fence == "" {
			return cut
		}
		closing := "\n" + fence
		if len(closing) <= reserved {
			return cut + closing
		}
		// Cutting shorter to make room may end in another fence, or none,
		// so look again.
		reserved = len(closing)
		cut = truncateBytes(s, max(n-reserved, 0))
	}
}

// openFence returns the opening run of the Markdown code fence (``` or
// ~~~, maybe longer) that s ends inside, or "". As in CommonMark, only a
// run of the same character, at least as long and with nothing after it,
// closes a fence.
func openFence(s string) string {
	open := ""
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimLeft(line, " ")
		run := fenceRun(line)
		switch {
		case run == "":
		case open == "":
			open = run
		case run[0] == open[0] && len(run) >= len(open) && strings.TrimSpace(line[len(run):]) == "":
			open = ""
		}
	}
	return open
}

// fenceRun returns line's leading run of three or more backticks or
// tildes, or "".
func fenceRun(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := len(line) - len(strings.TrimLeft(line, line[:1]))
	if n < 3 {
		return ""
	}
	return line[:n]
}

// neutraliseMentions puts a zero-width space after every "@", so text from
// an alert or from Claude can't @mention anyone on GitHub.
func neutraliseMentions(s string) string {
	return strings.ReplaceAll(s, "@", "@​")
}

// truncateBytes cuts s to at most n bytes, at a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	runes := 0
	for i := range s {
		if runes == n {
			return s[:i]
		}
		runes++
	}
	return s
}
