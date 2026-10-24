---
title: One Type per Slice Kind
date: 2026-10-25
status: ready-for-dev
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
