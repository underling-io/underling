---
title: Wire Slices Automatically
date: 2026-10-25
status: ready-for-dev
type: tech
---

## Wire Slices Automatically

`main` stops listing slices. Every package under `internal/slices/` is wired in automatically: it registers itself, and a generated import file pulls in every slice directory. Adding a slice means adding a folder and running `go generate`, with no edit to `main`.

### Acceptance Criteria

- `internal/agent` offers a registry:
  - `RegisterStateChange(func(Deps) (StateChange, error))` and `RegisterAutomation(func(Deps) (Automation, error))`.
  - `Deps` holds `Getenv func(string) string` and `Logger *slog.Logger`.
  - `Build(Deps) (Slices, error)` calls every registered factory and returns the first error. `Run` still takes a `Slices` value.
- Each existing slice registers itself in an `init()` that calls its `New`.
- `go generate ./...` writes a generated file in the parent `internal/slices` package. The file has one blank import per slice directory (sorted) and the standard `// Code generated … DO NOT EDIT.` header. A test in `internal/slices` fails if the directories and the imports differ, naming what's missing or extra.
- `main` imports no individual slice package. It blank-imports `internal/slices`, calls `agent.Build` with `os.Getenv` and its logger, and passes the result to `agent.Run`.
- Startup behaviour doesn't change. A slice config error, such as a missing `SENTRY_CLIENT_SECRET`, still logs "invalid configuration" and exits 2, `-h` still exits 0, and the same routes and handler are served.
- `docs/event-model.md` "Adding a slice" describes the new steps: create the folder, register in `init()`, run `go generate`. Wiring the slice in `main` is no longer a step.

### Notes

- The user chose self-registration in `init()` with generated blank imports, over generated constructor calls (no registry) and a hand-kept import list.
- Go can't discover packages at runtime, so the generated import file is what makes the directory the source of truth. The drift test is what stops a forgotten `go generate`.
- Putting the generated file in the existing parent package `internal/slices` (not a new subdirectory) keeps the import guard as it is, because the guard checks only subdirectories. The generator should be a `//go:build ignore` program next to it, run by a `//go:generate` directive.
