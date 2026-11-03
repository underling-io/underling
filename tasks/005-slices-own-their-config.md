---
title: Slices Own Their Config
date: 2026-10-25
status: done
type: tech
---

## Slices Own Their Config

Each slice loads its own configuration, so `main` no longer knows any slice-specific setting. Today `main` reads the Sentry client secret and passes it into the Sentry state-change slice, which spreads that slice across two packages.

### Acceptance Criteria

- `recordsentryissuealert.New` takes the environment lookup (`func(string) string`) and a logger and returns `(agent.StateChange, error)`. It reads and validates `SENTRY_CLIENT_SECRET` itself. A missing or empty secret is an error that names the variable.
- `SecretFromEnv` and the secret's env-var constant are no longer exported. `main` mentions no slice-specific config: no secret, no env-var name. It passes `os.Getenv` to slice constructors that need config.
- Startup behaviour doesn't change. Without the secret, the process logs "invalid configuration" with the error and exits 2. `-h` exits 0.
- The existing config tests now exercise the secret through `New`, and keep their missing and empty cases.
- `docs/event-model.md` "Adding a slice" says a slice loads its own config inside its constructor (from the lookup it's given) and returns an error if the config is invalid. It no longer says `main` reads the slice's config.

### Notes

- The constructor takes the lookup func, rather than calling `os.Getenv` itself, so tests can inject values. This follows the existing `SecretFromEnv(getenv)` pattern.
- Slices that need no config (`handlesentryissuealert`) keep their current signature. Don't add a common config interface or a registry: `main` still lists the slices explicitly.
- `agent.ParseFlags` / `agent.Config` (addr, data dir, shutdown timeout) are agent-wide settings and stay as they are.

## Implementation Plan

### Steps

1. **`recordsentryissuealert`:** `New(getenv func(string) string, logger *slog.Logger) (agent.StateChange, error)`. It calls the now-unexported secret loader and returns its error unwrapped, so the message still reads `SENTRY_CLIENT_SECRET must be set`. Unexport the loader and the env-var constant. Update the package and `New` doc comments to say the slice loads its secret from `SENTRY_CLIENT_SECRET`.
2. **Tests in that package:** the config tests call `New` with an injected env. A set secret gives no error, and missing or empty gives an error containing `SENTRY_CLIENT_SECRET`. The callback tests' `newMux` builds the slice through `New` with an env holding the test secret, so the signed-request tests prove the secret from the env is the one used for verification.
3. **`main`:** build the state change with `recordsentryissuealert.New(os.Getenv, logger)`. On error, log "invalid configuration" with the error and return 2, the same branch and order as today (after flag parsing, before listening). `main` must not mention `SENTRY` or `secret`.
4. **`docs/event-model.md`:** replace "`main` also reads the slice's config" with the AC wording (the constructor loads its config from the lookup it's given and returns an error if it's invalid). The constructor shape there becomes `New(getenv, …) (agent.StateChange, error)` for slices with config. Adjust the Sentry section's "The slice also loads the client secret" line if needed.

### Notes

- `cmd/underling` has no tests, and the exit-code behaviour is checked by building and running the binary. Without the secret: `env -u SENTRY_CLIENT_SECRET go run ./cmd/underling; echo $?` → 2, plus an "invalid configuration" log line. `go run ./cmd/underling -h` → 0.
- Sandbox: Bash can't write in the worktree (use Edit/Write; check formatting with `gofmt -l`). Go needs `GOCACHE=$TMPDIR/gocache` (and `go run` may need `GOTMPDIR=$TMPDIR`). The `internal/agent` tests can't bind ports here, which the user accepted. They are untouched by this task anyway.
- Verify with `grep -nE 'SENTRY|[Ss]ecret' cmd/underling/main.go`, which should return nothing.

## Implementation Notes

- The plan was followed with no deviations in scope. `main` names the built slice `recordAlert` and puts it in `StateChanges`. The error check sits in the same place as before: after flag parsing, before listening.
- The plan's exit-code check is slightly off. `go run` always exits 1 when the program fails, and prints `exit status 2` instead. So `go run ...; echo $?` shows 1, not 2. The real exit code was checked with a built binary (`go build -o $TMPDIR/underling ./cmd/underling`). With the secret unset it exits 2, and with it empty it also exits 2. Both log `"msg":"invalid configuration","error":"SENTRY_CLIENT_SECRET must be set"`. `-h` exits 0. Future plans should check exit codes with a built binary, not `go run`.
- `newMux` in `callback_test.go` now takes `t` so it can fail on a constructor error. Every call site changed from `newMux(s)` to `newMux(t, s)`.
- The new config tests also check that the error names `SENTRY_CLIENT_SECRET`. The old tests only checked that some error came back.
- `docs/event-model.md` gets a separate "Adding a slice" bullet on config, so the constructor bullet stays about slice kinds. The Sentry section now says the constructor loads the secret and fails if it is missing or empty.
- Verification: `gofmt -l` and `go vet ./...` are clean. `go test -race ./...` passes everywhere except `internal/agent`. Its four failures are all `bind: operation not permitted` from the sandbox, which the user accepted. That package still compiles and wasn't touched by this task.
