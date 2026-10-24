// Command heinzel runs the event-sourced app.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"heinzel/internal/app"
	_ "heinzel/internal/slices" // registers every slice with the app
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := app.ParseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		logger.Error("invalid arguments", "error", err)
		return 2
	}
	slices, err := app.Build(app.Deps{Getenv: os.Getenv, Logger: logger})
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		logger.Error("listen", "addr", cfg.Addr, "error", err)
		return 1
	}
	if err := app.Run(ctx, cfg, ln, slices, logger); err != nil {
		logger.Error("app stopped with error", "error", err)
		return 1
	}
	return 0
}
