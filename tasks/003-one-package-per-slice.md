---
title: One Package per Slice
date: 2026-10-25
status: done
type: refactor
---

## One Package per Slice

Task 002 put both Sentry slices into one feature package, `internal/features/sentry/`. That doesn't match Martin Dilger's event-modeling slices. In his model each slice is an independent unit: a state-change slice (trigger → command → event) and an automation slice (event → reaction) are separate, and they're coupled only through the event. So drop the "feature" grouping. Every slice gets its own package, and "feature" is renamed to "slice" throughout.

### Acceptance Criteria

- `internal/features/` is renamed to `internal/slices/`. Each slice lives in exactly one package, `internal/slices/<slice>/`. No package groups several slices.
- `agent.Feature` is renamed to `agent.Slice` with the same signature. `agent.Run` takes `[]agent.Slice`. No `feature`/`Feature` identifiers, comments or docs referring to the old concept remain in Go code or `docs/`. Historical task files are left alone.
- **Slice 1 (state change), `internal/slices/recordsentryissuealert/`:** `POST /sentry/callback` → command "Record Sentry issue alert" → `SentryIssueAlertReceived`. The package also owns `SENTRY_CLIENT_SECRET` loading. Its behaviour (status codes, signature check, size cap, validation, dedupe, persisted payload and stream ids) is unchanged. It registers no event handlers.
- **Slice 2 (automation), `internal/slices/handlesentryissuealert/`:** handles `SentryIssueAlertReceived`. It registers no routes. For now its reaction is still the single structured log line from task 002, a placeholder until the real reaction is defined. The handler name `sentry-issue-alert-logger` is kept unchanged because it's the persisted checkpoint key.
- The two slices don't import each other. They share only the `internal/events` contract. The import-guard test (moved to `internal/slices/`) still enforces the allowlist and forbids importing any other slice. It fails if no slice packages exist.
- `cmd/underling/main.go` wires both slices separately into `agent.Run`. Startup behaviour (exit 2 without the secret, `-h` → 0) is unchanged.
- `docs/event-model.md` describes slices, one package each, with "adding a slice" guidance. It documents the two Sentry slices as independent slices linked by the event, and the automation's reaction as a TBD placeholder.
- `make check` passes. Existing behaviour tests move with their code and keep passing.

### Notes

- Slice package names follow the command / automation naming used in event modeling (`record…`, `handle…`). The user only gave the shapes "webhook + command handler" and "handler of SentryIssueAlertReceived". The names are my choice and easy to change.
- Pure refactor: no change to the event store, the runner, the persisted bbolt layout, the event payload or the checkpoint name.

## Implementation Plan

### TLDR

This is a mechanical rename plus a split. `internal/features/` becomes `internal/slices/` and `agent.Feature` becomes `agent.Slice`. The single `sentry` package splits into two packages. `recordsentryissuealert` takes the callback, the config and `SecretFromEnv`. `handlesentryissuealert` takes the logger handler. Each package has its own constructor that returns an `agent.Slice`, and `main` wires both. Logic, payloads, stream ids, response bodies and the handler name stay byte-for-byte the same. Use `git mv` so history follows the files. (Synthesised from a Claude and Codex panel. They agreed on every structural decision; the Claude plan supplied the API locks and checks.)

### Steps

1. **Rename the seam in `internal/agent`.** Change `type Feature` to `type Slice` and keep the signature `func(mux *http.ServeMux, store *eventstore.Store) []eventhandler.Handler`. `Run(ctx, cfg, ln, slices []Slice, logger)` is otherwise unchanged. Reword the comments so a `Slice` is one slice: it registers zero or more routes and returns zero or more handlers. Rename the test helpers and comments in `agent_test.go` the same way.
2. **Create `internal/slices/recordsentryissuealert/` (state change).** Move in `callback.go`, `config.go` and their tests. The locked API:
   - `New(secret string, logger *slog.Logger) agent.Slice` registers only `POST /sentry/callback` and returns no handlers.
   - `SecretFromEnv` and its env var constant are unchanged.

   The package doc comment names the slice: trigger → "Record Sentry issue alert" → `SentryIssueAlertReceived`. Add an assertion that `New(...)` returns zero handlers.
3. **Create `internal/slices/handlesentryissuealert/` (automation).** Move in `logger.go` and its test. The locked API is `New(logger *slog.Logger) agent.Slice`. It registers no routes and returns exactly one handler, named `"sentry-issue-alert-logger"`. Log keys and messages are unchanged. The doc comment says the logging reaction is a placeholder until the real reaction is defined.
   - The moved test currently depends on things from `callback_test.go`, such as the `secret` const. Drop those dependencies.
   - Have the test obtain its handler through `New(logger)(nil, nil)`. A nil mux panics if the slice ever registers a route, so this proves "registers no routes".
