---
title: Rename Agent Package to App
date: 2026-10-25
status: done
type: tech
---

## Rename Agent Package to App

Rename `internal/agent` to `internal/app` (package `app`). It holds the slice kinds, the registry and the process lifecycle, and "agent" doesn't describe that.

### Acceptance Criteria

- `internal/agent` no longer exists. `internal/app` holds the same code and tests with the same exported names (`app.StateChange`, `app.Automation`, `app.Slices`, `app.Deps`, `app.RegisterStateChange`, `app.RegisterAutomation`, `app.Build`, `app.Run`, `app.Config`, `app.ParseFlags`), and its behaviour doesn't change.
- Every reference to the package or the running process says "app" instead of "agent": imports, call sites, identifiers, comments, test names and helpers, docs, error messages from the slice tests, and the lifecycle log messages ("app started", "app shutting down", "app stopped with error"). Historical task files are left alone. `grep -rniI agent cmd internal docs` returns nothing.
- `make check` passes, and the slice import guard and registration test accept `heinzel/internal/app` in place of `heinzel/internal/agent`.

### Notes

- The user chose `app` over `application` (too long at call sites) and `host`.
- The log-message rename is a deliberate, visible change: nothing parses these messages yet.

## Implementation Plan

### Steps

1. **Already done** (commits `8399165`, `16b9938`): `git mv internal/agent internal/app`, plus `agent.go` / `agent_test.go` renamed to `app.go` / `app_test.go`. The contents still say `package agent`.
2. **`internal/app`:** change the package clause to `app` and the external test package to `app_test`. Update the package doc, the comments and the three lifecycle log messages. Rename test helpers and test names that say agent.
3. **Callers:** update the import path and qualifier everywhere: `cmd/heinzel/main.go`, both slices, and the tests in `internal/slices` (e2e test, registration test, slicedirs fixtures, import guard allowlist). In `registration_test.go`, the `agentPath` constant becomes `appPath`, the default qualifier `"agent"` becomes `"app"`, and the `agentName` identifiers and messages change to match. The "same name on another package" fixture should use alias `app`, so it still tests a look-alike.
4. **Docs:** update `docs/event-model.md` and the comments in `gen.go`, `doc.go` and `generated_test.go`. The `cmd/heinzel/main.go` doc becomes "runs the event-sourced app".
5. **Check:** `grep -rniI agent cmd internal docs` returns nothing. `go vet ./...` passes. `go run gen.go -o $TMPDIR/x.go` from `internal/slices` still matches `slices_gen.go`. The e2e test passes.

### Notes

- Sandbox: Bash can't write in the worktree, so use Edit/Write and check formatting with `gofmt -l`. Go needs `GOCACHE=$TMPDIR/gocache GOTMPDIR=$TMPDIR`. The `internal/app` `Run` tests fail at `net.Listen` with "bind: operation not permitted", which is accepted. Everything else must pass.

## Implementation Notes

- Followed the plan as written, one commit per step. Every file that mentioned "agent" took a straight `agent`→`app` / `Agent`→`App` swap. Nothing needed rewording: "an `app.StateChange`" still reads fine. Renamed identifiers: `agentPath`→`appPath`, `agentName`→`appName`, `startAgent`→`startApp`, `TestRegisterCallsCountsAgentRegistrations`→`TestRegisterCallsCountsAppRegistrations`. The look-alike fixture now imports `example.com/other` under the alias `app`.
- The plan mentioned "slicedirs fixtures", but no fixture outside `registration_test.go` referred to the package, so that bullet had nothing to change.
- Only one line in `imports_test.go` needed gofmt realignment, and I did it by hand. Import order stayed valid everywhere because `app` sorts where `agent` did.
- Verification in the sandbox: `grep -rniI agent cmd internal docs` finds nothing, and no non-task tracked file mentions agent. `gofmt -l .` is clean and `go vet ./...` passes. `go run gen.go` produces output identical to `slices_gen.go`. `go test -race ./...` passes except the four `internal/app` `Run` tests, which fail at `net.Listen` ("bind: operation not permitted"), as accepted. The in-memory e2e test `TestSentryIssueAlertIsRecordedAndHandled` passes. Run `make check` outside the sandbox once to confirm the four socket tests.
