// Package eventstore is underling's embedded event store. Events live in
// hallgren/eventsourcing's bbolt store (events.db); the per-handler
// checkpoints live in a bbolt file of their own (checkpoints.db).
//
// Each stream maps to one hallgren aggregate: AggregateType is always
// "underling", AggregateID is the stream, Reason is the event type, Data is
// the JSON payload as-is and GlobalVersion is the global position.
package eventstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hallgren/eventsourcing/core"
	esbbolt "github.com/hallgren/eventsourcing/eventstore/bbolt"
)

// File names inside the data dir. An old underling.db is never opened.
const (
	EventsFile      = "events.db"
	CheckpointsFile = "checkpoints.db"
)

const aggregateType = "underling"

// ErrConflict is returned by Append when the stream is not at the expected
// version. Nothing is stored then.
var ErrConflict = errors.New("stream version conflict")

// Event is a stored event.
type Event struct {
	Stream    string
	Version   uint64
	Position  uint64
	Type      string
	Timestamp time.Time
	Payload   json.RawMessage
}

// NewEvent is an event to append.
type NewEvent struct {
	Type    string
	Payload json.RawMessage
}

// Store is the event store of one data dir. It is safe for concurrent use.
type Store struct {
	events      *esbbolt.BBolt
	checkpoints *checkpoints
	now         func() time.Time

	mu          sync.Mutex
	subscribers []chan struct{}
}

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock that timestamps appended events.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Open opens the store in dir, creating dir and its files if needed. It
// fails within a second if another process holds the store.
func Open(dir string, opts ...Option) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	events, err := esbbolt.New(filepath.Join(dir, EventsFile))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", EventsFile, err)
	}
	cps, err := openCheckpoints(filepath.Join(dir, CheckpointsFile))
	if err != nil {
		_ = events.Close()
		return nil, fmt.Errorf("open %s: %w", CheckpointsFile, err)
	}
	s := &Store{events: events, checkpoints: cps, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Close closes both files.
func (s *Store) Close() error {
	return errors.Join(s.events.Close(), s.checkpoints.close())
}

// Append appends events to stream if the stream is at expectedVersion (0
// for a new stream). It returns the stream's new version and the global
// position of the last event appended.
func (s *Store) Append(stream string, expectedVersion uint64, events []NewEvent) (version, position uint64, err error) {
	if len(events) == 0 {
		return 0, 0, errors.New("append: no events")
	}
	ts := s.now().UTC()
	batch := make([]core.Event, len(events))
	for i, e := range events {
		batch[i] = core.Event{
			AggregateID:   stream,
			AggregateType: aggregateType,
			Version:       core.Version(expectedVersion + 1 + uint64(i)),
			Reason:        e.Type,
			Timestamp:     ts,
			Data:          e.Payload,
		}
	}
	err = s.events.Save(batch)
	if errors.Is(err, core.ErrConcurrency) {
		return 0, 0, fmt.Errorf("%w: stream %q is not at version %d", ErrConflict, stream, expectedVersion)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("append to %q: %w", stream, err)
	}
	s.wake()
	last := batch[len(batch)-1]
	return uint64(last.Version), uint64(last.GlobalVersion), nil
}

// ReadAfter returns up to limit events after position, in global order.
func (s *Store) ReadAfter(position uint64, limit int) ([]Event, error) {
	// A fresh fetcher each time: hallgren's fetcher remembers where it
	// stopped. Its iterator holds a bbolt read transaction, which would block
	// Close, so it is closed before returning.
	it, err := s.events.All(core.Version(position))()
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer it.Close()
	var out []Event
	for len(out) < limit && it.Next() {
		e, err := it.Value()
		if err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		out = append(out, Event{
			Stream:    e.AggregateID,
			Version:   uint64(e.Version),
			Position:  uint64(e.GlobalVersion),
			Type:      e.Reason,
			Timestamp: e.Timestamp,
			Payload:   json.RawMessage(e.Data),
		})
	}
	return out, nil
}

// Checkpoint returns the last position the named handler processed, or 0.
func (s *Store) Checkpoint(name string) (uint64, error) {
	return s.checkpoints.get(name)
}

// SetCheckpoint records position as the last one the named handler
// processed.
func (s *Store) SetCheckpoint(name string, position uint64) error {
	return s.checkpoints.set(name, position)
}

// Subscribe returns a channel that is signalled after each successful
// Append. Signals coalesce, so a receiver must read until it catches up.
func (s *Store) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subscribers = append(s.subscribers, ch)
	s.mu.Unlock()
	return ch
}

func (s *Store) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