4. **Delete `internal/features/sentry/sentry.go`.** Its combining constructor is the grouping this task removes. Neither slice imports the other. They share only `internal/events`.
5. **Move the import guard to `internal/slices/`.** Move `doc.go` and `imports_test.go`. Rename the package, identifiers, messages and test name from feature to slice, and point the path constant at `underling/internal/slices`. The allowlist and the per-directory "own package only" rule already forbid importing a sibling slice, and the zero-packages failure stays. Check by hand: add an import of `recordsentryissuealert` to a `handlesentryissuealert` file, confirm the guard fails, then revert. `TestEventsPackageHoldsOnlyContracts`'s `../events` path still resolves from the new location.
6. **Wire `cmd/underling/main.go`.** Load the secret with `recordsentryissuealert.SecretFromEnv`, in the same place and with the same exit 2. Then pass `[]agent.Slice{recordsentryissuealert.New(secret, logger), handlesentryissuealert.New(logger)}` to `agent.Run`. `-h` still exits 0 before the secret is read.
7. **Update the `internal/events` doc comment** to say "shared between slices".
8. **Rewrite `docs/event-model.md`.**
   - The intro says each slice is one package under `internal/slices/<slice>/`, and slices are coupled only through events.
   - Give each Sentry slice its own section with its package path. Keep the existing check list and payload text under the record slice.
   - The automation section names `sentry-issue-alert-logger` and describes the log line as the current placeholder; the real reaction is TBD.
   - "Adding a feature" becomes "Adding a slice". Cover one package per slice, the `agent.Slice` constructor, the allowlist (including "never imports another slice"), the guard path `internal/slices/imports_test.go`, and that shared contracts go in `internal/events`.
9. **Final sweep.** `grep -rni feature --include='*.go' .` and `grep -rni feature docs/` must return nothing. `tasks/` is excluded.

### Notes

- Make no edits to the runner, the store, the `internal/events` contracts, the stream-id format, the response JSON or the handler name. Any diff in those beyond comments is a bug.
- Go commands and file writes in this worktree need to run outside the sandbox, because of the Go build cache and a path outside the primary repo.

## Implementation Notes

- Followed the plan with no structural deviations. Every move used `git mv`, so `logger*.go`, `callback*.go`, `config*.go` and `imports_test.go` show as renames. `doc.go` was rewritten entirely, so git records it as a delete plus an add.
- Each slice's constructor and package doc live in a new file named after the package (`recordsentryissuealert.go`, `handlesentryissuealert.go`). The plan didn't say where `New` should go, and the deleted `sentry.go` played the same role.
- Inside the import guard, the slice counter is called `sliceCount`, not `slices`. The test file imports the stdlib `slices` package, and a variable with that name would shadow it. `agent.Run`'s parameter is named `slices` because `agent.go` doesn't import that package.
- The guard's error message now says "their own package" (singular) to match the one-package-per-slice rule. Its logic is unchanged. Subpackages of a slice are still allowed.
- New test `TestSliceRunsNoEventHandlers` in `recordsentryissuealert` asserts that the slice returns zero handlers. The handler test gets its handler through `New(logger)(nil, nil)`, so a route registration would panic on the nil mux.
- I changed one comment: the callback comment now names the command "Record Sentry issue alert" instead of "Record issue alert". No code paths, response bodies, stream ids, payloads or the handler name changed.
- Hand-check from step 5: I added `_ "underling/internal/slices/recordsentryissuealert"` to `handlesentryissuealert.go`. `TestSlicesImportOnlyStdlibSharedEventsAndInfrastructure` failed with the expected message. I reverted the import before committing.
- Checked startup by hand with a built binary: without `SENTRY_CLIENT_SECRET` it exits 2, and `-h` exits 0.
- Final sweep: `grep -rni feature --include='*.go' .` and `grep -rni feature docs/` both return nothing.
- **Follow-ups from review.**
  - `cmd/underling` has no tests, so nothing automatically checks that both slices are wired. This gap predates the task.
  - Once the real automation reaction is defined, rename `alertLogger` and `logger.go`. The handler name `sentry-issue-alert-logger` must stay, because it's the checkpoint key.
  - The import guard deliberately allows a slice to have subpackages for its own internals.
