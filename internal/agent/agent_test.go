package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"heinzel/internal/agent"
	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
)

// waitLimit only guards against hangs; tests never sleep.
const waitLimit = 5 * time.Second

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// instance is one in-process run of the agent.
type instance struct {
	url            string
	cancel         context.CancelFunc
	done           chan error
	listenerClosed chan struct{}
}

// notifyingListener closes closed when the agent closes the listener.
type notifyingListener struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (l *notifyingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

func start(t *testing.T, cfg agent.Config, features []agent.Feature, logger *slog.Logger) *instance {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	nl := &notifyingListener{Listener: ln, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{url: "http://" + ln.Addr().String(), cancel: cancel, done: make(chan error, 1), listenerClosed: nl.closed}
	go func() { inst.done <- agent.Run(ctx, cfg, nl, features, logger) }()
	return inst
}

// stop cancels the run and returns its result.
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

// post sends an empty POST to path and returns the status and body.
func (i *instance) post(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Post(i.url+path, "text/plain", nil)
	if err != nil {
		t.Errorf("post: %v", err)
		return 0, ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("read response: %v", err)
	}
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// appendEvent appends one TestEvent to a new stream and writes its position.
func appendEvent(w http.ResponseWriter, store *eventstore.Store, stream string) {
	_, position, err := store.Append(stream, 0, []eventstore.NewEvent{{Type: "TestEvent", Payload: json.RawMessage(`{}`)}})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, strconv.FormatUint(position, 10))
}

// appender is a test feature serving POST /append/{stream}. It has no handlers.
func appender(mux *http.ServeMux, store *eventstore.Store) []eventhandler.Handler {
	mux.HandleFunc("POST /append/{stream}", func(w http.ResponseWriter, r *http.Request) {
		appendEvent(w, store, r.PathValue("stream"))
	})
	return nil
}

// withHandlers is a test feature that only contributes handlers.
func withHandlers(handlers ...eventhandler.Handler) agent.Feature {
	return func(*http.ServeMux, *eventstore.Store) []eventhandler.Handler { return handlers }
}

// blocking is a test feature serving POST /block, which waits for release
// before appending to stream "blocked".
func blocking() (feature agent.Feature, entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}, 1), make(chan struct{})
	feature = func(mux *http.ServeMux, store *eventstore.Store) []eventhandler.Handler {
		mux.HandleFunc("POST /block", func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			<-release
			appendEvent(w, store, "blocked")
		})
		return nil
	}
	return feature, entered, release
}

// recorder forwards every handled event's position to a channel.
type recorder struct{ handled chan uint64 }

func newRecorder() *recorder { return &recorder{handled: make(chan uint64, 100)} }

func (*recorder) Name() string                { return "recorder" }
func (r *recorder) Handle(e eventstore.Event) { r.handled <- e.Position }

func (r *recorder) expect(t *testing.T, want uint64) {
	t.Helper()
	select {
	case got := <-r.handled:
		if got != want {
			t.Fatalf("handled position %d, want %d", got, want)
		}
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for position %d", want)
	}
}

// lineWriter forwards each log line to a channel.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func waitForLog(t *testing.T, lines lineWriter, msg string) {
	t.Helper()
	timeout := time.After(waitLimit)
	for {
		select {
		case line := <-lines:
			var entry struct{ Msg string }
			if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.Msg == msg {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for log %q", msg)
		}
	}
}

func TestEventsAreHandledOnceAcrossRestarts(t *testing.T) {
	cfg := agent.Config{DataDir: t.TempDir(), ShutdownTimeout: time.Second}

	for run, stream := range []string{"first", "second"} {
		rec := newRecorder()
		inst := start(t, cfg, []agent.Feature{appender, withHandlers(rec)}, discard())

		wantPosition := uint64(run + 1)
		code, body := inst.post(t, "/append/"+stream)
		if code != http.StatusOK || body != strconv.FormatUint(wantPosition, 10) {
			t.Fatalf("run %d: got status %d body %q", run, code, body)
		}
		rec.expect(t, wantPosition)
		if err := inst.stop(t); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if len(rec.handled) != 0 {
			t.Fatalf("run %d: unexpectedly handled position %d", run, <-rec.handled)
		}
	}
}

