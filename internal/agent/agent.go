// Package agent wires the event store, the command side and the event
// handlers together and owns their lifecycle.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"heinzel/internal/command"
	"heinzel/internal/eventhandler"
	"heinzel/internal/eventstore"
	"heinzel/internal/httpapi"
)

const readHeaderTimeout = 10 * time.Second

// Run serves commands on ln until ctx is cancelled, then shuts down in order:
// it drains HTTP requests (forcing them closed after cfg.ShutdownTimeout),
// stops the event handlers, and closes the store. Every step runs even if an
// earlier one failed; the first error is returned.
func Run(ctx context.Context, cfg Config, ln net.Listener, registry *command.Registry, handlers []eventhandler.Handler, logger *slog.Logger) error {
	store, err := eventstore.Open(cfg.DataDir)
	if err != nil {
		_ = ln.Close()
		return err
	}
	runner := eventhandler.Start(store, handlers, logger)
	srv := &http.Server{
		Handler:           httpapi.NewHandler(registry, store, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("agent started", "addr", ln.Addr().String(), "dataDir", cfg.DataDir)

	var errs []error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		errs = append(errs, fmt.Errorf("serve http: %w", err))
	}
	logger.Info("agent shutting down")

	errs = append(errs, shutdownHTTP(srv, cfg.ShutdownTimeout))
	if err := runner.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("stop event handlers: %w", err))
	}
	if err := store.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close store: %w", err))
	}
	return firstError(errs)
}

func shutdownHTTP(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
