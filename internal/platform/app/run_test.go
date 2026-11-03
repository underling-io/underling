package app_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"heinzel/internal/platform/app"
	"heinzel/internal/platform/eventhandler"
	"heinzel/internal/platform/eventstore"
)

// waitLimit only guards against hangs; the tests never sleep.
const waitLimit = 5 * time.Second

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// instance is one in-process run of the app.
type instance struct {
	url            string
	cancel         context.CancelFunc
	done           chan error
	listenerClosed chan struct{}
}

// notifyingListener closes closed when the app closes it.
type notifyingListener struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (l *notifyingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

func start(t *testing.T, cfg app.Config, slices app.Slices, logger *slog.Logger) *instance {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	nl := &notifyingListener{Listener: ln, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{url: "http://" + ln.Addr().String(), cancel: cancel, done: make(chan error, 1), listenerClosed: nl.closed}
	go func() { inst.done <- app.Run(ctx, cfg, nl, slices, logger) }()
	return inst
}

func (i *instance) stop(t *testing.T) error {
	t.Helper()
	i.cancel()
	select {
	case err := <-i.done:
		return err
	case <-time.After(waitLimit):
		t.Fatal("run did not return")
		return nil
	}
}

func (i *instance) waitUntilListenerClosed(t *testing.T) {
	t.Helper()
	select {
	case <-i.listenerClosed:
	case <-time.After(waitLimit):
		t.Fatal("listener still open")
	}
}

// post sends an empty POST to path and returns the status and body.
func (i *instance) post(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Post(i.url+path, "text/plain", nil)
	if err != nil {
		t.Errorf("post: %v", err)
		return 0, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// appendTo appends a TestEvent to a new stream and writes its position.
func appendTo(c *gin.Context, store *eventstore.Store, stream string) {
	_, position, err := store.Append(stream, 0, []eventstore.NewEvent{{Type: "TestEvent", Payload: json.RawMessage(`{}`)}})
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	c.String(http.StatusOK, strconv.FormatUint(position, 10))
}

// appender serves POST /append/:stream.
var appender = app.StateChange{
	Method: http.MethodPost,
	Path:   "/append/:stream",
	Handler: func(store *eventstore.Store) gin.HandlerFunc {
		return func(c *gin.Context) { appendTo(c, store, c.Param("stream")) }
	},
}

// blocking serves POST /block, which waits for release before appending.
func blocking() (sc app.StateChange, entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}, 1), make(chan struct{})
	sc = app.StateChange{
		Method: http.MethodPost,
		Path:   "/block",
		Handler: func(store *eventstore.Store) gin.HandlerFunc {
			return func(c *gin.Context) {
				entered <- struct{}{}
				<-release
				appendTo(c, store, "blocked")
			}
		},
	}
	return sc, entered, release
}

// automation wraps a handler that needs neither ctx nor the store.
func automation(h eventhandler.Handler) app.Automation {
	return app.Automation{Handler: func(context.Context, *eventstore.Store) eventhandler.Handler { return h }}
}

// recorder passes each handled position to a channel. It blocks in its
// first Handle while block is set, until released.
type recorder struct {
	name    string
	block   bool
	entered chan uint64
	release chan struct{}
	handled chan uint64
}

func newRecorder(name string, block bool) *recorder {
	return &recorder{name: name, block: block, entered: make(chan uint64, 1), release: make(chan struct{}), handled: make(chan uint64, 100)}
}

func (r *recorder) Name() string { return r.name }
func (r *recorder) Handle(e eventstore.Event) {
	if r.block {
		r.block = false
		r.entered <- e.Position
		<-r.release
	}
	r.handled <- e.Position
}

func (r *recorder) expect(t *testing.T, want ...uint64) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-r.handled:
			if got != w {
				t.Fatalf("handled position %d, want %d", got, w)
			}
		case <-time.After(waitLimit):
			t.Fatalf("timed out waiting for position %d", w)
		}
	}
}

