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

	"github.com/hallgren/eventsourcing/core"
	esbbolt "github.com/hallgren/eventsourcing/eventstore/bbolt"
	bolt "go.etcd.io/bbolt"

	"underling/internal/platform/eventstore"
)

func openStore(t *testing.T, dir string, opts ...eventstore.Option) *eventstore.Store {
	t.Helper()
	s, err := eventstore.Open(dir, opts...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func note(text string) eventstore.NewEvent {
	return eventstore.NewEvent{Type: "NoteRegistered", Payload: json.RawMessage(`{"text":"` + text + `"}`)}
}

func appendOK(t *testing.T, s *eventstore.Store, stream string, expected uint64, events ...eventstore.NewEvent) (uint64, uint64) {
	t.Helper()
	version, position, err := s.Append(stream, expected, events)
	if err != nil {
		t.Fatalf("append to %s: %v", stream, err)
	}
	return version, position
}

func TestAppendingToANewStreamStartsAtVersionAndPositionOne(t *testing.T) {
	s := openStore(t, t.TempDir())

	version, position := appendOK(t, s, "a", 0, note("hi"))

	if version != 1 || position != 1 {
		t.Fatalf("got version %d position %d, want 1 1", version, position)
	}
}

func TestAnExpectedVersionMismatchIsAConflictThatStoresNothing(t *testing.T) {
	s := openStore(t, t.TempDir())
	appendOK(t, s, "a", 0, note("first"))
	wake := s.Subscribe()

	_, _, err := s.Append("a", 0, []eventstore.NewEvent{note("second")})

	if !errors.Is(err, eventstore.ErrConflict) || !bytes.Contains([]byte(err.Error()), []byte(`"a"`)) {
		t.Fatalf("got %v, want ErrConflict naming the stream", err)
	}
	if pending(wake) {
		t.Fatal("a rejected append woke the handlers")
	}
	if version, position := appendOK(t, s, "b", 0, note("third")); version != 1 || position != 2 {
		t.Fatalf("got version %d position %d, want 1 2", version, position)
	}
}

func TestAnEmptyAppendIsAnError(t *testing.T) {
	s := openStore(t, t.TempDir())

	if _, _, err := s.Append("a", 0, nil); err == nil {
		t.Fatal("empty append succeeded")
	}
	appendOK(t, s, "a", 0, note("hi"))
}

func TestReadAfterReturnsStoredEventsInGlobalOrder(t *testing.T) {
	zurich := time.FixedZone("CET", 3600)
	clock := time.Date(2026, 10, 24, 12, 0, 0, 5, zurich)
	s := openStore(t, t.TempDir(), eventstore.WithClock(func() time.Time { return clock }))
	appendOK(t, s, "a", 0, note("one"), note("two"))
	appendOK(t, s, "b", 0, note("three"))
	if version, position := appendOK(t, s, "a", 2, note("four")); version != 3 || position != 4 {
		t.Fatalf("got version %d position %d, want 3 4", version, position)
	}

	got, err := s.ReadAfter(1, 1)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []eventstore.Event{{
		Stream:    "a",
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
	var positions []uint64
	for _, e := range all {
		positions = append(positions, e.Position)
	}
	if !reflect.DeepEqual(positions, []uint64{1, 2, 3, 4}) || all[2].Stream != "b" {
		t.Fatalf("got %+v, want positions 1..4", all)
	}
	if none, err := s.ReadAfter(4, 10); err != nil || len(none) != 0 {
		t.Fatalf("got %+v, %v; want no events", none, err)
	}
}

func TestCheckpointsDefaultToZeroAndPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := eventstore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, err := s.Checkpoint("logger"); err != nil || got != 0 {
		t.Fatalf("got %d, %v; want 0", got, err)
	}
	if err := s.SetCheckpoint("logger", 7); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s = openStore(t, dir)
	if got, err := s.Checkpoint("logger"); err != nil || got != 7 {
		t.Fatalf("got %d, %v; want 7", got, err)
	}
	if got, err := s.Checkpoint("other"); err != nil || got != 0 {
		t.Fatalf("got %d, %v; want 0", got, err)
	}
}

func TestOnDiskLayout(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 10, 24, 11, 0, 0, 5, time.UTC)
	s, err := eventstore.Open(dir, eventstore.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	appendOK(t, s, "a", 0, note("hi"))
	if err := s.SetCheckpoint("logger", 1); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	events, err := esbbolt.New(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatalf("open events.db with hallgren: %v", err)
	}
	it, err := events.All(0)()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !it.Next() {
		t.Fatal("events.db holds no event")
	}
	got, err := it.Value()
	it.Close()
	_ = events.Close()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := core.Event{AggregateID: "a", AggregateType: "underling", Version: 1, GlobalVersion: 1, Reason: "NoteRegistered", Timestamp: clock, Data: []byte(`{"text":"hi"}`)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	db, err := bolt.Open(filepath.Join(dir, "checkpoints.db"), 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("open checkpoints.db: %v", err)
	}
	defer db.Close()
	_ = db.View(func(tx *bolt.Tx) error {
		if got := tx.Bucket([]byte("checkpoints")).Get([]byte("logger")); !bytes.Equal(got, []byte{0, 0, 0, 0, 0, 0, 0, 1}) {
			t.Errorf("checkpoint: got %v, want 8-byte big-endian 1", got)
		}
		return nil
	})
	if info, err := os.Stat(filepath.Join(dir, "checkpoints.db")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("checkpoints.db mode %v (error %v), want 0600", info.Mode().Perm(), err)
	}
}

func TestAnOldUnderlingDBIsLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	junk := []byte("not a bbolt file")
	old := filepath.Join(dir, "underling.db")
	if err := os.WriteFile(old, junk, 0o600); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, dir)
	appendOK(t, s, "a", 0, note("hi"))

	if got, err := os.ReadFile(old); err != nil || !bytes.Equal(got, junk) {
		t.Fatalf("underling.db is now %q (error %v), want it untouched", got, err)
	}
}

func TestOpenCreatesAMissingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")

	openStore(t, dir)

	for _, file := range []string{"events.db", "checkpoints.db"} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Errorf("stat %s: %v", file, err)
		}
	}
}

