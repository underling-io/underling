// Package eventhandler runs checkpointed event handlers over the event store.
package eventhandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"heinzel/internal/eventstore"
)

const batchSize = 100

// Handler processes stored events. Its name is its checkpoint key and must
// stay fixed. Handle must not fail; failure handling is out of scope.
type Handler interface {
	Name() string
	Handle(e eventstore.Event)
}

// Source is the part of the event store the handlers read from.
type Source interface {
	ReadAfter(position uint64, limit int) ([]eventstore.Event, error)
	Checkpoint(name string) (uint64, error)
	SetCheckpoint(name string, position uint64) error
	Subscribe() <-chan struct{}
}

// Runner runs one loop per handler until stopped.
type Runner struct {
	logger *slog.Logger
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	errs   []error
}

// Start launches a loop for each handler. Each loop catches up from its
// checkpoint, then follows new events in global order.
func Start(source Source, handlers []Handler, logger *slog.Logger) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{logger: logger, cancel: cancel}
	for _, h := range handlers {
		wake := source.Subscribe()
		r.wg.Go(func() {
			if err := run(ctx, source, h, wake); err != nil {
				logger.Error("event handler stopped", "handler", h.Name(), "error", err)
				r.mu.Lock()
				r.errs = append(r.errs, err)
				r.mu.Unlock()
			}
		})
	}
	return r
}

// Stop signals every loop to stop and waits for them. A loop that is handling
// an event finishes and checkpoints it first; later events stay unprocessed.
func (r *Runner) Stop() error {
	r.cancel()
	r.logger.Info("stopping event handlers")
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.errs...)
}

func run(ctx context.Context, source Source, h Handler, wake <-chan struct{}) error {
	name := h.Name()
	position, err := source.Checkpoint(name)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}
	for {
		events, err := source.ReadAfter(position, batchSize)
		if err != nil {
			return fmt.Errorf("read events after %d: %w", position, err)
		}
		if len(events) == 0 {
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		for _, e := range events {
			if ctx.Err() != nil {
				return nil
			}
			h.Handle(e)
			if err := source.SetCheckpoint(name, e.Position); err != nil {
				return fmt.Errorf("save checkpoint %d: %w", e.Position, err)
			}
			position = e.Position
		}
	}
}