// midEventHandler blocks while handling the first event it sees.
type midEventHandler struct {
	entered chan uint64
	release chan struct{}
	handled chan uint64
}

func newMidEventHandler() *midEventHandler {
	return &midEventHandler{entered: make(chan uint64, 1), release: make(chan struct{}), handled: make(chan uint64, 100)}
}

func (h *midEventHandler) Name() string { return "projector" }
func (h *midEventHandler) Handle(e eventstore.Event) {
	select {
	case h.entered <- e.Position:
		<-h.release
	default:
	}
	h.handled <- e.Position
}

func TestShutdownDrainsRequestsThenStopsHandlersThenClosesStore(t *testing.T) {
	dir := t.TempDir()
	cfg := agent.Config{DataDir: dir, ShutdownTimeout: waitLimit}
	block, requestEntered, releaseRequest := blocking()
	handler := newMidEventHandler()
	logs := make(lineWriter, 100)
	inst := start(t, cfg, []agent.Feature{appender, block, withHandlers(handler)}, slog.New(slog.NewJSONHandler(logs, nil)))

	for _, stream := range []string{"a", "b", "c"} {
		if code, body := inst.post(t, "/append/"+stream); code != http.StatusOK {
			t.Fatalf("append %s: got %d (%s)", stream, code, body)
		}
	}
	<-handler.entered // handler is now mid-event at position 1

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
		t.Fatalf("in-flight request: got status %d, want 200", code)
	}
	waitForLog(t, logs, "stopping event handlers")
	assertNotDone(t, inst.done, "run returned while a handler was mid-event")
	close(handler.release)
	if err := inst.stop(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := len(handler.handled); got != 1 {
		t.Fatalf("handler processed %d events before stopping, want 1", got)
	}
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	checkpoint, err := s.Checkpoint("projector")
	if err != nil || checkpoint != 1 {
		t.Fatalf("got checkpoint %d err %v, want 1", checkpoint, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resumed := newMidEventHandler()
	resumed.entered <- 0 // never block
	inst = start(t, cfg, []agent.Feature{withHandlers(resumed)}, discard())
	for _, want := range []uint64{2, 3, 4} {
		select {
		case got := <-resumed.handled:
			if got != want {
				t.Fatalf("resumed handler got position %d, want %d", got, want)
			}
		case <-time.After(waitLimit):
			t.Fatalf("timed out waiting for position %d", want)
		}
	}
	if err := inst.stop(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestShutdownCompletesWhenRequestOutlivesTimeout(t *testing.T) {
	dir := t.TempDir()
	cfg := agent.Config{DataDir: dir, ShutdownTimeout: 10 * time.Millisecond}
	block, requestEntered, releaseRequest := blocking()
	inst := start(t, cfg, []agent.Feature{block}, discard())

	inFlight := make(chan struct{})
	go func() {
		defer close(inFlight)
		resp, err := http.Post(inst.url+"/block", "text/plain", nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-requestEntered

	_ = inst.stop(t) // must return although the request is still blocked

	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	close(releaseRequest)
	<-inFlight
}

func TestRunFailsWhenStoreCannotOpen(t *testing.T) {
	dir := t.TempDir()
	held, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer held.Close()

	inst := start(t, agent.Config{DataDir: dir, ShutdownTimeout: time.Second}, nil, discard())
	select {
	case err := <-inst.done:
		if err == nil {
			t.Fatal("run succeeded, want error")
		}
	case <-time.After(waitLimit):
		t.Fatal("run did not fail fast")
	}
	inst.waitUntilListenerClosed(t)
}

// waitUntilListenerClosed waits until the agent stops accepting connections,
// i.e. until shutdown has begun.
func (i *instance) waitUntilListenerClosed(t *testing.T) {
	t.Helper()
	select {
	case <-i.listenerClosed:
	case <-time.After(waitLimit):
		t.Fatal("listener still open")
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
