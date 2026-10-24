package app_test

import (
	"io"
	"testing"
	"time"

	"heinzel/internal/app"
)

func TestParseFlagsDefaults(t *testing.T) {
	got, err := app.ParseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := app.Config{Addr: ":8080", DataDir: "./data", ShutdownTimeout: 10 * time.Second}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseFlagsOverrides(t *testing.T) {
	got, err := app.ParseFlags([]string{"-addr", "127.0.0.1:9000", "-data-dir", "/var/lib/heinzel", "-shutdown-timeout", "3s"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := app.Config{Addr: "127.0.0.1:9000", DataDir: "/var/lib/heinzel", ShutdownTimeout: 3 * time.Second}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseFlagsRejectsUnknownFlags(t *testing.T) {
	if _, err := app.ParseFlags([]string{"-nope"}, io.Discard); err == nil {
		t.Fatal("parse succeeded, want error")
	}
}

func TestParseFlagsRejectsPositionalArguments(t *testing.T) {
	if _, err := app.ParseFlags([]string{"serve"}, io.Discard); err == nil {
		t.Fatal("parse succeeded, want error")
	}
}
