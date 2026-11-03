package investigatesentryissue

import (
	"strings"
	"testing"

	"underling/internal/events"
)

func TestPromptCapsTheTitle(t *testing.T) {
	long := strings.Repeat("é", maxPromptTitleRunes+100)

	got := investigationPrompt(events.SentryIssueAlertReceived{IssueID: "42", Title: long}, "b")

	if !strings.Contains(got, strings.Repeat("é", maxPromptTitleRunes)) || strings.Contains(got, strings.Repeat("é", maxPromptTitleRunes+1)) {
		t.Fatalf("prompt does not cap the title at %d runes", maxPromptTitleRunes)
	}
}

func TestPromptStartsAutoTaskAndCarriesIssueDetailsAsData(t *testing.T) {
	alert := events.SentryIssueAlertReceived{
		IssueID:        "42",
		Project:        1,
		Title:          "TypeError: x is undefined",
		Level:          "error",
		WebURL:         "https://sentry.io/i/42",
		Culprit:        "app.handler",
		ExceptionType:  "TypeError",
		ExceptionValue: "x is undefined",
		Frames:         []events.SentryStackFrame{{File: "app.js", Function: "handler", Line: 7}},
	}

	got := investigationPrompt(alert, "underling/sentry-42")

	if !strings.HasPrefix(got, "/at:auto-task ") {
		t.Errorf("prompt does not start with /at:auto-task: %q", got)
	}
	for _, want := range []string{
		"underling/sentry-42",
		"Commit",
		"Do not push",
		"/at:ship-task",
		"summary",
		`"issueId": "42"`,
		`"culprit": "app.handler"`,
		`"exceptionType": "TypeError"`,
		`"exceptionValue": "x is undefined"`,
		`"file": "app.js"`,
		`"webUrl": "https://sentry.io/i/42"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}

func TestPromptOverridesAutoTasksWorktreeBranchAndShipSteps(t *testing.T) {
	got := investigationPrompt(events.SentryIssueAlertReceived{IssueID: "42"}, "underling/sentry-42")

	for _, want := range []string{
		"Run every /at:auto-task step in place, in the current directory",
		"Do not create a git worktree",
		"do not create, switch or rename branches",
		"commit only on branch underling/sentry-42",
		"Skip auto-task's ship and hand-off steps",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}
