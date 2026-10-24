---
title: Vertical Slices and Sentry Callback
date: 2026-10-24
status: new
type: feat
---

## Vertical Slices and Sentry Callback

Restructure the agent into vertical feature slices, event-modeling style, and add the first real slice: a Sentry issue-alert callback.

### Acceptance Criteria

- Each feature lives in exactly one package under `internal/features/<name>/` that holds everything for it: HTTP route, command handling and its event handler(s).
- A feature package's non-stdlib imports are limited to an explicit allowlist: the shared events package plus generic infrastructure (event store, handler runner, and whatever HTTP/lifecycle package remains). Never another feature package or a shared domain/helper package. An automated test enforces this.
- The shared events package contains only event contracts, no behaviour.
- `POST /sentry/callback` accepts Sentry internal-integration webhooks. `Sentry-Hook-Signature` must be the hex HMAC-SHA256 of the raw body keyed with the client secret, verified before decoding. Missing or wrong signature → 401, nothing stored. Body over 10 MiB → 413, nothing stored.
- A validly signed webhook with `Sentry-Hook-Resource: event_alert` stores one SentryIssueAlertReceived event with at least: Sentry event id, issue id, project (numeric `data.event.project`), title, level, web URL and triggering rule name. Returns 2xx.
- Redelivery of the same alert (same Sentry event id and rule name) stores nothing new and returns 2xx. The same event triggering a different rule is a new alert and is stored.
- Validly signed webhooks with any other, missing or unknown resource return 2xx and store nothing. A validly signed `event_alert` with malformed JSON or a missing event id / issue id returns 400 and stores nothing.
- The client secret comes from env var `SENTRY_CLIENT_SECRET`. If it is missing or empty the agent logs an error and exits with code 2 before listening.
- The Sentry slice's event handler writes one structured log line per SentryIssueAlertReceived event (handler name, position, issue id, title, level) and ignores other event types.
- The RegisterNote command, the generic event-logger, the generic `POST /commands` endpoint and the `internal/command` package are removed. Infrastructure tests use test-only commands/handlers instead.
- `docs/event-model.md` describes each slice as trigger → command → event → reaction (for Sentry: a state-change slice and an automation slice).

### Notes

- Don't change the event store, runner or persisted bbolt layout beyond what removing the placeholders requires.
