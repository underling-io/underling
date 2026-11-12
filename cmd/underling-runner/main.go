// Command underling-runner runs one investigation's Claude step in a
// process of its own, as its environment describes it
// (docs/event-model.md § Runner).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"underling/internal/slices/investigatesentryissue/runner"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runner.Run(ctx, runner.Params{
		Getenv:     os.Getenv,
		Logger:     slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		HTTPClient: &http.Client{},
	})
}
