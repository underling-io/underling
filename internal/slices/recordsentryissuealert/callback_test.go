package recordsentryissuealert_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"heinzel/internal/events"
	"heinzel/internal/platform/app"
	"heinzel/internal/platform/eventstore"
	"heinzel/internal/slices/recordsentryissuealert"
)

const secret = "test-client-secret"

// eventAlertFixture is shaped like Sentry's documented event_alert webhook.
const eventAlertFixture = `{
  "action": "triggered",
  "installation": {"uuid": "a8e5d37a-696c-4c54-adb5-b3f28d64c7de"},
  "data": {
    "event": {
      "_ref": 1293919,
      "_ref_version": 2,
      "contexts": {"browser": {"name": "Chrome", "type": "browser", "version": "75.0.3770"}},
      "culprit": "?(runner)",
      "datetime": "2019-08-19T20:58:37.391000Z",
      "dist": null,
      "event_id": "aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4",
      "exception": {"values": [{"type": "ReferenceError", "value": "blooopy is not defined"}]},
      "fingerprint": ["{{ default }}"],
      "hashes": ["29f68ffe4ca9ad0b1e2b4f9a3ad0f4bc"],
      "issue_url": "https://sentry.io/api/0/issues/1117540176/",
      "issue_id": "1117540176",
      "key_id": "667532",
      "level": "error",
      "location": "example.js",
      "logger": "",
      "message": "",
      "metadata": {"filename": "example.js", "type": "ReferenceError", "value": "blooopy is not defined"},
      "platform": "javascript",
      "project": 1,
      "received": 1566248777.677,
      "release": null,
      "request": {"url": "https://example.com"},
      "sdk": {"name": "sentry.javascript.browser", "version": "5.5.0"},
      "tags": [["browser", "Chrome 75.0.3770"], ["level", "error"]],
      "time_spent": null,
      "title": "ReferenceError: blooopy is not defined",
      "type": "error",
      "url": "https://sentry.io/api/0/projects/test-org/front-end/events/aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4/",
      "user": {"ip_address": "162.218.85.90"},
      "version": "7",
      "web_url": "https://sentry.io/organizations/test-org/issues/1117540176/events/aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4/"
    },
    "triggered_rule": "Very Important Alert Rule!",
    "issue_alert": {"title": "Very Important Alert Rule!", "settings": [{"name": "channel", "value": "#general"}]}
  },
  "actor": {"type": "application", "id": "sentry", "name": "Sentry"}
}`

// alert returns the fixture with the event id and triggered rule replaced.
func alert(t *testing.T, eventID, rule string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(eventAlertFixture), &body); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	data := body["data"].(map[string]any)
	data["triggered_rule"] = rule
	data["event"].(map[string]any)["event_id"] = eventID
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return string(encoded)
}

