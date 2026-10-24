---
title: Sentry Alert End-to-End Test
date: 2026-10-25
status: done
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

## Implementation Plan

### Steps

1. **Location:** put a new test file in `internal/slices`, in package `slices_test`. It imports `heinzel/internal/slices` (blank import) so every slice registers, exactly as `main` gets them. It must not import any individual slice package, so it keeps working as slices are added.
2. **In-memory listener (test helper in that file):** a `net.Listener` backed by `net.Pipe`. `Accept` returns server-side conns from a channel. `Close` is idempotent and unblocks `Accept` with `net.ErrClosed`, because `http.Server.Serve` treats that as a normal stop on shutdown. `Addr` returns any fixed address. The `http.Client` gets a `Transport` whose `DialContext` hands the client end of a new pipe to the listener (and fails once it's closed). Disable keep-alives on that transport, so no idle conn holds up shutdown.
3. **Run the agent:**
   - Call `agent.Build` with a `Getenv` that serves `SENTRY_CLIENT_SECRET` from a test map, and a JSON `slog` logger writing to a goroutine-safe sink.
   - Call `agent.Run` in a goroutine, with `agent.Config{DataDir: t.TempDir(), ShutdownTimeout: a few seconds}`, the pipe listener and that logger.
   - On cleanup, cancel the context and require `Run` to return nil within a bounded wait.
4. **Send the webhook:**
   - POST a minimal valid `event_alert` body to `http://<anything>/sentry/callback`, with `Sentry-Hook-Resource: event_alert` and `Sentry-Hook-Signature` set to the hex HMAC-SHA256 of the body.
   - Assert `200` and JSON `{"status":"recorded"}`.
   - The body needs the fields validation requires (`data.event.event_id`, `data.event.issue_id` as a string, `data.triggered_rule`). See `recordsentryissuealert/callback_test.go` for the shape, and copy a minimal version. Test packages can't share it.
5. **Observe the handler:** the sink pushes each decoded JSON log record to a channel. Wait (select with a timeout of a few seconds, no sleeps) for a record with `msg == "sentry issue alert received"`, `handler == "sentry-issue-alert-logger"` and `issueId` equal to the posted one. Fail with the records seen so far if it doesn't arrive.

### Notes

- Prove the test isn't vacuous. In a `$TMPDIR` copy, break the signature or skip the POST and see the test fail on the timeout, then restore.
- Sandbox: use Edit/Write in the worktree, check formatting with `gofmt -l`, and run Go with `GOCACHE=$TMPDIR/gocache`. This test must pass here: it binds no socket. Run it with `-race -count=5` to catch flakiness.

## Implementation Notes

- The test is in `internal/slices/sentryalert_e2e_test.go` as `TestSentryIssueAlertIsRecordedAndHandled`. No production code changed.
- Test first: the first version expected `issueId == "wrong-1117540176"` and failed after the 5s timeout. Its failure message showed the real handler record with `issueId:1117540176`, so the whole path already worked. Then the expectation was corrected.
- Deviation in the log sink: the plan said "push each record to a channel". The sink instead keeps the records in a mutex-guarded slice and pings a 1-buffered "arrived" channel. Writers never block and no record is dropped, however much the agent logs. The waiter rescans after each ping, and the timeout message lists every record seen.
- The listener's `Dial` also gives up when the dial context is cancelled, so a client timeout can't leave a dial blocked forever. The client has a 5s `Timeout`.
- Shutdown is checked: cleanup cancels the context and requires `agent.Run` to return nil within 10s. `t.TempDir()` is created before that cleanup is registered, so the data dir is removed only after the store is closed.
- Non-vacuity, run in a `git archive` copy under `$TMPDIR`. Each change made the test fail, and the restored copy passed:
  1. Signing a different body: `got 401 "", want 200 "recorded"`.
  2. Skipping the POST (wait only): timeout, no matching log record.
  3. The handler logging `EventID` as `issueId`: timeout.
  4. `agent.Build` dropping automations: timeout.
  5. `agent.Run` returning an extra error: `agent.Run returned boom, want nil`.
- Sandbox: as expected, the 4 `internal/agent` Run tests still fail at `net.Listen` with "bind: operation not permitted". Everything else passes `go vet ./... && go test -race ./...`.
