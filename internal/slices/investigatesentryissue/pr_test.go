package investigatesentryissue

import (
	"strings"
	"testing"
	"unicode/utf8"

	"heinzel/internal/events"
)

const zwsp = "\u200b"

func TestPRTitleIsCappedAndMentionsAreNeutralised(t *testing.T) {
	alert := events.SentryIssueAlertReceived{IssueID: "42", Title: "Error for @acme/team " + strings.Repeat("x", 300)}

	got := prTitle(alert)

	if n := utf8.RuneCountInString(got); n != maxPRTitleRunes {
		t.Errorf("title has %d runes, want %d", n, maxPRTitleRunes)
	}
	if !strings.HasPrefix(got, "Fix Sentry issue 42: Error for @"+zwsp+"acme/team ") {
		t.Errorf("got title %q", got)
	}
}

func TestPRBodyNeutralisesMentionsAndKeepsTheSentryLink(t *testing.T) {
	alert := events.SentryIssueAlertReceived{IssueID: "42", Title: "ping @oncall", WebURL: "https://sentry.io/organizations/acme/issues/42/"}

	got := prBody(alert, "Fixed it. cc @octocat")

	for _, want := range []string{"ping @" + zwsp + "oncall", "cc @" + zwsp + "octocat", "(https://sentry.io/organizations/acme/issues/42/)"} {
		if !strings.Contains(got, want) {
			t.Errorf("body lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "@") != strings.Count(got, "@"+zwsp) {
		t.Errorf("body has a live mention:\n%s", got)
	}
}

func TestPRBodyCutsOnlyTheSummaryAtARuneBoundary(t *testing.T) {
	alert := events.SentryIssueAlertReceived{IssueID: "42", Title: "t", WebURL: "https://sentry.io/i/42"}

	got := prBody(alert, strings.Repeat("ü", maxPRBodyBytes))

	if len(got) > maxPRBodyBytes || len(got) < maxPRBodyBytes-utf8.UTFMax {
		t.Errorf("body has %d bytes, want just under %d", len(got), maxPRBodyBytes)
	}
	if !utf8.ValidString(got) {
		t.Errorf("body was cut inside a rune")
	}
	if !strings.HasPrefix(got, "Sentry issue [42](https://sentry.io/i/42): t\n\n## Claude's summary\n\nüü") {
		t.Errorf("body lost its header: %q", got[:80])
	}
	if !strings.HasSuffix(got, prFooter) {
		t.Errorf("body lost its footer: %q", got[len(got)-200:])
	}
}

func TestPRBodyKeepsACutCodeFenceClosed(t *testing.T) {
	alert := events.SentryIssueAlertReceived{IssueID: "42", Title: "t", WebURL: "https://sentry.io/i/42"}
	summary := "Root cause below.\n\n```go\n" + strings.Repeat("x := 1\n", maxPRBodyBytes/7) + "```\n"

	got := prBody(alert, summary)

	if len(got) > maxPRBodyBytes {
		t.Errorf("body has %d bytes, want at most %d", len(got), maxPRBodyBytes)
	}
	if !strings.HasSuffix(got, prFooter) {
		t.Errorf("body lost its footer")
	}
	if n := strings.Count(got, "```"); n%2 != 0 {
		t.Errorf("body has %d fences, so one is left open:\n%s", n, got[len(got)-300:])
	}
}

func TestPRBodyClosesACutTildeFenceWithTildes(t *testing.T) {
	alert := events.SentryIssueAlertReceived{IssueID: "42", Title: "t", WebURL: "https://sentry.io/i/42"}
	summary := "Root cause below.\n\n~~~go\n```\n" + strings.Repeat("x := 1\n", maxPRBodyBytes/7) + "~~~\n"

	got := prBody(alert, summary)

	if len(got) > maxPRBodyBytes {
		t.Errorf("body has %d bytes, want at most %d", len(got), maxPRBodyBytes)
	}
	if !strings.HasSuffix(got, "\n~~~"+prFooter) {
		t.Errorf("the cut tilde fence isn't closed with tildes:\n%s", got[len(got)-300:])
	}
}

func TestOpenFence(t *testing.T) {
	tests := map[string]string{
		"text":                     "",
		"```\ncode":                "```",
		"```\ncode\n```":           "",
		"~~~\ncode":                "~~~",
		"~~~\ncode\n~~~":           "",
		"~~~\n```\ncode":           "~~~", // ``` doesn't close a tilde fence
		"```\n~~~\ncode":           "```",
		"  ~~~~\ncode\n~~~":        "~~~~", // a shorter run doesn't close it
		"~~~~\ncode\n~~~~~\nafter": "",
	}
	for s, want := range tests {
		if got := openFence(s); got != want {
			t.Errorf("openFence(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestPRBodyLeavesAShortSummaryWhole(t *testing.T) {
	alert := events.SentryIssueAlertReceived{IssueID: "42", Title: "t", WebURL: "https://sentry.io/i/42"}

	got := prBody(alert, "```\ncode\n```")

	if want := "Sentry issue [42](https://sentry.io/i/42): t\n\n## Claude's summary\n\n```\ncode\n```" + prFooter; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
