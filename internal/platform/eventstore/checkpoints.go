package eventstore

import (
	"encoding/binary"
	"time"

	bolt "go.etcd.io/bbolt"
)

var checkpointsBucket = []byte("checkpoints")

// checkpoints maps handler names to 8-byte big-endian positions.
type checkpoints struct {
	db *bolt.DB
}

func openCheckpoints(path string) (*checkpoints, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(checkpointsBucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &checkpoints{db: db}, nil
}

func (c *checkpoints) close() error { return c.db.Close() }

func (c *checkpoints) get(name string) (uint64, error) {
	var position uint64
	err := c.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(checkpointsBucket).Get([]byte(name)); len(v) == 8 {
			position = binary.BigEndian.Uint64(v)
		}
		return nil
	})
	return position, err
}

func (c *checkpoints) set(name string, position uint64) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(checkpointsBucket).Put([]byte(name), binary.BigEndian.AppendUint64(nil, position))
	})
}
