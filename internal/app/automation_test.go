package app_test

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"heinzel/internal/app"
	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
)

// automation wraps a handler that needs neither ctx nor the store.
func automation(h eventhandler.Handler) app.Automation {
	return app.Automation{Handler: func(context.Context, *eventstore.Store) eventhandler.Handler { return h }}
}

// pipeListener is an in-memory net.Listener that never accepts a
// connection, so Run can be exercised without binding a socket.
type pipeListener struct {
	done      chan struct{}
	closeOnce sync.Once
}

func newPipeListener() *pipeListener { return &pipeListener{done: make(chan struct{})} }

func (l *pipeListener) Accept() (net.Conn, error) {
	<-l.done
	return nil, net.ErrClosed
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// ctxBlocker blocks in Handle until its ctx is cancelled, then reports it.
type ctxBlocker struct {
	ctx     context.Context
	entered chan struct{}
	sawDone chan bool
}

func (*ctxBlocker) Name() string { return "ctx-blocker" }
func (b *ctxBlocker) Handle(eventstore.Event) {
	close(b.entered)
	<-b.ctx.Done()
	b.sawDone <- true
}

func TestAutomationCtxIsCancelledOnShutdownBeforeHandlersStop(t *testing.T) {
	built := make(chan *eventstore.Store, 1)
	blocker := &ctxBlocker{entered: make(chan struct{}), sawDone: make(chan bool, 1)}
	a := app.Automation{Handler: func(ctx context.Context, store *eventstore.Store) eventhandler.Handler {
		blocker.ctx = ctx
		built <- store
		return blocker
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, app.Config{DataDir: t.TempDir(), ShutdownTimeout: time.Second}, newPipeListener(), app.Slices{Automations: []app.Automation{a}}, discard())
	}()

	var store *eventstore.Store
	select {
	case store = <-built:
	case <-time.After(waitLimit):
		t.Fatal("automation was not built")
	}
	if _, _, err := store.Append("s", 0, []eventstore.NewEvent{{Type: "TestEvent", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	select {
	case <-blocker.entered:
	case <-time.After(waitLimit):
		t.Fatal("handler never saw the event")
	}
	if blocker.ctx.Err() != nil {
		t.Fatal("automation ctx cancelled before shutdown")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("run did not return: the automation ctx was not cancelled before stopping the handlers")
	}
	if !<-blocker.sawDone {
		t.Fatal("handler did not observe cancellation")
	}
}
