---
title: Vertical Slices and Sentry Callback
date: 2026-10-24
status: done
type: feat
---

## Vertical Slices and Sentry Callback

Restructure the agent into vertical feature slices, event-modeling style, and add the first real slice: a Sentry issue-alert callback.

### Acceptance Criteria

- Each feature lives in exactly one package under `internal/features/<name>/` that holds everything for it: HTTP route, command handling and its event handler(s). Sentry is one package, `internal/features/sentry/`, holding both its slices.
- A feature package's non-stdlib imports are limited to an explicit allowlist: the shared events package plus generic infrastructure (event store, handler runner, and whatever HTTP/lifecycle package remains). Never another feature package or a shared domain/helper package. An automated test enforces this.
- The shared events package contains only event contracts, no behaviour.
- `POST /sentry/callback` accepts Sentry internal-integration webhooks. `Sentry-Hook-Signature` must be the hex HMAC-SHA256 of the raw body keyed with the client secret, verified before decoding. Missing or wrong signature → 401, nothing stored. Body over 10 MiB → 413, nothing stored.
- A validly signed webhook with `Sentry-Hook-Resource: event_alert` stores one SentryIssueAlertReceived event with at least `data.event.event_id`, `data.event.issue_id` (string), `data.event.project` (number), `data.event.title`, `data.event.level`, `data.event.web_url` and `data.triggered_rule`. Returns 2xx.
- Redelivery of the same alert (same Sentry event id and rule name) stores nothing new and returns 2xx. The same event triggering a different rule is a new alert and is stored.
- Validly signed webhooks with any other, missing or unknown resource return 2xx and store nothing. A validly signed `event_alert` with malformed JSON or a missing/empty event id, issue id or triggered rule returns 400 and stores nothing.
- The client secret comes from env var `SENTRY_CLIENT_SECRET`. If it is missing or empty the agent logs an error and exits with code 2 before listening.
- The Sentry slice's event handler writes one structured log line per SentryIssueAlertReceived event (handler name, position, issue id, title, level) and ignores other event types.
- The RegisterNote command, the generic event-logger, the generic `POST /commands` endpoint and the `internal/command` package are removed. Infrastructure tests use test-only commands/handlers instead.
- `docs/event-model.md` describes each slice as trigger → command → event → reaction (for Sentry: a state-change slice and an automation slice).

### Notes

- Don't change the event store, runner or persisted bbolt layout beyond what removing the placeholders requires.

## Implementation Plan

### TLDR

Replace the generic command pipeline (`internal/command`, `internal/httpapi`, the event-logger) with a small feature seam in `internal/agent`. Add a contracts-only `internal/events` package and one feature package, `internal/features/sentry/`, which does three things:
- verifies the webhook;
- appends one event per (Sentry event id, rule), using the store's expected-version-0 conflict for idempotency;
- logs each alert from a checkpointed handler.

A go/parser test enforces the import allowlist. (Synthesised from a Claude and Codex panel. They agreed on every structural decision; the Claude plan supplied the detail.)

### Steps

1. **Shared contract `internal/events`.** Add a type-name constant `"SentryIssueAlertReceived"` and a payload struct. The persisted JSON is locked: `{"eventId": string, "issueId": string, "project": number (int64), "title": string, "level": string, "webUrl": string, "triggeredRule": string}`. Stdlib-only imports, no funcs or methods.
2. **Feature seam. This cross-package contract is locked.**
   - `type Feature func(mux *http.ServeMux, store *eventstore.Store) []eventhandler.Handler`
   - `agent.Run(ctx, cfg, ln, features []agent.Feature, logger)`

   `Run` opens the store, calls each feature to register its routes on one mux and collect its handlers, starts the runner, then serves. Keep the shutdown order and error behaviour exactly as they are. Delete `internal/httpapi` and `internal/command` entirely.
3. **Agent tests on test-only features.** Use a test feature with an appending route and a blocking route. Port all of these:
   - restart/resume, using a recording handler instead of the event-logger;
   - drain → stop handlers → close store order;
   - shutdown timeout;
   - store-open failure.

   The reordering mutants from task 001's notes must still be killed. Carry the 400-style decoding concerns into the Sentry tests (step 6), not into `agent`.
4. **Remove the event-logger** (its file and test). Leave the runner and the store untouched. The `"NoteRegistered"` literals in the runner and store tests are inert fixture data and can stay.
5. **Sentry config in the feature.** The sentry package exposes a constructor that takes the secret, plus a helper that reads `SENTRY_CLIENT_SECRET` through an injected getenv and errors on missing or empty. `main` calls it after `ParseFlags` and before `net.Listen`; on error it logs and returns 2. `agent` keeps no Sentry knowledge. `-h` still exits 0 without the secret.
6. **`POST /sentry/callback`, the state-change slice.** In this order:
   1. **Size:** read the raw body through `http.MaxBytesReader` (10 MiB). A `*http.MaxBytesError` → 413.
   2. **Signature:** verify `Sentry-Hook-Signature` as hex HMAC-SHA256 of the raw bytes. Hex-decode the header, then `hmac.Equal`. Missing, non-hex or wrong → 401.
   3. **Resource:** a `Sentry-Hook-Resource` other than exactly `event_alert` → 200, nothing stored.
   4. **Decode:** decode `data.event.{event_id, issue_id, project, title, level, web_url}` and `data.triggered_rule`. Ignore unknown fields. A JSON or type error, or an empty or whitespace-only `event_id`, `issue_id` or `triggered_rule` → 400. Trim only to validate; store values as sent.
   5. **Append:** append to stream `"sentry-issue-alert-" + hex(sha256(eventID + "\x00" + triggeredRule))` with expected version 0. `eventstore.ErrConflict` → 200, a duplicate. Any other error → 500 with a generic body, and log the cause.

   Success returns 200 with a small JSON body. Tests use a fixture shaped like Sentry's documented `event_alert` payload, with top-level `action`, `installation` and `actor` and many extra `event` fields. They must cover:
   - same event and rule twice → 1 stored;
   - same event with a different rule → 2 stored;
   - over 10 MiB with a valid signature → 413;
   - valid signature with no resource header → 200 and nothing stored.
