package eventhandler_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"underling/internal/platform/eventhandler"
	"underling/internal/platform/eventstore"
)

// waitLimit only guards against hangs; the tests never sleep.
const waitLimit = 5 * time.Second

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// recorder passes the position of each event it handles to a channel.
type recorder struct {
	name    string
	handled chan uint64
}

func newRecorder(name string) *recorder {
	return &recorder{name: name, handled: make(chan uint64, 100)}
}

func (r *recorder) Name() string              { return r.name }
func (r *recorder) Handle(e eventstore.Event) { r.handled <- e.Position }

func (r *recorder) expect(t *testing.T, want ...uint64) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-r.handled:
			if got != w {
				t.Fatalf("%s handled position %d, want %d", r.name, got, w)
			}
		case <-time.After(waitLimit):
			t.Fatalf("%s: timed out waiting for position %d", r.name, w)
		}
	}
}

func (r *recorder) expectNothingMore(t *testing.T) {
	t.Helper()
	select {
	case got := <-r.handled:
		t.Fatalf("%s unexpectedly handled position %d", r.name, got)
	default:
	}
}

func openStore(t *testing.T, dir string) *eventstore.Store {
	t.Helper()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendNotes(t *testing.T, s *eventstore.Store, streams ...string) {
	t.Helper()
	for _, stream := range streams {
		if _, _, err := s.Append(stream, 0, []eventstore.NewEvent{{Type: "NoteRegistered", Payload: json.RawMessage(`{}`)}}); err != nil {
			t.Fatalf("append %s: %v", stream, err)
		}
	}
}

func checkpoint(t *testing.T, s *eventstore.Store, name string) uint64 {
	t.Helper()
	position, err := s.Checkpoint(name)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return position
}

func stop(t *testing.T, r *eventhandler.Observer) {
	t.Helper()
	if err := r.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestARunnerCatchesUpThenFollowsNewEvents(t *testing.T) {
	s := openStore(t, t.TempDir())
	appendNotes(t, s, "a", "b")
	rec := newRecorder("recorder")

	runner := eventhandler.Start(s, []eventhandler.Handler{rec}, discard())
	rec.expect(t, 1, 2)
	appendNotes(t, s, "c")
	rec.expect(t, 3)
	stop(t, runner)

	if got := checkpoint(t, s, "recorder"); got != 3 {
		t.Fatalf("checkpoint %d, want 3", got)
	}
	rec.expectNothingMore(t)
}

func TestARestartResumesAfterTheCheckpoint(t *testing.T) {
	dir := t.TempDir()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	appendNotes(t, s, "a", "b")
	first := newRecorder("recorder")
	runner := eventhandler.Start(s, []eventhandler.Handler{first}, discard())
	first.expect(t, 1, 2)
	stop(t, runner)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, dir)
	appendNotes(t, s, "c", "d")
	second := newRecorder("recorder")
	runner = eventhandler.Start(s, []eventhandler.Handler{second}, discard())
	second.expect(t, 3, 4)
	stop(t, runner)

	if got := checkpoint(t, s, "recorder"); got != 4 {
		t.Fatalf("checkpoint %d, want 4", got)
	}
	second.expectNothingMore(t)
}

func TestEachHandlerKeepsItsOwnCheckpoint(t *testing.T) {
	s := openStore(t, t.TempDir())
	appendNotes(t, s, "a", "b")
	if err := s.SetCheckpoint("ahead", 1); err != nil {
		t.Fatal(err)
	}
	behind, ahead := newRecorder("behind"), newRecorder("ahead")

	runner := eventhandler.Start(s, []eventhandler.Handler{behind, ahead}, discard())
	behind.expect(t, 1, 2)
	ahead.expect(t, 2)
	stop(t, runner)

	ahead.expectNothingMore(t)
}

// logLines passes each log line to a channel.
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
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == msg {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for log %q", msg)
		}
	}
}

// blocker blocks in its first Handle until released.
type blocker struct {
	entered chan uint64
	release chan struct{}
	handled chan uint64
}

func (*blocker) Name() string { return "blocker" }
func (b *blocker) Handle(e eventstore.Event) {
	select {
	case b.entered <- e.Position:
		<-b.release
	default:
	}
	b.handled <- e.Position
}

func TestStopLetsTheCurrentEventFinishAndLeavesTheRest(t *testing.T) {
	dir := t.TempDir()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatal("stop returned while the handler was mid-event")
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
		t.Fatalf("checkpoint %d, want 1", got)
	}
	if got := len(b.handled); got != 1 {
		t.Fatalf("handled %d events, want 1", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, dir)
	rec := newRecorder("blocker")
	runner = eventhandler.Start(s, []eventhandler.Handler{rec}, discard())
	rec.expect(t, 2, 3)
	stop(t, runner)
}

// brokenCheckpoints is a store whose checkpoints can't be saved.
type brokenCheckpoints struct{ *eventstore.Store }

func (brokenCheckpoints) SetCheckpoint(string, uint64) error { return errors.New("disk full") }

func TestAFailingLoopIsLoggedAndReportedByStop(t *testing.T) {
	s := openStore(t, t.TempDir())
	appendNotes(t, s, "a")
	logs := make(logLines, 100)
	rec := newRecorder("recorder")

	runner := eventhandler.Start(brokenCheckpoints{s}, []eventhandler.Handler{rec}, slog.New(slog.NewJSONHandler(logs, nil)))
	logs.waitFor(t, "event handler stopped")

	if err := runner.Stop(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("stop returned %v, want the loop's error", err)
	}
}
