// Package eventstore persists events in a single bbolt file.
//
// The on-disk layout is a locked contract:
//   - events:      8-byte big-endian global position -> JSON Event
//   - streams:     stream ID -> 8-byte big-endian current version
//   - checkpoints: handler name -> 8-byte big-endian last processed position
package eventstore

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// FileName is the name of the bbolt file inside the data directory.
const FileName = "heinzel.db"

const openTimeout = time.Second

var (
	bucketEvents      = []byte("events")
	bucketStreams     = []byte("streams")
	bucketCheckpoints = []byte("checkpoints")
)

// ErrConflict is returned by Append when the stream's current version does not
// match the expected version. Nothing is written in that case.
var ErrConflict = errors.New("stream version conflict")

// Event is a stored event.
type Event struct {
	StreamID  string          `json:"streamId"`
	Version   uint64          `json:"version"`
	Position  uint64          `json:"position"`
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// NewEvent is an event that has not been stored yet.
type NewEvent struct {
	Type    string
	Payload json.RawMessage
}

// Store is a bbolt-backed event store.
type Store struct {
	db  *bolt.DB
	now func() time.Time

	mu          sync.Mutex
	subscribers []chan struct{}
}

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock used to timestamp appended events.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Open opens (creating if needed) the store in dir.
func Open(dir string, opts ...Option) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("open event store: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketEvents, bucketStreams, bucketCheckpoints} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init event store: %w", err)
	}
	s := &Store{db: db, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Close closes the underlying bbolt file.
func (s *Store) Close() error {
	return s.db.Close()
}

// Append appends events to stream if its current version equals expectedVersion
// (0 means the stream must not exist). It returns the stream's new version and
// the global position of the last appended event.
func (s *Store) Append(stream string, expectedVersion uint64, events []NewEvent) (version, position uint64, err error) {
	if len(events) == 0 {
		return 0, 0, errors.New("append: no events")
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		streams := tx.Bucket(bucketStreams)
		eventsBucket := tx.Bucket(bucketEvents)

		version = decodeUint64(streams.Get([]byte(stream)))
		if version != expectedVersion {
			return fmt.Errorf("%w: stream %q is at version %d, expected %d", ErrConflict, stream, version, expectedVersion)
		}
		if lastKey, _ := eventsBucket.Cursor().Last(); lastKey != nil {
			position = decodeUint64(lastKey)
		}
		ts := s.now().UTC()
		for _, e := range events {
			version++
			position++
			data, err := json.Marshal(Event{
				StreamID:  stream,
				Version:   version,
				Position:  position,
				Type:      e.Type,
				Timestamp: ts,
				Payload:   e.Payload,
			})
			if err != nil {
				return err
			}
			if err := eventsBucket.Put(encodeUint64(position), data); err != nil {
				return err
			}
		}
		return streams.Put([]byte(stream), encodeUint64(version))
	})
	if err != nil {
		return 0, 0, err
	}
	s.notify()
	return version, position, nil
}

// Subscribe returns a channel that receives a wakeup after every committed
// Append. Wakeups coalesce: the channel holds at most one pending signal, so a
// receiver must re-read the store after each wakeup.
func (s *Store) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subscribers = append(s.subscribers, ch)
	s.mu.Unlock()
	return ch
}

func (s *Store) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ReadAfter returns up to limit events with a global position greater than
// position, in global order.
func (s *Store) ReadAfter(position uint64, limit int) ([]Event, error) {
	var events []Event
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketEvents).Cursor()
		for k, v := c.Seek(encodeUint64(position + 1)); k != nil && len(events) < limit; k, v = c.Next() {
			var e Event
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("decode event at position %d: %w", decodeUint64(k), err)
			}
			events = append(events, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// Checkpoint returns the last position processed by the named handler, or 0.
func (s *Store) Checkpoint(name string) (uint64, error) {
	var position uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		position = decodeUint64(tx.Bucket(bucketCheckpoints).Get([]byte(name)))
		return nil
	})
	return position, err
}

// SetCheckpoint records position as the last one processed by the named handler.
func (s *Store) SetCheckpoint(name string, position uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCheckpoints).Put([]byte(name), encodeUint64(position))
	})
}

func encodeUint64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func decodeUint64(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
