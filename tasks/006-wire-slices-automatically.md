---
title: Wire Slices Automatically
date: 2026-10-25
status: done
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

## Implementation Plan

### TLDR

Add a small registry to `internal/agent`, with `Build` turning registrations into `Slices`. Each slice registers in `init()`. A `go:generate` program in `internal/slices` writes the blank imports, and a test keeps that file in sync with the directory. `main` then shrinks to `Build` + `Run`. `Run` and its tests don't change.

### Steps

1. **Registry in `internal/agent`:** add `Deps`, `RegisterStateChange`, `RegisterAutomation` and `Build`, with the exact signatures in the ACs.
   - Keep the registry state in an unexported type. The exported funcs act on one package-level instance, so the internal tests can exercise a fresh instance without leaking global registrations into other tests.
   - `Build` calls the factories in registration order and returns the first error unwrapped, so main's log keeps `SENTRY_CLIENT_SECRET must be set`.
   - Test `Build` for the order kept within each kind, the first error stopping it, and an empty registry giving empty `Slices`.
2. **Slice registration:** each slice package gets an `init()` that registers a factory calling its existing `New`. The record slice passes `d.Getenv, d.Logger`. The handle slice wraps its result as `(automation, nil)`. `New` stays exported, so the existing tests are untouched.
3. **Generator and generated file in `internal/slices`:**
   - Add a `//go:generate go run gen.go` directive (on `doc.go`), and a `//go:build ignore` `gen.go` that lists the subdirectories of its working directory. It writes, through `go/format`, a file in package `slices` with the standard generated-code header and one sorted blank import `underling/internal/slices/<dir>` per directory.
   - Give the generator an output-path flag that defaults to the generated file's name. The sandbox can't write into the worktree from Bash, so verify by generating to `$TMPDIR` and diffing against the committed file, which you write with Write/Edit.
   - Update the `doc.go` package comment, which currently says the package holds only the test.
4. **Drift test in `internal/slices`:** compare the subdirectory names with the blank imports parsed from the generated file (the existing `imports` helper parses imports already), and report missing and extra entries. Also assert that the file has the generated-code header.
5. **`main`:** remove both slice imports and the `agent.Slices{…}` literal, and add `_ "underling/internal/slices"`. Call `agent.Build(agent.Deps{Getenv: os.Getenv, Logger: logger})`. On error, log "invalid configuration" and return 2, in the same place as today (after flag parsing, before listening). Check: `grep -n 'internal/slices/' cmd/underling/main.go` is empty.
6. **Docs:** update "Adding a slice" in `docs/event-model.md` to say: create the folder, write `New`, register in `init()` via `agent.RegisterStateChange` / `agent.RegisterAutomation`, then run `go generate ./...` (the drift test fails until you do). Remove the step of passing the slice to `agent.Run` from `main`.

### Notes

- Go 1.21+ initialises imported packages in import-path order, so the registration order (and the route and handler order) is deterministic.
- Check exit codes with a built binary (`go build -o $TMPDIR/underling ./cmd/underling`), not `go run`, which turns every failure into exit 1. Without the secret it exits 2 with "invalid configuration". `-h` exits 0.
- Sandbox: use Edit/Write for every file in the worktree, and check formatting with `gofmt -l`. Go needs `GOCACHE=$TMPDIR/gocache GOTMPDIR=$TMPDIR`. The `internal/agent` `Run` tests can't bind ports here (accepted), but the new `Build` tests don't listen, so they must pass.

## Implementation Notes

- **Names chosen:** the generated file is `internal/slices/slices_gen.go`, with header `// Code generated by gen.go; DO NOT EDIT.`. The generator's output flag is `-o` (default `slices_gen.go`). The package-level registry instance is `registered`, of the unexported type `registry`. The `Build` tests are internal (`package agent`) and use fresh `registry` values, so they never touch the global registrations.
- **Drift test in practice:** checked on a copy of the repo in `$TMPDIR`. With a new slice directory and a removed import, the test names both missing slices. With the header removed, it reports the missing header. After `go generate ./...`, the test passes. If a directory is deleted but its import stays, the compiler fails first ("package … is not in std") and names the stale package. So the test's "extra" message only shows when the stale path still resolves. That's fine, since both cases fail loudly.
- **Startup behaviour** (checked with a built binary): without `SENTRY_CLIENT_SECRET`, it logs `"invalid configuration"` with `"SENTRY_CLIENT_SECRET must be set"` and exits 2. `-h` exits 0. An unknown flag exits 2. With the secret set, it gets past `Build` and fails only at `listen` (sandbox, exit 1).
- **Generator check:** `go run gen.go -o $TMPDIR/…` produces output identical to the committed `slices_gen.go`. The committed file was written with Write because Bash can't write into the worktree.
- **Docs:** the "Adding a slice" list no longer mentions `main` passing `os.Getenv`. That job now belongs to `agent.Build`, described in the new registration step.
- **Review fixes:**
  - Round 1: `registration_test.go` fails, naming the folder, when a slice never registers. `gen.go` and the slice tests share one slice-folder rule (`isSliceDir`). `gen.go` keeps its own copy because it's `//go:build ignore`, and `TestGenUsesTheSameSliceDirRule` compares the two function bodies via the AST. The import guard uses the same rule. Checked in `$TMPDIR` copies: junk folders leave `slices_gen.go` unchanged (the old generator imported them and broke the build), and changing `gen.go`'s rule fails the equality test.
  - Round 2: the round-1 wiring test (which ran every factory with a dummy `Getenv` and compared an exact slice count) is gone. It would break any slice that validates its config, and any slice that registers in a loop. The per-folder registration check and the drift test cover a missing or unregistered slice, and each slice's own tests cover its route and handler. `registerCalls` now counts register calls in any func body, so registering through a helper called from `init()` passes; a dot import of `internal/agent` counts bare `RegisterX(...)` calls. `isSliceDir` and `registerCalls` use `go/build` (`build.Default.ImportDir`), so build constraints apply: a folder whose only file is `x_windows.go` (on a non-Windows host) or `//go:build ignore` is not a slice, and registrations in excluded files don't count. Test fixtures now need a real package clause, because `go/build` parses it. `gen.go` also requires `doc.go`'s package clause to be `slices` and stops with the same message otherwise. Checked in `$TMPDIR` copies: a helper registration passes; a folder with no registration fails with its name; a `winonly/x_windows.go` folder plus an `ignored/` folder leave `slices_gen.go` unchanged, and build, vet and tests stay green (the pre-round-2 generator imported both).
  - Deleting `wiring_test.go` needed `git rm` outside the sandbox, because the sandbox can't delete files in the worktree.
- **Known sandbox failures:** the 4 `internal/agent` `Run` tests fail at `net.Listen` with `bind: operation not permitted`, as accepted. Every other test passes under `-race`.
