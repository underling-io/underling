// Package eventhandler runs checkpointed event handlers over the event
// store: one goroutine per handler, in global order, resuming after the
// handler's checkpoint.
package eventhandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"underling/internal/platform/eventstore"
)

const batchSize = 100

// Handler reacts to stored events. Its name is its checkpoint key, so it
// must never change. Handle cannot fail: it deals with its own errors.
type Handler interface {
	Name() string
	Handle(e eventstore.Event)
}

// Source is what the observer needs from the event store.
type Source interface {
	ReadAfter(position uint64, limit int) ([]eventstore.Event, error)
	Checkpoint(name string) (uint64, error)
	SetCheckpoint(name string, position uint64) error
	Subscribe() <-chan struct{}
}

// Observer runs a loop per handler until stopped.
type Observer struct {
	logger *slog.Logger
	stop   context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	errs []error
}

// Start starts a loop for each handler: it catches up from the handler's
// checkpoint, then follows new events.
func Start(source Source, handlers []Handler, logger *slog.Logger) *Observer {
	ctx, stop := context.WithCancel(context.Background())
	observer := &Observer{logger: logger, stop: stop}
	for _, h := range handlers {
		wake := source.Subscribe()
		observer.wg.Go(func() {
			if err := loop(ctx, source, h, wake); err != nil {
				logger.Error("event handler stopped", "handler", h.Name(), "error", err)
				observer.mu.Lock()
				observer.errs = append(observer.errs, err)
				observer.mu.Unlock()
			}
		})
	}
	return observer
}

// Stop stops every loop and waits for them. A handler in the middle of an
// event finishes and checkpoints it; later events stay for the next start.
func (o *Observer) Stop() error {
	o.stop()
	o.logger.Info("stopping event handlers")
	o.wg.Wait()
	o.mu.Lock()
	defer o.mu.Unlock()
	return errors.Join(o.errs...)
}

func loop(ctx context.Context, source Source, h Handler, wake <-chan struct{}) error {
	name := h.Name()
	position, err := source.Checkpoint(name)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}
	for {
		batch, err := source.ReadAfter(position, batchSize)
		if err != nil {
			return fmt.Errorf("read events after %d: %w", position, err)
		}
		if len(batch) == 0 {
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		for _, e := range batch {
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
