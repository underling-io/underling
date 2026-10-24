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
	"heinzel/internal/command"
	"heinzel/internal/eventhandler"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		logger.Error("listen", "addr", cfg.Addr, "error", err)
		return 1
	}
	handlers := []eventhandler.Handler{eventhandler.NewEventLogger(logger)}
	if err := agent.Run(ctx, cfg, ln, command.DefaultRegistry(), handlers, logger); err != nil {
		logger.Error("agent stopped with error", "error", err)
		return 1
	}
	return 0
}