func sign(key, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func newStore(t *testing.T) *eventstore.Store {
	t.Helper()
	s, err := eventstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newRouter serves the slice's route, as the app does, on store. The slice
// reads secret from its env, so a signed request is accepted only if that
// secret is the key it verifies with.
func newRouter(t *testing.T, store *eventstore.Store) http.Handler {
	t.Helper()
	sc, err := recordsentryissuealert.New(env(map[string]string{"SENTRY_CLIENT_SECRET": secret}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new slice: %v", err)
	}
	return app.Router(store, []app.StateChange{sc})
}

type request struct {
	body      string
	resource  string // Sentry-Hook-Resource; omitted when empty
	signature string // Sentry-Hook-Signature; omitted when empty
}

// signed returns a request for body with a valid signature and resource event_alert.
func signed(body string) request {
	return request{body: body, resource: "event_alert", signature: sign(secret, body)}
}

func post(router http.Handler, r request) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/sentry/callback", strings.NewReader(r.body))
	req.Header.Set("Content-Type", "application/json")
	if r.resource != "" {
		req.Header.Set("Sentry-Hook-Resource", r.resource)
	}
	if r.signature != "" {
		req.Header.Set("Sentry-Hook-Signature", r.signature)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func storedEvents(t *testing.T, s *eventstore.Store) []eventstore.Event {
	t.Helper()
	stored, err := s.ReadAfter(0, 1000)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return stored
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("got status %d, want %d: %s", rec.Code, want, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("got Content-Type %q, want application/json", ct)
	}
}

func assertStored(t *testing.T, s *eventstore.Store, want int) {
	t.Helper()
	if got := len(storedEvents(t, s)); got != want {
		t.Fatalf("stored %d events, want %d", got, want)
	}
}

func TestValidAlertIsRecorded(t *testing.T) {
	s := newStore(t)

	rec := post(newRouter(t, s), signed(eventAlertFixture))

	assertStatus(t, rec, http.StatusOK)
	stored := storedEvents(t, s)
	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	e := stored[0]
	streamHash := sha256.Sum256([]byte("aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4\x00Very Important Alert Rule!"))
	if e.Type != "SentryIssueAlertReceived" || e.Stream != "sentry-issue-alert-"+hex.EncodeToString(streamHash[:]) || e.Version != 1 {
		t.Fatalf("got event %+v", e)
	}
	var got map[string]any
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	want := map[string]any{
		"eventId":        "aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4",
		"issueId":        "1117540176",
		"project":        1.0,
		"title":          "ReferenceError: blooopy is not defined",
		"level":          "error",
		"webUrl":         "https://sentry.io/organizations/test-org/issues/1117540176/events/aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4/",
		"triggeredRule":  "Very Important Alert Rule!",
		"culprit":        "?(runner)",
		"exceptionType":  "ReferenceError",
		"exceptionValue": "blooopy is not defined",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got payload %v, want %v", got, want)
	}
	var typed events.SentryIssueAlertReceived
	if err := json.Unmarshal(e.Payload, &typed); err != nil || typed.Project != 1 {
		t.Fatalf("payload does not decode into the contract: %+v, %v", typed, err)
	}
}

func TestBadSignatureIsUnauthorizedAndStoresNothing(t *testing.T) {
	body := eventAlertFixture
	cases := map[string]string{
		"missing":      "",
		"not hex":      "zz" + sign(secret, body)[2:],
		"wrong secret": sign("other-secret", body),
		"other body":   sign(secret, body+" "),
		"truncated":    sign(secret, body)[:62],
	}
	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)

			rec := post(newRouter(t, s), request{body: body, resource: "event_alert", signature: signature})

			assertStatus(t, rec, http.StatusUnauthorized)
			assertStored(t, s, 0)
		})
	}
}

func TestOtherResourcesAreAcknowledgedAndStoreNothing(t *testing.T) {
	for _, resource := range []string{"", "issue", "installation", "error", "EVENT_ALERT", "event_alert "} {
		t.Run(resource, func(t *testing.T) {
			s := newStore(t)
			r := signed(eventAlertFixture)
			r.resource = resource

			rec := post(newRouter(t, s), r)

			assertStatus(t, rec, http.StatusOK)
			assertStored(t, s, 0)
		})
	}
}

func TestRedeliveredAlertIsStoredOnce(t *testing.T) {
	s := newStore(t)
	router := newRouter(t, s)

	first := post(router, signed(alert(t, "event-1", "rule-a")))
	second := post(router, signed(alert(t, "event-1", "rule-a")))

	assertStatus(t, first, http.StatusOK)
	assertStatus(t, second, http.StatusOK)
	assertStored(t, s, 1)
}

func TestSameEventTriggeringAnotherRuleIsANewAlert(t *testing.T) {
	s := newStore(t)
	router := newRouter(t, s)

	assertStatus(t, post(router, signed(alert(t, "event-1", "rule-a"))), http.StatusOK)
	assertStatus(t, post(router, signed(alert(t, "event-1", "rule-b"))), http.StatusOK)

	assertStored(t, s, 2)
}