func TestASecondOpenFailsFast(t *testing.T) {
	dir := t.TempDir()
	openStore(t, dir)

	start := time.Now()
	second, err := eventstore.Open(dir)

	if err == nil {
		_ = second.Close()
		t.Fatal("second open succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("second open took %v", elapsed)
	}
}

func TestASecondOpenBlockedOnCheckpointsReleasesTheEventsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := bolt.Open(filepath.Join(dir, "checkpoints.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}

	if s, err := eventstore.Open(dir); err == nil {
		_ = s.Close()
		t.Fatal("open succeeded while checkpoints.db is held")
	}
	_ = held.Close()

	openStore(t, dir) // would time out if events.db were still held
}

func TestAppendWakesSubscribersWithCoalescedSignals(t *testing.T) {
	s := openStore(t, t.TempDir())
	first, second := s.Subscribe(), s.Subscribe()

	appendOK(t, s, "a", 0, note("a"))
	appendOK(t, s, "b", 0, note("b"))

	for _, ch := range []<-chan struct{}{first, second} {
		if !pending(ch) {
			t.Fatal("subscriber not woken")
		}
		if pending(ch) {
			t.Fatal("signals not coalesced")
		}
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

func TestConcurrentAppendsToANewStreamAdmitExactlyOne(t *testing.T) {
	s := openStore(t, t.TempDir())
	const writers = 50

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Go(func() {
			_, _, err := s.Append("same", 0, []eventstore.NewEvent{note("x")})
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

func TestCloseIsNotBlockedByAnEarlierRead(t *testing.T) {
	s, err := eventstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	appendOK(t, s, "a", 0, note("a"), note("b"))
	if _, err := s.ReadAfter(0, 1); err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close blocked: a read left a transaction open")
	}
}
