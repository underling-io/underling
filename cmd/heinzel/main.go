// Command heinzel runs the event-sourced agent.
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

	"heinzel/internal/agent"
	"heinzel/internal/features/sentry"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := agent.ParseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		logger.Error("invalid arguments", "error", err)
		return 2
	}
	sentrySecret, err := sentry.SecretFromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 2
	}
	features := []agent.Feature{sentry.New(sentrySecret, logger)}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		logger.Error("listen", "addr", cfg.Addr, "error", err)
		return 1
	}
	if err := agent.Run(ctx, cfg, ln, features, logger); err != nil {
		logger.Error("agent stopped with error", "error", err)
		return 1
	}
	return 0
}
