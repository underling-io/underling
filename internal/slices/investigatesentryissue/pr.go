package investigatesentryissue

import (
	"fmt"
	"strings"

	"heinzel/internal/events"
)

// GitHub limits; the body cap leaves headroom below GitHub's 65536.
const (
	maxPRTitleRunes = 200
	maxPRBodyBytes  = 60000
)

// prTitle names the Sentry issue. Alert text can't @mention anyone.
func prTitle(alert events.SentryIssueAlertReceived) string {
	title := fmt.Sprintf("Fix Sentry issue %s: %s", alert.IssueID, alert.Title)
	return truncateRunes(neutraliseMentions(title), maxPRTitleRunes)
}

// prBody links the Sentry issue and carries Claude's summary. Neither the
// alert's text nor Claude's can @mention anyone. Only the summary is cut to
// keep the body within maxPRBodyBytes, so the footer always survives.
func prBody(alert events.SentryIssueAlertReceived, summary string) string {
	header := fmt.Sprintf("Sentry issue [%s](%s): %s\n\n## Claude's summary\n\n", neutraliseMentions(alert.IssueID), alert.WebURL, neutraliseMentions(alert.Title))
	header = truncateBytes(header, maxPRBodyBytes-len(prFooter))
	summary = truncateSummary(neutraliseMentions(strings.TrimSpace(summary)), maxPRBodyBytes-len(header)-len(prFooter))
	return header + summary + prFooter
}

// prFooter ends every PR body.
const prFooter = "\n\n---\nOpened by heinzel. Claude Code investigated the issue in a sandbox; review before merging.\n"

// truncateSummary cuts s to at most n bytes at a rune boundary. If the cut
// leaves a code fence open, it closes it with the fence's own characters,
// within the n bytes.
func truncateSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := truncateBytes(s, n)
	for reserve := 0; ; {
		fence := openFence(cut)
		if fence == "" {
			return cut
		}
		closing := "\n" + fence
		if len(closing) <= reserve {
			return cut + closing
		}
		// Make room for the closing fence and look again: the shorter cut
		// may end in another fence, or in none.
		reserve = len(closing)
		cut = truncateBytes(s, max(n-reserve, 0))
	}
}

// openFence returns the opening run of the Markdown code fence (``` or ~~~,
// possibly longer) that s ends inside, or "" if s ends outside any fence. As
// in CommonMark, only a run of the same character, at least as long and
// with nothing after it, closes a fence.
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

// fenceRun returns line's leading run of three or more backticks or tildes,
// or "".
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
