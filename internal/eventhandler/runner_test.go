package eventhandler_test

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
)

// waitLimit only guards against hangs; tests never sleep.
const waitLimit = 5 * time.Second

// recorder forwards every handled event's position to a channel.
type recorder struct {
	name    string
	handled chan uint64
}

func newRecorder() *recorder {
	return &recorder{name: "recorder", handled: make(chan uint64, 100)}
}

func (r *recorder) Name() string              { return r.name }
func (r *recorder) Handle(e eventstore.Event) { r.handled <- e.Position }
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

func (r *recorder) expectNothingMore(t *testing.T) {
	t.Helper()
	select {
	case got := <-r.handled:
		t.Fatalf("unexpectedly handled position %d", got)
	default:
	}
}

func openStore(t *testing.T, dir string) *eventstore.Store {
	t.Helper()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func appendNotes(t *testing.T, s *eventstore.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		event := eventstore.NewEvent{Type: "NoteRegistered", Payload: json.RawMessage(`{}`)}
		if _, _, err := s.Append(id, 0, []eventstore.NewEvent{event}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
}

func checkpoint(t *testing.T, s *eventstore.Store, name string) uint64 {
	t.Helper()
	pos, err := s.Checkpoint(name)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return pos
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRunnerCatchesUpThenFollowsNewEvents(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	appendNotes(t, s, "a", "b")
	rec := newRecorder()

	runner := eventhandler.Start(s, []eventhandler.Handler{rec}, discard())
	rec.expect(t, 1, 2)
	appendNotes(t, s, "c")
	rec.expect(t, 3)
	if err := runner.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := checkpoint(t, s, "recorder"); got != 3 {
		t.Fatalf("got checkpoint %d, want 3", got)
	}
	rec.expectNothingMore(t)
}

func TestRunnerResumesFromCheckpointAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	appendNotes(t, s, "a", "b")
	first := newRecorder()
	runner := eventhandler.Start(s, []eventhandler.Handler{first}, discard())
	first.expect(t, 1, 2)
	if err := runner.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s = openStore(t, dir)
	defer s.Close()
	appendNotes(t, s, "c", "d")
	second := newRecorder()
	runner = eventhandler.Start(s, []eventhandler.Handler{second}, discard())
	second.expect(t, 3, 4)
	if err := runner.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	second.expectNothingMore(t)
	if got := checkpoint(t, s, "recorder"); got != 4 {
		t.Fatalf("got checkpoint %d, want 4", got)
	}
}

func TestEachHandlerKeepsItsOwnCheckpoint(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	appendNotes(t, s, "a", "b")
	if err := s.SetCheckpoint("ahead", 1); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}
	behind := newRecorder()
	ahead := &recorder{name: "ahead", handled: make(chan uint64, 100)}

	runner := eventhandler.Start(s, []eventhandler.Handler{behind, ahead}, discard())
	behind.expect(t, 1, 2)
	ahead.expect(t, 2)
	if err := runner.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	ahead.expectNothingMore(t)
}

// logLines forwards each log line to a channel.
type logLines chan string

func (l logLines) Write(p []byte) (int, error) {
	l <- string(p)
	return len(p), nil
}

func (l logLines) waitFor(t *testing.T, msg string) {
	t.Helper()
	timeout := time.After(waitLimit)
	for {
		select {
		case line := <-l:
			var entry struct{ Msg string }
			if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.Msg == msg {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for log %q", msg)
		}
	}
}

// blocker blocks while handling its first event until released.
type blocker struct {
	entered chan uint64
	release chan struct{}
	handled chan uint64
}

func (b *blocker) Name() string { return "blocker" }
func (b *blocker) Handle(e eventstore.Event) {
	select {
	case b.entered <- e.Position:
		<-b.release
	default:
	}
	b.handled <- e.Position
}

func TestStopFinishesAndCheckpointsCurrentEventOnly(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	appendNotes(t, s, "a", "b", "c")
	b := &blocker{entered: make(chan uint64, 1), release: make(chan struct{}), handled: make(chan uint64, 100)}
	logs := make(logLines, 100)
	runner := eventhandler.Start(s, []eventhandler.Handler{b}, slog.New(slog.NewJSONHandler(logs, nil)))
	<-b.entered

	stopped := make(chan error, 1)
	go func() { stopped <- runner.Stop() }()
	logs.waitFor(t, "stopping event handlers")
	select {
	case <-stopped:
		t.Fatal("stop returned while handler was mid-event")
	default:
	}
	close(b.release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("stop did not return")
	}

	if got := checkpoint(t, s, "blocker"); got != 1 {
		t.Fatalf("got checkpoint %d, want 1", got)
	}
	if got := len(b.handled); got != 1 {
		t.Fatalf("handled %d events, want 1", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s = openStore(t, dir)
	defer s.Close()
	rec := &recorder{name: "blocker", handled: make(chan uint64, 100)}
	runner = eventhandler.Start(s, []eventhandler.Handler{rec}, discard())
	rec.expect(t, 2, 3)
	if err := runner.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