// lineWriter passes each log line to a channel.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func (w lineWriter) waitFor(t *testing.T, msg string) {
	t.Helper()
	timeout := time.After(waitLimit)
	for {
		select {
		case line := <-w:
			var entry struct{ Msg string }
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == msg {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for log %q", msg)
		}
	}
}

func assertNotDone(t *testing.T, done <-chan error, msg string) {
	t.Helper()
	select {
	case <-done:
		t.Fatal(msg)
	default:
	}
}

func TestEventsAreHandledOnceAcrossRestarts(t *testing.T) {
	cfg := app.Config{DataDir: t.TempDir(), ShutdownTimeout: time.Second}

	for run, stream := range []string{"first", "second"} {
		rec := newRecorder("recorder", false)
		inst := start(t, cfg, app.Slices{StateChanges: []app.StateChange{appender}, Automations: []app.Automation{automation(rec)}}, discard())

		want := uint64(run + 1)
		if code, body := inst.post(t, "/append/"+stream); code != http.StatusOK || body != strconv.FormatUint(want, 10) {
			t.Fatalf("run %d: got %d %q", run, code, body)
		}
		rec.expect(t, want)
		if err := inst.stop(t); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if len(rec.handled) != 0 {
			t.Fatalf("run %d: also handled position %d", run, <-rec.handled)
		}
	}
}

