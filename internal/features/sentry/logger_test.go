package sentry_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
	"heinzel/internal/features/sentry"
)

// alertLogger returns the feature's only handler, logging to buf.
func alertLogger(t *testing.T, buf *bytes.Buffer) eventhandler.Handler {
	t.Helper()
	handlers := sentry.New(secret, slog.New(slog.NewJSONHandler(buf, nil)))(http.NewServeMux(), nil)
	if len(handlers) != 1 {
		t.Fatalf("got %d handlers, want 1", len(handlers))
	}
	return handlers[0]
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		delete(decoded, "time")
		lines = append(lines, decoded)
	}
	return lines
}

func storedEvent(position uint64, eventType, payload string) eventstore.Event {
	return eventstore.Event{
		StreamID:  "sentry-issue-alert-abc",
		Version:   1,
		Position:  position,
		Type:      eventType,
		Timestamp: time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC),
		Payload:   json.RawMessage(payload),
	}
}

func TestAlertLoggerNameIsFixed(t *testing.T) {
	if got := alertLogger(t, new(bytes.Buffer)).Name(); got != "sentry-issue-alert-logger" {
		t.Fatalf("got name %q, want sentry-issue-alert-logger", got)
	}
}

func TestAlertLoggerLogsOneLinePerAlert(t *testing.T) {
	var buf bytes.Buffer
	h := alertLogger(t, &buf)

	h.Handle(storedEvent(7, "SentryIssueAlertReceived", `{"eventId":"e1","issueId":"1117540176","project":1,"title":"ReferenceError: blooopy is not defined","level":"error","webUrl":"https://sentry.io/x","triggeredRule":"rule-a"}`))

	got := logLines(t, &buf)
	// The alert's level is logged as sentryLevel: a "level" attribute would
	// duplicate the log record's own level key.
	want := []map[string]any{{
		"level":       "INFO",
		"msg":         "sentry issue alert received",
		"handler":     "sentry-issue-alert-logger",
		"position":    7.0,
		"issueId":     "1117540176",
		"title":       "ReferenceError: blooopy is not defined",
		"sentryLevel": "error",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAlertLoggerIgnoresOtherEventTypes(t *testing.T) {
	var buf bytes.Buffer
	h := alertLogger(t, &buf)

	h.Handle(storedEvent(1, "NoteRegistered", `{"id":"n1","text":"hello"}`))

	if buf.Len() != 0 {
		t.Fatalf("got log output %q, want none", buf.String())
	}
}

func TestAlertLoggerLogsUndecodablePayloadAsError(t *testing.T) {
	var buf bytes.Buffer
	h := alertLogger(t, &buf)

	h.Handle(storedEvent(3, "SentryIssueAlertReceived", `{"issueId":1}`))

	lines := logLines(t, &buf)
	if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["handler"] != "sentry-issue-alert-logger" || lines[0]["position"] != 3.0 {
		t.Fatalf("got %v, want one error line", lines)
	}
}
