---
title: Sentry Alert End-to-End Test
date: 2026-10-25
status: ready-for-dev
type: tech
---

## Sentry Alert End-to-End Test

Add an in-process integration test that sends a signed Sentry issue-alert webhook to the running agent, and checks that the `SentryIssueAlertReceived` automation handles it. It covers the whole path: registry wiring, the HTTP route, the event store and the handler runner.

### Acceptance Criteria

- The test wires the agent the way `main` does: slices come from `agent.Build` (the real registrations under `internal/slices`, no hand-built `agent.Slices`), and `agent.Run` serves them on a fresh data dir.
- It POSTs a correctly signed `event_alert` webhook to `POST /sentry/callback` and expects `200` with status `recorded`. It then waits, with a timeout and no sleeps, until `sentry-issue-alert-logger` logs "sentry issue alert received" with the posted `issueId`.
- It runs as part of `go test ./...`, with no build tag, no real network port and no external service. It shuts the agent down cleanly at the end.

### Notes

- "Writes to Sentry" means sending what Sentry would send to our webhook. Nothing calls the real Sentry.
- The sandbox here (and possibly CI) forbids binding any socket, TCP or unix. Serve `agent.Run` on an in-memory `net.Listener` instead, and give the HTTP client a matching dialer.
- The handler's only observable effect today is its log line, so capture the logger passed through `agent.Deps`.