func TestShutdownDrainsRequestsThenStopsHandlersThenClosesTheStore(t *testing.T) {
	dir := t.TempDir()
	cfg := app.Config{DataDir: dir, ShutdownTimeout: waitLimit}
	block, requestEntered, releaseRequest := blocking()
	handler := newRecorder("projector", true)
	logs := make(lineWriter, 100)
	inst := start(t, cfg, app.Slices{StateChanges: []app.StateChange{appender, block}, Automations: []app.Automation{automation(handler)}}, slog.New(slog.NewJSONHandler(logs, nil)))

	for _, stream := range []string{"a", "b", "c"} {
		if code, body := inst.post(t, "/append/"+stream); code != http.StatusOK {
			t.Fatalf("append %s: got %d (%s)", stream, code, body)
		}
	}
	<-handler.entered // the handler is now mid-event at position 1

	inFlight := make(chan int, 1)
	go func() {
		code, _ := inst.post(t, "/block")
		inFlight <- code
	}()
	<-requestEntered
	inst.cancel()
	inst.waitUntilListenerClosed(t)

	assertNotDone(t, inst.done, "run returned before the in-flight request finished")
	close(releaseRequest)
	if code := <-inFlight; code != http.StatusOK {
		t.Fatalf("in-flight request got %d, want 200", code)
	}
	logs.waitFor(t, "stopping event handlers")
	assertNotDone(t, inst.done, "run returned while a handler was mid-event")
	close(handler.release)
	if err := inst.stop(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(handler.handled); got != 1 {
		t.Fatalf("handler processed %d events before stopping, want 1", got)
	}

	resumed := newRecorder("projector", false)
	inst = start(t, cfg, app.Slices{Automations: []app.Automation{automation(resumed)}}, discard())
	resumed.expect(t, 2, 3, 4)
	if err := inst.stop(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestShutdownCompletesWhenARequestOutlivesTheTimeout(t *testing.T) {
	dir := t.TempDir()
	block, requestEntered, releaseRequest := blocking()
	inst := start(t, app.Config{DataDir: dir, ShutdownTimeout: 10 * time.Millisecond}, app.Slices{StateChanges: []app.StateChange{block}}, discard())

	inFlight := make(chan struct{})
	go func() {
		defer close(inFlight)
		if resp, err := http.Post(inst.url+"/block", "text/plain", nil); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-requestEntered

	if err := inst.stop(t); err == nil || !strings.Contains(err.Error(), "shut down http server") {
		t.Errorf("run returned %v, want the forced shutdown's error", err)
	}

	s, err := eventstore.Open(dir) // the store was closed despite the request
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	_ = s.Close()
	close(releaseRequest)
	<-inFlight
}

func TestRunFailsFastWhenTheStoreCannotOpen(t *testing.T) {
	dir := t.TempDir()
	held, err := eventstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	inst := start(t, app.Config{DataDir: dir, ShutdownTimeout: time.Second}, app.Slices{}, discard())

	select {
	case err := <-inst.done:
		if err == nil {
			t.Fatal("run succeeded, want an error")
		}
	case <-time.After(waitLimit):
		t.Fatal("run did not fail fast")
	}
	inst.waitUntilListenerClosed(t)
}

// ctxBlocker blocks in Handle until its ctx is cancelled.
type ctxBlocker struct {
	ctx     context.Context
	entered chan struct{}
	sawDone chan struct{}
}

func (*ctxBlocker) Name() string { return "ctx-blocker" }
func (b *ctxBlocker) Handle(eventstore.Event) {
	close(b.entered)
	<-b.ctx.Done()
	close(b.sawDone)
}

func TestTheAutomationsCtxIsCancelledBeforeTheHandlersStop(t *testing.T) {
	built := make(chan *eventstore.Store, 1)
	blocker := &ctxBlocker{entered: make(chan struct{}), sawDone: make(chan struct{})}
	a := app.Automation{Handler: func(ctx context.Context, store *eventstore.Store) eventhandler.Handler {
		blocker.ctx = ctx
		built <- store
		return blocker
	}}
	inst := start(t, app.Config{DataDir: t.TempDir(), ShutdownTimeout: time.Second}, app.Slices{Automations: []app.Automation{a}}, discard())

	store := <-built
	if _, _, err := store.Append("s", 0, []eventstore.NewEvent{{Type: "TestEvent", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	<-blocker.entered
	if blocker.ctx.Err() != nil {
		t.Fatal("the automations' ctx was cancelled before shutdown")
	}

	if err := inst.stop(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	<-blocker.sawDone
}

func TestTheRouterServesStateChangesOnTheStore(t *testing.T) {
	store, err := eventstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	router := app.Router(store, []app.StateChange{appender})

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/append/a", http.StatusOK},
		{http.MethodGet, "/append/b", http.StatusMethodNotAllowed},
		{http.MethodPost, "/append/c/", http.StatusNotFound},
		{http.MethodPost, "/nowhere", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.want {
			t.Errorf("%s %s: got %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
	if stored, _ := store.ReadAfter(0, 10); len(stored) != 1 || stored[0].Stream != "a" {
		t.Errorf("stored %+v, want one event on stream a", stored)
	}
}

func TestHealthzAnswersOnceListening(t *testing.T) {
	inst := start(t, app.Config{DataDir: t.TempDir(), ShutdownTimeout: time.Second}, app.Slices{}, discard())
	defer func() {
		if err := inst.stop(t); err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	resp, err := http.Get(inst.url + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("got %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}
}

func TestParseFlags(t *testing.T) {
	got, err := app.ParseFlags(nil, io.Discard)
	if want := (app.Config{Addr: ":8080", DataDir: "./data", ShutdownTimeout: 10 * time.Second}); err != nil || got != want {
		t.Errorf("defaults: got %+v, %v; want %+v", got, err, want)
	}

	got, err = app.ParseFlags([]string{"-addr", "127.0.0.1:9000", "-data-dir", "/var/lib/heinzel", "-shutdown-timeout", "3s"}, io.Discard)
	if want := (app.Config{Addr: "127.0.0.1:9000", DataDir: "/var/lib/heinzel", ShutdownTimeout: 3 * time.Second}); err != nil || got != want {
		t.Errorf("overrides: got %+v, %v; want %+v", got, err, want)
	}

	for _, args := range [][]string{{"-nope"}, {"serve"}, {"-shutdown-timeout", "soon"}} {
		if _, err := app.ParseFlags(args, io.Discard); err == nil {
			t.Errorf("ParseFlags(%q) succeeded, want an error", args)
		}
	}
}
