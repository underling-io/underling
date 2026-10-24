package eventhandler_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
)

func TestEventLoggerWritesOneStructuredLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	h := eventhandler.NewEventLogger(slog.New(slog.NewJSONHandler(&buf, nil)))

	h.Handle(eventstore.Event{
		StreamID:  "n1",
		Version:   1,
		Position:  3,
		Type:      "NoteRegistered",
		Timestamp: time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC),
		Payload:   json.RawMessage(`{"id":"n1","text":"hello"}`),
	})

	if h.Name() != "event-logger" {
		t.Fatalf("got name %q, want event-logger", h.Name())
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	delete(got, "time")
	delete(got, "msg")
	delete(got, "level")
	want := map[string]any{
		"handler":  "event-logger",
		"position": 3.0,
		"streamId": "n1",
		"version":  1.0,
		"type":     "NoteRegistered",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
