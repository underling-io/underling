package eventstore_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"heinzel/internal/eventstore"
)

func openStore(t *testing.T, dir string) *eventstore.Store {
	t.Helper()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func noteEvent(text string) eventstore.NewEvent {
	return eventstore.NewEvent{Type: "NoteRegistered", Payload: json.RawMessage(`{"text":"` + text + `"}`)}
}

func TestAppendToNewStreamStartsAtVersionAndPositionOne(t *testing.T) {
	s := openStore(t, t.TempDir())

	version, position, err := s.Append("a", 0, []eventstore.NewEvent{noteEvent("hi")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if version != 1 || position != 1 {
		t.Fatalf("got version %d position %d, want 1 1", version, position)
	}
}

func TestAppendRejectsExpectedVersionMismatchWithoutWriting(t *testing.T) {
	s := openStore(t, t.TempDir())
	if _, _, err := s.Append("a", 0, []eventstore.NewEvent{noteEvent("first")}); err != nil {
		t.Fatalf("append: %v", err)
	}

	_, _, err := s.Append("a", 0, []eventstore.NewEvent{noteEvent("second")})
	if !errors.Is(err, eventstore.ErrConflict) {
		t.Fatalf("got err %v, want ErrConflict", err)
	}

	version, position, err := s.Append("b", 0, []eventstore.NewEvent{noteEvent("third")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if version != 1 || position != 2 {
		t.Fatalf("got version %d position %d, want 1 2", version, position)
	}
}

func TestReadAfterReturnsStoredEventsInGlobalOrder(t *testing.T) {
	zurich := time.FixedZone("CET", 3600)
	clock := time.Date(2026, 10, 24, 12, 0, 0, 5, zurich)
	s, err := eventstore.Open(t.TempDir(), eventstore.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, _, err := s.Append("a", 0, []eventstore.NewEvent{noteEvent("one"), noteEvent("two")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	version, position, err := s.Append("a", 2, []eventstore.NewEvent{noteEvent("three")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if version != 3 || position != 3 {
		t.Fatalf("got version %d position %d, want 3 3", version, position)
	}

	got, err := s.ReadAfter(1, 1)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []eventstore.Event{{
		StreamID:  "a",
		Version:   2,
		Position:  2,
		Type:      "NoteRegistered",
		Timestamp: time.Date(2026, 10, 24, 11, 0, 0, 5, time.UTC),
		Payload:   json.RawMessage(`{"text":"two"}`),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	all, err := s.ReadAfter(0, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(all) != 3 || all[0].Position != 1 || all[2].Position != 3 {
		t.Fatalf("got %+v, want positions 1..3", all)
	}

	none, err := s.ReadAfter(3, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("got %+v, want no events", none)
	}
}

func TestCheckpointDefaultsToZeroAndPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	got, err := s.Checkpoint("event-logger")
	if err != nil || got != 0 {
		t.Fatalf("got checkpoint %d err %v, want 0 nil", got, err)
	}
	if err := s.SetCheckpoint("event-logger", 7); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s = openStore(t, dir)
	got, err = s.Checkpoint("event-logger")
	if err != nil || got != 7 {
		t.Fatalf("got checkpoint %d err %v, want 7 nil", got, err)
	}
	other, err := s.Checkpoint("other")
	if err != nil || other != 0 {
		t.Fatalf("got checkpoint %d err %v, want 0 nil", other, err)
	}
}

func TestPersistedLayout(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 10, 24, 11, 0, 0, 5, time.UTC)
	s, err := eventstore.Open(dir, eventstore.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := s.Append("a", 0, []eventstore.NewEvent{noteEvent("hi")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.SetCheckpoint("event-logger", 1); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := bolt.Open(filepath.Join(dir, "heinzel.db"), 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	defer db.Close()
	one := []byte{0, 0, 0, 0, 0, 0, 0, 1}
	err = db.View(func(tx *bolt.Tx) error {
		var event map[string]any
		if err := json.Unmarshal(tx.Bucket([]byte("events")).Get(one), &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		want := map[string]any{
			"streamId":  "a",
			"version":   1.0,
			"position":  1.0,
			"type":      "NoteRegistered",
			"timestamp": "2026-10-24T11:00:00.000000005Z",
			"payload":   map[string]any{"text": "hi"},
		}
		if !reflect.DeepEqual(event, want) {
			t.Errorf("event: got %v, want %v", event, want)
		}
		if got := tx.Bucket([]byte("streams")).Get([]byte("a")); !bytes.Equal(got, one) {
			t.Errorf("stream version: got %v, want %v", got, one)
		}
		if got := tx.Bucket([]byte("checkpoints")).Get([]byte("event-logger")); !bytes.Equal(got, one) {
			t.Errorf("checkpoint: got %v, want %v", got, one)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestOpenFailsFastWhenAlreadyOpen(t *testing.T) {
	dir := t.TempDir()
	openStore(t, dir)

	second, err := eventstore.Open(dir)
	if err == nil {
		_ = second.Close()
		t.Fatal("second open succeeded, want error")
	}
}

func TestOpenCreatesMissingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	openStore(t, dir)

	if _, err := os.Stat(filepath.Join(dir, "heinzel.db")); err != nil {
		t.Fatalf("stat db file: %v", err)
	}
}

func TestAppendNotifiesSubscribersWithCoalescedWakeups(t *testing.T) {
	s := openStore(t, t.TempDir())
	first, second := s.Subscribe(), s.Subscribe()

	for _, id := range []string{"a", "b"} {
		if _, _, err := s.Append(id, 0, []eventstore.NewEvent{noteEvent(id)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	for _, ch := range []<-chan struct{}{first, second} {
		if !pending(ch) {
			t.Fatal("subscriber not notified")
		}
		if pending(ch) {
			t.Fatal("wakeups not coalesced")
		}
	}

	if _, _, err := s.Append("a", 0, []eventstore.NewEvent{noteEvent("again")}); !errors.Is(err, eventstore.ErrConflict) {
		t.Fatalf("got err %v, want ErrConflict", err)
	}
	if pending(first) {
		t.Fatal("notified after rejected append")
	}
}

func pending(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestAppendRequiresEvents(t *testing.T) {
	s := openStore(t, t.TempDir())

	if _, _, err := s.Append("a", 0, nil); err == nil {
		t.Fatal("append without events succeeded, want error")
	}
	if _, _, err := s.Append("a", 0, []eventstore.NewEvent{noteEvent("hi")}); err != nil {
		t.Fatalf("append after empty append: %v", err)
	}
}

func TestConcurrentAppendsToNewStreamAdmitExactlyOne(t *testing.T) {
	s := openStore(t, t.TempDir())
	const writers = 50

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Go(func() {
			_, _, err := s.Append("same", 0, []eventstore.NewEvent{noteEvent("x")})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)

	var ok, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, eventstore.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflicts != writers-1 {
		t.Fatalf("got %d ok and %d conflicts, want 1 and %d", ok, conflicts, writers-1)
	}
}
