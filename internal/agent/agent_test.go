package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"heinzel/internal/agent"
	"heinzel/internal/command"
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

func start(t *testing.T, cfg agent.Config, registry *command.Registry, handlers []eventhandler.Handler, logger *slog.Logger) *instance {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	nl := &notifyingListener{Listener: ln, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{url: "http://" + ln.Addr().String(), cancel: cancel, done: make(chan error, 1), listenerClosed: nl.closed}
	go func() { inst.done <- agent.Run(ctx, cfg, nl, registry, handlers, logger) }()
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

func (i *instance) post(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(i.url+"/commands", "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("post: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Errorf("decode response: %v", err)
	}
	return resp.StatusCode, decoded
}

func registerNote(id string) string {
	return fmt.Sprintf(`{"type":"RegisterNote","payload":{"id":%q,"text":"hello"}}`, id)
}

// lineWriter forwards each log line to a channel.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func nextLine(t *testing.T, lines lineWriter) map[string]any {
	t.Helper()
	select {
	case line := <-lines:
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		return decoded
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a log line")
		return nil
	}
}

func waitForLog(t *testing.T, lines lineWriter, msg string) {
	t.Helper()
	for {
		if nextLine(t, lines)["msg"] == msg {
			return
		}
	}
}

func TestRegisteredNoteIsLoggedOnceAcrossRestarts(t *testing.T) {
	cfg := agent.Config{DataDir: t.TempDir(), ShutdownTimeout: time.Second}

	for run, id := range []string{"first", "second"} {
		lines := make(lineWriter, 100)
		logger := slog.New(slog.NewJSONHandler(lines, nil))
		inst := start(t, cfg, command.DefaultRegistry(), []eventhandler.Handler{eventhandler.NewEventLogger(logger)}, discard())

		code, body := inst.post(t, registerNote(id))
		if code != http.StatusCreated {
			t.Fatalf("run %d: got status %d (%v), want 201", run, code, body)
		}
		wantPosition := float64(run + 1)
		if body["streamId"] != id || body["version"] != 1.0 || body["position"] != wantPosition {
			t.Fatalf("run %d: got body %v", run, body)
		}
		line := nextLine(t, lines)
		if line["streamId"] != id || line["position"] != wantPosition || line["handler"] != "event-logger" {
			t.Fatalf("run %d: got log line %v", run, line)
		}
		if err := inst.stop(t); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if len(lines) != 0 {
			t.Fatalf("run %d: unexpected extra log line %q", run, <-lines)
		}
	}
}

// blockingCommand registers a "Block" command that waits for release before
// deciding to register a note on stream "blocked".
func blockingCommand(registry *command.Registry) (entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}, 1), make(chan struct{})
	registry.Register("Block", func(context.Context, json.RawMessage) (command.Decision, error) {
		entered <- struct{}{}
		<-release
		return command.RegisterNote(context.Background(), json.RawMessage(`{"id":"blocked","text":"late"}`))
	})
	return entered, release
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
	registry := command.DefaultRegistry()
	commandEntered, releaseCommand := blockingCommand(registry)
	handler := newMidEventHandler()
	logs := make(lineWriter, 100)
	inst := start(t, cfg, registry, []eventhandler.Handler{handler}, slog.New(slog.NewJSONHandler(logs, nil)))

	for _, id := range []string{"a", "b", "c"} {
		if code, body := inst.post(t, registerNote(id)); code != http.StatusCreated {
			t.Fatalf("register %s: got %d (%v)", id, code, body)
		}
	}
	<-handler.entered // handler is now mid-event at position 1

	inFlight := make(chan int, 1)
	go func() {
		code, _ := inst.post(t, `{"type":"Block","payload":{}}`)
		inFlight <- code
	}()
	<-commandEntered
	inst.cancel()
	inst.waitUntilListenerClosed(t)

	assertNotDone(t, inst.done, "run returned before the in-flight request finished")
	close(releaseCommand)
	if code := <-inFlight; code != http.StatusCreated {
		t.Fatalf("in-flight request: got status %d, want 201", code)
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
	inst = start(t, cfg, command.DefaultRegistry(), []eventhandler.Handler{resumed}, discard())
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
	registry := command.DefaultRegistry()
	commandEntered, releaseCommand := blockingCommand(registry)
	inst := start(t, cfg, registry, nil, discard())

	inFlight := make(chan struct{})
	go func() {
		defer close(inFlight)
		resp, err := http.Post(inst.url+"/commands", "application/json", strings.NewReader(`{"type":"Block","payload":{}}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-commandEntered

	_ = inst.stop(t) // must return although the request is still blocked

	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	close(releaseCommand)
	<-inFlight
}

func TestRunFailsWhenStoreCannotOpen(t *testing.T) {
	dir := t.TempDir()
	held, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer held.Close()

	inst := start(t, agent.Config{DataDir: dir, ShutdownTimeout: time.Second}, command.DefaultRegistry(), nil, discard())
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
