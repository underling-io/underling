package app

import (
	"flag"
	"fmt"
	"io"
	"time"
)

// Config holds the app's command-line settings.
type Config struct {
	Addr            string
	DataDir         string
	ShutdownTimeout time.Duration
}

// ParseFlags parses command-line arguments (without the program name) into a
// Config. Usage and parse errors are written to output.
func ParseFlags(args []string, output io.Writer) (Config, error) {
	var cfg Config
	fs := flag.NewFlagSet("heinzel", flag.ContinueOnError)
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
