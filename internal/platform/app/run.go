package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"underling/internal/platform/eventhandler"
	"underling/internal/platform/eventstore"
)

const readHeaderTimeout = 10 * time.Second

// gin.SetMode writes a package global, so it is set once here rather than
// per router, where concurrent tests would race on it.
func init() { gin.SetMode(gin.ReleaseMode) }

// Config holds the command-line settings.
type Config struct {
	Addr            string
	DataDir         string
	ShutdownTimeout time.Duration
}

// ParseFlags parses the arguments (without the program name). Usage and
// errors go to output.
func ParseFlags(args []string, output io.Writer) (Config, error) {
	var cfg Config
	fs := flag.NewFlagSet("underling", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.Addr, "addr", ":8080", "HTTP listen address")
	fs.StringVar(&cfg.DataDir, "data-dir", "./data", "directory holding the event store")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 10*time.Second, "how long in-flight requests may take to finish on shutdown")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return cfg, nil
}

// Router returns the HTTP handler serving the state changes' routes on
// store, plus the platform's GET /healthz, which answers 200 "ok" without
// auth or touching the store. Unknown paths get 404 and known paths with
// another method 405.
func Router(store *eventstore.Store, stateChanges []StateChange) http.Handler {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.RedirectTrailingSlash = false
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	for _, sc := range stateChanges {
		r.Handle(sc.Method, sc.Path, sc.Handler(store))
	}
	return r
}

// Run serves the state changes on ln and runs the automations until ctx is
// cancelled. It then shuts down in order: drain HTTP requests (closing them
// after cfg.ShutdownTimeout), cancel the automations' ctx, stop the event
// handlers, close the store. Every step runs; the first error is returned.
func Run(ctx context.Context, cfg Config, ln net.Listener, slices Slices, logger *slog.Logger) error {
	store, err := eventstore.Open(cfg.DataDir)
	if err != nil {
		_ = ln.Close()
		return err
	}
	automationCtx, cancelAutomations := context.WithCancel(context.Background())
	defer cancelAutomations()
	handlers := make([]eventhandler.Handler, 0, len(slices.Automations))
	for _, a := range slices.Automations {
		handlers = append(handlers, a.Handler(automationCtx, store))
	}
	observer := eventhandler.Start(store, handlers, logger)

	srv := &http.Server{
		Handler:           Router(store, slices.StateChanges),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("app started", "addr", ln.Addr().String(), "dataDir", cfg.DataDir)

	var errs []error
	select {
	case <-ctx.Done():
	case err := <-served:
		errs = append(errs, fmt.Errorf("serve http: %w", err))
	}
	logger.Info("app shutting down")

	errs = append(errs, drain(srv, cfg.ShutdownTimeout))
	cancelAutomations()
	if err := observer.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("stop event handlers: %w", err))
	}
	if err := store.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close store: %w", err))
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func drain(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}
