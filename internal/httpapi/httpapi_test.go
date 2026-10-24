package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"heinzel/internal/command"
	"heinzel/internal/eventstore"
	"heinzel/internal/httpapi"
)

func newStore(t *testing.T) *eventstore.Store {
	t.Helper()
	s, err := eventstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newHandler(store httpapi.Appender) http.Handler {
	return httpapi.NewHandler(command.DefaultRegistry(), store, slog.New(slog.DiscardHandler))
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/commands", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func registerNote(id string) string {
	return fmt.Sprintf(`{"type":"RegisterNote","payload":{"id":%q,"text":"hello"}}`, id)
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("got Content-Type %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body
}

func storedEvents(t *testing.T, s *eventstore.Store) []eventstore.Event {
	t.Helper()
	events, err := s.ReadAfter(0, 1000)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}

func TestRegisterNoteReturnsCreated(t *testing.T) {
	s := newStore(t)
	h := newHandler(s)
	if rec := post(h, registerNote("first")); rec.Code != http.StatusCreated {
		t.Fatalf("setup: got %d", rec.Code)
	}

	rec := post(h, registerNote("n1"))

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201: %s", rec.Code, rec.Body)
	}
	got := decodeBody(t, rec)
	want := map[string]any{"streamId": "n1", "version": 1.0, "position": 2.0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got body %v, want %v", got, want)
	}
	events := storedEvents(t, s)
	if len(events) != 2 || events[1].Type != "NoteRegistered" || events[1].StreamID != "n1" {
		t.Fatalf("got stored events %+v", events)
	}
}

func TestBadRequestsReturn400AndStoreNothing(t *testing.T) {
	cases := map[string]string{
		"malformed JSON":       `{"type":`,
		"not an object":        `[]`,
		"trailing data":        registerNote("n1") + `{}`,
		"missing payload":      `{"type":"AcceptAnything"}`,
		"null payload":         `{"type":"AcceptAnything","payload":null}`,
		"non-object payload":   `{"type":"AcceptAnything","payload":"n1"}`,
		"array payload":        `{"type":"AcceptAnything","payload":[]}`,
		"empty type":           `{"type":"","payload":{"id":"n1","text":"hello"}}`,
		"missing type":         `{"payload":{"id":"n1","text":"hello"}}`,
		"unknown type":         `{"type":"Nope","payload":{"id":"n1","text":"hello"}}`,
		"failed validation":    `{"type":"RegisterNote","payload":{"id":"n1","text":" "}}`,
		"body exceeds 1 MiB":   `{"type":"RegisterNote","payload":{"id":"n1","text":"` + strings.Repeat("x", 1<<20) + `"}}`,
		"empty body":           ``,
		"wrong payload fields": `{"type":"RegisterNote","payload":{"id":1,"text":"hello"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)

			registry := command.DefaultRegistry()
			registry.Register("AcceptAnything", func(context.Context, json.RawMessage) (command.Decision, error) {
				return command.Decision{StreamID: "any", Events: []eventstore.NewEvent{{Type: "Accepted", Payload: json.RawMessage(`{}`)}}}, nil
			})
			h := httpapi.NewHandler(registry, s, slog.New(slog.DiscardHandler))

			rec := post(h, body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400: %s", rec.Code, rec.Body)
			}
			if msg, _ := decodeBody(t, rec)["error"].(string); msg == "" {
				t.Fatalf("got body %s, want non-empty error", rec.Body)
			}
			if events := storedEvents(t, s); len(events) != 0 {
				t.Fatalf("stored %d events, want none", len(events))
			}
		})
	}
}

func TestDuplicateIDReturnsConflict(t *testing.T) {
	s := newStore(t)
	h := newHandler(s)
	post(h, registerNote("n1"))

	rec := post(h, registerNote("n1"))

	if rec.Code != http.StatusConflict {
		t.Fatalf("got status %d, want 409: %s", rec.Code, rec.Body)
	}
	if msg, _ := decodeBody(t, rec)["error"].(string); msg == "" {
		t.Fatalf("got body %s, want non-empty error", rec.Body)
	}
	if events := storedEvents(t, s); len(events) != 1 {
		t.Fatalf("stored %d events, want 1", len(events))
	}
}

type failingStore struct{}

func (failingStore) Append(string, uint64, []eventstore.NewEvent) (uint64, uint64, error) {
	return 0, 0, errors.New("bbolt: database not open")
}

func TestStoreFailureReturns500WithoutInternals(t *testing.T) {
	rec := post(newHandler(failingStore{}), registerNote("n1"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500: %s", rec.Code, rec.Body)
	}
	msg, _ := decodeBody(t, rec)["error"].(string)
	if msg == "" || strings.Contains(msg, "bbolt") {
		t.Fatalf("got error %q, want generic message", msg)
	}
}

func TestConcurrentRegistrationsOfSameIDAdmitExactlyOne(t *testing.T) {
	s := newStore(t)
	h := newHandler(s)
	const requests = 50

	codes := concurrently(requests, func(int) int { return post(h, registerNote("same")).Code })

	created, conflicts := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || conflicts != requests-1 {
		t.Fatalf("got %d created and %d conflicts, want 1 and %d", created, conflicts, requests-1)
	}
	if events := storedEvents(t, s); len(events) != 1 {
		t.Fatalf("stored %d events, want 1", len(events))
	}
}

func TestConcurrentRegistrationsKeepPositionsGapless(t *testing.T) {
	s := newStore(t)
	h := newHandler(s)
	const requests = 100

	codes := concurrently(requests, func(i int) int {
		id := fmt.Sprintf("note-%d", i)
		if i%2 == 1 {
			id = fmt.Sprintf("note-%d", i-1) // every odd request races a duplicate
		}
		return post(h, registerNote(id)).Code
	})

	created := 0
	for _, code := range codes {
		if code == http.StatusCreated {
			created++
		}
	}
	if created != requests/2 {
		t.Fatalf("got %d created, want %d", created, requests/2)
	}
	events := storedEvents(t, s)
	if len(events) != created {
		t.Fatalf("stored %d events, want %d", len(events), created)
	}
	for i, e := range events {
		if e.Position != uint64(i+1) {
			t.Fatalf("event %d has position %d, want %d", i, e.Position, i+1)
		}
	}
}

func concurrently(n int, f func(i int) int) []int {
	results := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			results[i] = f(i)
		})
	}
	close(start)
	wg.Wait()
	return results
}