func TestIDsAreStoredAsSent(t *testing.T) {
	s := newStore(t)

	assertStatus(t, post(newRouter(t, s), signed(alert(t, " event-1 ", " rule-a "))), http.StatusOK)

	var got events.SentryIssueAlertReceived
	if err := json.Unmarshal(storedEvents(t, s)[0].Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.EventID != " event-1 " || got.TriggeredRule != " rule-a " {
		t.Fatalf("got %+v, want untrimmed ids", got)
	}
}

func TestInvalidAlertIsBadRequestAndStoresNothing(t *testing.T) {
	withEvent := func(event string) string {
		return `{"data":{"event":` + event + `,"triggered_rule":"rule-a"}}`
	}
	cases := map[string]string{
		"malformed JSON":         `{"data":`,
		"empty body":             ``,
		"not an object":          `[]`,
		"missing event id":       withEvent(`{"issue_id":"1"}`),
		"blank event id":         withEvent(`{"event_id":" \t","issue_id":"1"}`),
		"missing issue id":       withEvent(`{"event_id":"e1"}`),
		"blank issue id":         withEvent(`{"event_id":"e1","issue_id":"  "}`),
		"numeric issue id":       withEvent(`{"event_id":"e1","issue_id":1}`),
		"string project":         withEvent(`{"event_id":"e1","issue_id":"1","project":"1"}`),
		"missing triggered rule": `{"data":{"event":{"event_id":"e1","issue_id":"1"}}}`,
		"blank triggered rule":   `{"data":{"event":{"event_id":"e1","issue_id":"1"},"triggered_rule":" "}}`,
		"missing data":           `{"action":"triggered"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)

			rec := post(newRouter(t, s), signed(body))

			assertStatus(t, rec, http.StatusBadRequest)
			assertStored(t, s, 0)
		})
	}
}

func TestOversizedBodyIsRejectedAndStoresNothing(t *testing.T) {
	s := newStore(t)
	body := `{"data":{"event":{"event_id":"e1","issue_id":"1","title":"` + strings.Repeat("x", 10<<20) + `"},"triggered_rule":"rule-a"}}`

	rec := post(newRouter(t, s), signed(body))

	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
	assertStored(t, s, 0)
}

func TestOversizedBodyClosesTheConnection(t *testing.T) {
	srv := httptest.NewServer(newRouter(t, newStore(t)))
	defer srv.Close()
	body := strings.Repeat("x", 10<<20+1)

	resp, err := srv.Client().Post(srv.URL+"/sentry/callback", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
		t.Fatalf("got status %d, close %v; want %d and the connection closed", resp.StatusCode, resp.Close, http.StatusRequestEntityTooLarge)
	}
}

func TestStoreFailureIsInternalErrorWithoutDetails(t *testing.T) {
	s := newStore(t)
	router := newRouter(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	rec := post(router, signed(eventAlertFixture))

	assertStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), "database") {
		t.Fatalf("got body %s, want a generic error", rec.Body)
	}
}

func TestSignatureIsCheckedBeforeDecoding(t *testing.T) {
	s := newStore(t)

	rec := post(newRouter(t, s), request{body: `{"data":`, resource: "event_alert", signature: sign("other-secret", `{"data":`)})

	assertStatus(t, rec, http.StatusUnauthorized)
}

// recordedPayload posts a signed alert whose event is event and returns the
// stored payload as a generic map.
func recordedPayload(t *testing.T, event map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": map[string]any{"event": event, "triggered_rule": "rule-a"}})
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	s := newStore(t)
	assertStatus(t, post(newRouter(t, s), signed(string(body))), http.StatusOK)
	stored := storedEvents(t, s)
	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	var got map[string]any
	if err := json.Unmarshal(stored[0].Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return got
}

func TestExceptionDetailsComeFromTheOutermostExceptionAndKeepTheLast50Frames(t *testing.T) {
	var frames []any
	for i := 1; i <= 60; i++ {
		frames = append(frames, map[string]any{"filename": fmt.Sprintf("f%d.go", i), "function": fmt.Sprintf("fn%d", i), "lineno": i, "in_app": true})
	}
	got := recordedPayload(t, map[string]any{
		"event_id": "e1",
		"issue_id": "1",
		"culprit":  "app.handler",
		"exception": map[string]any{"values": []any{
			map[string]any{"type": "InnerError", "value": "cause", "stacktrace": map[string]any{"frames": []any{map[string]any{"filename": "inner.go", "function": "inner", "lineno": 1}}}},
			map[string]any{"type": "OuterError", "value": "wrapped", "stacktrace": map[string]any{"frames": frames}},
		}},
	})

	if got["culprit"] != "app.handler" || got["exceptionType"] != "OuterError" || got["exceptionValue"] != "wrapped" {
		t.Fatalf("got payload %v, want culprit and the outermost exception", got)
	}
	gotFrames, _ := got["frames"].([]any)
	if len(gotFrames) != 50 {
		t.Fatalf("got %d frames, want 50", len(gotFrames))
	}
	first := map[string]any{"file": "f11.go", "function": "fn11", "line": 11.0}
	last := map[string]any{"file": "f60.go", "function": "fn60", "line": 60.0}
	if !reflect.DeepEqual(gotFrames[0], first) || !reflect.DeepEqual(gotFrames[49], last) {
		t.Fatalf("got first frame %v and last %v, want %v and %v (Sentry's order, last 50 kept)", gotFrames[0], gotFrames[49], first, last)
	}
}

func TestExceptionDetailsAreCappedInLength(t *testing.T) {
	long := func(n int) string { return strings.Repeat("ü", n) }
	got := recordedPayload(t, map[string]any{
		"event_id": "e1",
		"issue_id": "1",
		"culprit":  long(2100),
		"exception": map[string]any{"values": []any{map[string]any{
			"type":       long(2100),
			"value":      long(2100),
			"stacktrace": map[string]any{"frames": []any{map[string]any{"filename": long(600), "function": long(600), "lineno": 1}}},
		}}},
	})

	for field, want := range map[string]string{"culprit": long(2000), "exceptionType": long(2000), "exceptionValue": long(2000)} {
		if got[field] != want {
			t.Errorf("%s has %d runes, want 2000", field, len([]rune(fmt.Sprint(got[field]))))
		}
	}
	frame := got["frames"].([]any)[0].(map[string]any)
	if frame["file"] != long(500) || frame["function"] != long(500) {
		t.Errorf("frame file and function have %d and %d runes, want 500", len([]rune(fmt.Sprint(frame["file"]))), len([]rune(fmt.Sprint(frame["function"]))))
	}
}

func TestAlertWithoutExceptionDetailsHasOnlyTheOriginalFields(t *testing.T) {
	got := recordedPayload(t, map[string]any{"event_id": "e1", "issue_id": "1", "project": 3, "title": "t", "level": "error", "web_url": "https://sentry.io/x"})

	want := map[string]any{"eventId": "e1", "issueId": "1", "project": 3.0, "title": "t", "level": "error", "webUrl": "https://sentry.io/x", "triggeredRule": "rule-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got payload %v, want %v", got, want)
	}
}

func TestMalformedExceptionDetailsAreDroppedNotRejected(t *testing.T) {
	got := recordedPayload(t, map[string]any{"event_id": "e1", "issue_id": "1", "exception": map[string]any{"values": "not a list"}})

	if _, ok := got["exceptionType"]; ok {
		t.Fatalf("got payload %v, want no exception details", got)
	}
}

func TestStoredAlertWithoutDetailsStillDecodes(t *testing.T) {
	var got events.SentryIssueAlertReceived
	old := `{"eventId":"e1","issueId":"1","project":1,"title":"t","level":"error","webUrl":"u","triggeredRule":"r"}`
	if err := json.Unmarshal([]byte(old), &got); err != nil || got.IssueID != "1" || got.Frames != nil || got.Culprit != "" {
		t.Fatalf("got %+v, %v; want the old payload to decode with empty details", got, err)
	}
}
