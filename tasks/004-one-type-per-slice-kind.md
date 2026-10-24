---
title: One Type per Slice Kind
date: 2026-10-25
status: done
type: tech
---

## One Type per Slice Kind

Replace the catch-all `agent.Slice` func type with one type per event-modeling slice kind, so each slice gets only what its kind needs. Today the automation slice is handed a mux and a store it ignores, and the state-change slice returns a handler list that's always empty.

### Acceptance Criteria

- `agent.Slice` is gone. `internal/agent` has instead:
  - `StateChange`: a struct with `Pattern string` and `Handler func(store *eventstore.Store) http.Handler`.
  - `Automation`: an alias for `eventhandler.Handler`.
  - `Slices`: a struct holding `StateChanges []StateChange` and `Automations []Automation`.
- `agent.Run` takes a `Slices` value instead of `[]Slice`. It registers each state change's route on its mux and runs every automation.
- `recordsentryissuealert.New(secret, logger)` returns an `agent.StateChange` for `POST /sentry/callback`. `handlesentryissuealert.New(logger)` returns an `agent.Automation`. `main` wires them through `agent.Slices`.
- Pure refactor: existing tests keep their assertions (except the one dropped per Notes) and pass. The callback tests still go through a mux with the returned pattern, so `POST /sentry/callback` stays tested.
- `docs/event-model.md` "Adding a slice" describes the two kinds and their constructor shapes. `grep -rnE 'agent\.Slice\b|type Slice ' cmd internal docs` returns nothing. Historical task files are left alone.

### Notes

- The user picked this shape over narrowing the dependencies behind interfaces or making Slice an interface.
- `TestSliceRunsNoEventHandlers` (recordsentryissuealert) becomes redundant now that the type enforces it. Drop it, along with doc and comment lines saying a slice "runs no event handlers" or "registers no routes".

## Implementation Plan

### TLDR

Swap the func type for the three types in the ACs, have `Run` build the mux and handler list from `Slices`, and adjust both slice constructors, `main`, the tests and the docs. The refactor touches only types and wiring, with no logic changes.

### Steps

1. **`internal/agent`:** replace `Slice` with `StateChange`, `Automation` (a true alias, `= eventhandler.Handler`, so `eventhandler.Start` takes `[]Automation` without conversion) and `Slices`, exactly as the ACs say. `Run(ctx, cfg, ln, slices Slices, logger)` calls each `StateChange.Handler(store)` once at startup and does `mux.Handle(Pattern, …)`. Everything else in `Run` (open-store failure closing `ln`, shutdown order, error collection) stays the same.
2. **`agent_test.go`:** rewrite the test helpers. `appender` and `blocking` become `StateChange` values, and `withHandlers` goes away in favour of putting handlers in `Slices.Automations`. `start` takes `agent.Slices`. Test bodies and assertions stay the same.
3. **`recordsentryissuealert`:** `New(secret, logger) agent.StateChange`, with pattern `POST /sentry/callback` and a handler func that closes over secret and logger and builds the callback with the store. `newMux` in the tests does `mux.Handle(sc.Pattern, sc.Handler(store))` using the returned value, so the pattern stays covered. Delete `TestSliceRunsNoEventHandlers`.
4. **`handlesentryissuealert`:** `New(logger) agent.Automation`, which returns the alert logger. Its test calls `New(...)` directly instead of `New(...)(nil, nil)[0]`.
5. **`main`:** pass `agent.Slices{StateChanges: …, Automations: …}`.
6. **`docs/event-model.md`:** in the intro and "Adding a slice", describe the two kinds and their constructors (`New(...) agent.StateChange` / `New(...) agent.Automation`). Drop the sentences "It runs no event handlers." and "The slice registers no routes." Update the package doc comments and `New` comments to match.

### Notes

- The import guard already allows `internal/agent`, so it needs no change. `handlesentryissuealert` no longer imports `net/http` or `eventstore`.
- Bash can't write inside the worktree in this sandbox: use Edit/Write for file changes, and no `gofmt -w`. Check formatting with `gofmt -l`. `git commit` works.
- Verification in this run is `go vet ./... && go test -race ./...` with `GOCACHE=$TMPDIR/gocache`. The sandbox blocks local binding, so the `internal/agent` tests fail at their `net.Listen` call. They must compile (`go vet` passes) but can't run here. The user should run `make check` before shipping.

## Implementation Notes

- Implemented as planned, with no deviations. The code went in one commit because the type change breaks every caller at once. TDD red was the compile failure after the tests moved to the new API. Docs went in a separate commit.
- Verification: `gofmt -l .` is clean and `go vet ./...` is clean. `go test -race ./...` passes everywhere except the four `internal/agent` Run tests, which fail only at `net.Listen` ("bind: operation not permitted", sandbox). The `internal/agent` config tests pass. **Run `make check` outside the sandbox before shipping** so the agent lifecycle tests actually run against the new `Slices` wiring.
- `handlesentryissuealert.New`'s doc comment now says only that it logs each alert. The package doc already names the handler, so the comment doesn't repeat it.
- The `alertLogger` test helper in `logger_test.go` keeps its `t *testing.T` parameter (now just `t.Helper()`), so the test bodies stay unchanged.
- `logger.go` comments still say "implements eventhandler.Handler". That is still accurate, since `agent.Automation` is an alias, so they were left alone.