7. **Automation slice: the handler `sentry-issue-alert-logger`.** Its name is the checkpoint key, so freeze it. For each `SentryIssueAlertReceived` it logs one line with `handler`, `position`, `issueId`, `title` and `level`. It ignores other types. A payload it can't decode is logged as an error and skipped, because `Handle` can't fail. The feature's register function returns this handler, and `main` passes the feature to `agent.Run`.
8. **Import-guard test in `internal/features`.** Add a `doc.go` with only the package clause, plus the test. The test parses every `.go` file, tests included, in each subdirectory with `go/parser` `ImportsOnly`.
   - Allowed: stdlib plus exactly `heinzel/internal/{events,eventstore,eventhandler,agent}`.
   - The module path `heinzel` has no dot, so treat a `heinzel/` prefix as non-stdlib before applying the "first element has no dot" stdlib rule. External modules (bbolt) fail.
   - Fail if there are zero feature packages.
   - Also assert that `internal/events` is stdlib-only and has no `FuncDecl`.
   - Check: importing another feature or a new shared helper from sentry makes the test fail.
9. **Write `docs/event-model.md`.** Cover:
   - the Sentry state-change slice: `POST /sentry/callback` → "Record issue alert" (signature, validation, dedupe) → `SentryIssueAlertReceived`;
   - the automation slice: `SentryIssueAlertReceived` → `sentry-issue-alert-logger` logs;
   - a short "adding a feature" section: package location, `agent.Feature`, the allowlist, where shared events go.

### Notes

- Existing data dirs keep an orphan `event-logger` checkpoint and old `NoteRegistered` events. This is harmless: the new handler starts at 0 and skips non-Sentry events. Don't migrate.
- `Sentry-Hook-Timestamp` is deliberately not checked, because dedupe makes replays harmless.
- The stream id is hashed because event ids and rule names are free text, so a plain join could collide.
- Two features registering the same route pattern make `ServeMux` panic at startup. That's acceptable.
- Bash in this worktree needs to run outside the sandbox (Go build cache plus the path outside the primary repo).

## Implementation Notes

- **Layout.** `internal/agent` now holds `Feature`, `Run` and `ParseFlags`. `internal/events` holds only the `SentryIssueAlertReceived` contract. `internal/features/sentry/` holds `sentry.go` (`New(secret, logger) agent.Feature`), `callback.go` (the state-change slice), `logger.go` (the automation slice) and `config.go` (`SecretFromEnv`). `internal/features/doc.go` and `imports_test.go` hold the import guard. `internal/command`, `internal/httpapi` and the event-logger are gone.
- **`sentry.New` also takes a logger.** The locked `agent.Feature` signature has no logger, so the constructor takes one alongside the secret. The callback uses it for 500 causes and the handler logs through it.
- **The alert level is logged as `sentryLevel`, not `level` (deviation).** slog's JSON handler already writes the record's own `level` key, so a `level` attribute would produce a line with two `level` keys, and most JSON readers keep only the last one. The persisted event field is still `level`; only the log key differs.
- **Response bodies.** Success is `{"status":"recorded"}`, a redelivery is `{"status":"duplicate"}` and an ignored resource is `{"status":"ignored"}`, all 200. Errors are `{"error":"..."}` with generic messages; JSON decoder details are not echoed. A body that fails to read for a reason other than size returns 400.
- **Store failure is tested with a closed store.** `Append` on a closed bbolt store fails, so the 500 test needs no fake store and the callback keeps the concrete `*eventstore.Store` the `Feature` signature hands it.
- **Agent tests.** Three test features stand in for the old pipeline: `appender` (`POST /append/{stream}`), `blocking()` (`POST /block`) and `withHandlers(...)`. The restart test splits the route and the recording handler across two features, which also checks that `Run` combines the handlers from all features. The two reordering mutants (stopping handlers before draining HTTP, closing the store before stopping handlers) were re-run by hand and both still fail `TestShutdownDrainsRequestsThenStopsHandlersThenClosesStore`.
- **Import guard checked by hand.** Adding an import of `heinzel/internal/features/other`, `heinzel/internal/helper` or `go.etcd.io/bbolt` to a sentry file fails the guard. So do a func or a non-stdlib import in `internal/events`. A stdlib import passes. A feature may import its own package path, which its external `_test` package needs. The guard only parses imports, so it also flags imports of packages that don't exist yet.
- **`SENTRY_CLIENT_SECRET`.** A whitespace-only value counts as set; only missing or empty is rejected. Checked by hand on a built binary: missing → exit 2, empty → exit 2, `-h` without the secret → exit 0.
- **No end-to-end smoke test.** Running the agent and curling `POST /sentry/callback` was denied in this session's environment. The callback is covered through the real mux and a real bbolt store in `callback_test.go`, and `main` wiring was checked only for the startup exit codes above.
- **Untouched.** The runner, the store and the bbolt layout are unchanged. The `"NoteRegistered"` and `"event-logger"` literals in the runner and store tests remain as inert fixture data, as the plan allowed.
