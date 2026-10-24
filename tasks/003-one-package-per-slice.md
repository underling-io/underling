---
title: One Package per Slice
date: 2026-10-25
status: ready-for-dev
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
- `cmd/heinzel/main.go` wires both slices separately into `agent.Run`. Startup behaviour (exit 2 without the secret, `-h` → 0) is unchanged.
- `docs/event-model.md` describes slices, one package each, with "adding a slice" guidance. It documents the two Sentry slices as independent slices linked by the event, and the automation's reaction as a TBD placeholder.
- `make check` passes. Existing behaviour tests move with their code and keep passing.

### Notes

- Slice package names follow the command / automation naming used in event modeling (`record…`, `handle…`). The user only gave the shapes "webhook + command handler" and "handler of SentryIssueAlertReceived". The names are my choice and easy to change.
- Pure refactor: no change to the event store, the runner, the persisted bbolt layout, the event payload or the checkpoint name.
