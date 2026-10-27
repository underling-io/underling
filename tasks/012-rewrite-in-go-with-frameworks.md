---
title: Rewrite heinzel in Go with Mainstream Frameworks
date: 2026-10-25
status: done
type: tech
---

## Rewrite heinzel in Go with Mainstream Frameworks

Rewrite heinzel from scratch in idiomatic Go, built on the most common Go libraries for web servers and event sourcing instead of hand-rolled infrastructure, with the same behaviour as today. The result replaces both the current Go code and the unfinished Rust rewrite (epic 011), and ships as a single static binary with the event store embedded.

### Locked Decisions

- **Spec.** `docs/event-model.md` and the current Go tests are the behaviour spec: the HTTP route, status codes and headers, the Sentry checks, the investigation decision path and its isolation/sandbox rules, the env vars and defaults, the CLI flags (`-addr`, `-data-dir`, `-shutdown-timeout`), the exit codes (2 for invalid arguments/config, 1 for runtime errors), the log messages and fields, and the handler/checkpoint names. Port the tests' scenarios, not their mechanics.
- **Same event contracts.** Event type names and JSON payloads stay exactly as in `internal/events`.
- **Frameworks.** Mainstream, maintained, pure-Go libraries; no cgo, so `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` still builds:
  - HTTP: Gin.
  - Event sourcing: an established Go event-sourcing library (aggregates/streams with optimistic concurrency, a global event log, subscriptions or projections with checkpoints). The plan picks it and says why.
  - Embedded store: whatever embedded backend that library supports in pure Go (e.g. SQLite via `modernc.org/sqlite`, or bbolt). No external database or server.
  - Add other mainstream libraries where they clearly beat hand-rolling (CLI/config, logging stays `log/slog`).
- **Fresh store.** Existing `heinzel.db` bbolt files aren't read or migrated. Use a new file name so a leftover file is never opened.
- **Slices.** Vertical slices, one package per slice, as today. A slice holds everything it needs: route or event handler, command handling, config, domain logic, infrastructure adapters and tests (sub-packages allowed, owned by the slice). Slices never import each other. The only code shared between slices is the event contracts and a small set of utility/platform helpers (e.g. the app's slice registration and the store/bus types the framework needs). A guard test enforces this.
- **Wiring.** Slices are wired automatically: adding a slice package is enough, `main` never names a slice.
- **Shutdown order** as today: drain HTTP (forced after the shutdown timeout), cancel the automations' context, stop event handlers, close the store. Every step runs; the first error is returned.
- **Go practice.** Idiomatic, readable Go; patterns only where they pay off. Sparse comments: doc comments on exported identifiers and the odd "why", no narration. Given/when/then tests for slice logic, as in task 010.
- **Cutover in this task.** Delete the old Go code and the Rust crate (`Cargo.*`, `build.rs`, `src/`, `tests/`, the `target/` ignore). Epic 011 is superseded.

### Acceptance Criteria

- `make check` (go vet + `go test -race ./...`) passes, and `make build-linux-arm64` produces one static binary that starts with nothing but a data dir and the slices' env vars.
- The event store runs embedded in the process: append with expected version (mismatch is a conflict and stores nothing), global ordered positions, per-handler checkpoints so a restart resumes, and handlers woken on append.
- `POST /sentry/callback` behaves as specified in `docs/event-model.md` (size limit 413, signature 401, non-alert resources 200 without storing, validation 400, dedupe of redeliveries), stores `SentryIssueAlertReceived` with the identical JSON payload, and the Sentry alert end-to-end test passes against the real binary wiring.
- The investigate-Sentry-issue automation reaches every outcome in the spec (Skipped stale/unmapped, Started, PullRequestOpened, NoChanges, Failed per stage, duplicate ignored, shutdown left unhandled), with the same stream names, expected versions, branch names, PR title/body rules, CI-config refusal, and the same `claude` invocation, environment, generated sandbox settings and token handling.
- Each slice package imports only the stdlib, third-party libraries, the shared events package, the shared utility/platform packages and its own sub-packages; the guard test fails on a planted violation.
- No `.rs`, `Cargo.*`, `build.rs` or old bbolt-based store remains; `docs/event-model.md` (incl. "Adding a slice") describes the new Go code.

### Notes

- Judgement calls made without the user (auto-task): single task instead of an epic, since the user asked for one rewrite; Gin as "most common" web framework; old Go and Rust deleted in this task; flags keep Go's single-dash style.
- Host prerequisites (git, claude, codex, bubblewrap/socat) are unchanged.
- "From scratch" is read as: new code and structure written fresh, with the old tests' scenarios as the spec. The sandbox/token-isolation rules may be carried over exactly where they are the spec (the Claude planner argued for adapting rather than retyping the slice internals; overruled to follow the user's instruction).

## Implementation Plan

### TLDR

Gin for HTTP, `github.com/hallgren/eventsourcing` (core + its pure-Go bbolt event store) for the event log, plus thin app-owned glue for what the library lacks (persisted checkpoints, wake-on-append, the checkpointed handler loop). Rewrite every package fresh in idiomatic Go, using the current Go tests' scenarios and `docs/event-model.md` as the spec, in phases that each leave `make check` green. Delete Rust and the old code as each part is replaced.

### Steps

1. **Delete the Rust crate.** Remove `Cargo.toml`, `Cargo.lock`, `build.rs`, `build/`, `src/`, `tests/` and the `target/` line in `.gitignore`. Nothing Go depends on it.
2. **Dependencies.** Add `github.com/gin-gonic/gin`, `github.com/hallgren/eventsourcing/core` and `github.com/hallgren/eventsourcing/eventstore/bbolt` at their latest tags; keep `go.etcd.io/bbolt` direct (checkpoints). Never add `eventstore/sql` (pulls cgo `mattn/go-sqlite3` and testcontainers). Confirm the tagged API (`New(file)`, `Save([]core.Event)` → `core.ErrConcurrency`, `All(start)` fetcher, `Close`) from the module cache before building on it.
3. **Event store (shared platform).** A small store wrapping hallgren; the slice-facing contract:
   - `Append(stream, expectedVersion, events) (version, position, err)`; a mismatch returns a conflict error (matchable with `errors.Is`) naming the stream, stores nothing and advances nothing. An empty append is an error. 50 concurrent appends to a new stream at version 0 admit exactly one.
   - `ReadAfter(position, limit)` returns stored events (stream, version, global position, type, timestamp, raw JSON payload) in position order.
   - Named checkpoints (default 0) and a coalesced wake-up channel signalled only after a successful append.
   - Injectable clock; timestamps UTC with nanoseconds.
   - Files: events in `<data-dir>/events.db` (hallgren's bbolt format); checkpoints in `<data-dir>/checkpoints.db` (bucket `checkpoints`, name → 8-byte big-endian position, 0600, 1s open timeout so a second process fails fast). `heinzel.db` is never opened (test: a planted junk `heinzel.db` is left untouched). If the second open fails, close the first.
   - Mapping: `AggregateType` constant `"heinzel"`, `AggregateID` = stream, `Version` = expected+1+i, `Reason` = event type, `Data` = payload bytes as-is, `GlobalVersion` = position.
   - Load-bearing: create a fresh fetcher per `ReadAfter`, call it once, copy at most `limit` events and close it before returning. An open iterator holds a bbolt read transaction, which blocks `Close` forever; a long investigation must never run while one is open.
4. **Event handler runner (shared platform).** Not hallgren's `Projection` (in-memory positions, polling, no stop between events, holds a read tx across callbacks). Own loop: one goroutine per named handler, resume after its checkpoint, read in batches, check stop before each event (never interrupt `Handle`), checkpoint after each event, sleep on wake-up or stop. Same log lines ("stopping event handlers", "event handler stopped") and the existing runner test scenarios.
5. **App, wiring and the record slice — one green step.**
   - Slice kinds: a state change is `{Method, Path, Handler func(store) gin.HandlerFunc}`; an automation is a handler factory taking the automations' context and the store. Factories take deps (env lookup, logger) and may fail → "invalid configuration", exit 2.
   - An exported router builder (store + state changes → `http.Handler`) used by `Run` and by slice tests, so no slice builds Gin itself. `gin.New()` without Logger/Recovery; `HandleMethodNotAllowed = true` (405 like ServeMux); `RedirectTrailingSlash = false`; release mode set once at package init (`gin.SetMode` per router races under `-race`).
   - `Run` serves through its own `http.Server` (ReadHeaderTimeout 10s, slog error log) on the given listener — never `engine.Run` — and keeps the locked shutdown order and "app started"/"app shutting down" fields.
   - CLI: stdlib `flag` (`-addr :8080`, `-data-dir ./data`, `-shutdown-timeout 10s`); pflag/cobra would break single-dash long flags. Logs: `log/slog` JSON on stderr.
   - Registration: each slice registers in `init()`; `go generate` writes one file blank-importing every slice dir; `main` imports only that package. Keep the generated-file freshness, slice-dir and registration tests.
   - Record slice `POST /sentry/callback`: read through `http.MaxBytesReader(c.Writer, c.Request.Body, 10 MiB)` (413 via `*http.MaxBytesError`), verify the signature before decoding, decode with `encoding/json` (not Gin binding — it changes what is a 400), respond with `c.Data(status, "application/json", …)` (`c.JSON` adds `; charset=utf-8`, which the tests assert against). Port all callback, config and exception scenarios and the e2e test.
6. **Investigate slice.** Rewrite the slice and its slice-owned sub-packages (workspace, claude, github) against the spec section and existing tests, keeping: handler/checkpoint name, stream names and expected versions, branch naming, stale/unmapped/duplicate/shutdown handling, every outcome and failure stage, git timeouts and low-speed limits, token only via `GIT_CONFIG_*`, mirror push with hooks off, process-group kill, run-dir cleanup, work-dir ownership/mode checks, exact `claude` argv, child env and generated sandbox settings JSON, CI-config refusal, PR title/body caps, fence closing and `@` neutralising. Given/when/then tests for the investigator's decision path; keep the fake-claude re-exec and real-git tests.
7. **Guard test.** A checker function (dir + slice import path) table-tested on temp dirs with planted violations: another slice, a sibling slice from a sub-package, any non-shared `heinzel/...` package. Allowed: stdlib, third-party, `internal/events`, the shared platform packages, own sub-packages. Keep the "events holds only contracts" test.
8. **Delete the old implementation** as each replacement lands (old bbolt store layout, old app/slices code); no leftovers at the end.
9. **Build and docs.** `make build-linux-arm64` succeeds with `CGO_ENABLED=0`; `go list -deps ./cmd/heinzel` has no `runtime/cgo` or `mattn`. Rewrite `docs/event-model.md` for the new code: event store section (two files, mapping, own checkpoints/wake-ups, old `heinzel.db` ignored), the new state-change shape and Gin paths, testing routes via the router builder, and the import rule.

### Plan Notes

- Library choice: looplab/eventhorizon and modernice/goes have no embedded store (Codex proposed eventhorizon + a hand-written bbolt adapter — rejected, it would hand-roll the store anyway); Watermill has no per-stream optimistic concurrency; hallgren is the maintained one with a pure-Go embedded backend. hallgren's SQLite store truncates timestamps to seconds and doesn't lock out a second process.
- At-least-once delivery between `Handle` and the checkpoint is as today, made safe by the investigator's expected-version appends.
- No go-github or go-git: one PR endpoint with a strict 201 rule, and the git CLI carries the token/mirror/timeout rules.

## Implementation Notes

- **Packages.** Shared platform: `internal/platform/eventstore` (hallgren bbolt store in `events.db` + own `checkpoints.db`, wake-ups, injectable clock), `internal/platform/eventhandler` (checkpointed runner), `internal/platform/app` (registry, `Router` builder on Gin, `Run`, `ParseFlags`). `internal/events` is unchanged (locked contracts). Slices: `recordsentryissuealert` (Gin handler, `alert.go` for decoding/caps) and `investigatesentryissue` with sub-packages `workspace` (now split into `workspace.go` + `git.go`), `claude` (`command.go`, `run.go`, `stream.go`, `paths.go`) and `github`. `cmd/heinzel/main.go` names no slice.
- **Deviation: `claude` no longer imports `workspace`.** `claude.Command` takes a small `claude.Dirs` struct; `infrastructure.go` maps the workspace onto it, so the sub-packages are independent.
- **Deviation: guard test lives in `internal/slices/imports_test.go`** as `importViolations(dir, allowed)` with `sliceMayImport(self)`, table-tested on temp dirs. The shared platform is matched by the `heinzel/internal/platform/` prefix rather than a list; a second check (`platformMayImport`) keeps everything under `internal/platform` from importing `internal/slices/...`, so the prefix can't launder cross-slice deps (review F2). The old doc rule "a sub-package must not import its parent" is dropped (it was never enforced; Go's import cycles cover the usual case).
- **Deviation: some test files were ported by adapting the old ones** (record callback tests, investigator GWT tests, workspace/claude/github tests, e2e and fake-claude tests): the scenarios are the spec and the internal names (`investigator`, `cloner`/`run`/`prOpener`, `config` fields) were kept, so only mechanics changed (Gin router via `app.Router`, `Event.Stream` instead of `StreamID`, `claude.Dirs`).
- **Deviation (open for the user): "from scratch" was only partly met.** New: the event store and checkpoints, `app` run/router/registration, the record slice's alert and callback code, the import guard, and the restructured workspace and claude run/stream code. Adapted from the old code (same logic, renamed or reworded, roughly 65–100% of lines unchanged): the handler runner, the investigator, the investigate slice's config, github, infrastructure and root files, `claude/paths.go`, the `go generate` wiring and `main.go`. Most slice tests and roughly half of the platform tests are the old tests with renamed calls. Options: accept this, or send the slice internals and the runner back for a genuine rewrite. **User decision (2026-10-27): accepted as adapted.** Also decided: the forced-shutdown race (F1) stays as it is, matching the old behaviour.
- Gap-check leftovers, deliberately not changed: Gin answers 404 for non-canonical paths (`//sentry/callback`) where ServeMux redirected; response bodies have no trailing newline and the router's own 404/405 bodies differ (status codes and `Allow` unchanged); the conflict error no longer names the stream's current version (hallgren doesn't report it). Follow-up: a test that runs the built binary (flags, exit codes, SIGTERM), which the old Go didn't have either.
- **`go list -deps` check.** No `runtime/cgo` and no `mattn/go-sqlite3`. It does list `github.com/mattn/go-isatty` (pure Go, pulled in by Gin itself) and `quic-go` (Gin v1.12's HTTP/3 support, pure Go). The plan's "no mattn" was read as "no go-sqlite3"; unavoidable with Gin.
- **Small behaviour notes.** Config errors for missing secrets now read `GITHUB_TOKEN: must be set` (still naming the variable) and are reported in a fixed order. Encoding the investigation event contracts can't fail, so the old "encode investigation started/outcome" error logs are gone.
- **Not verified manually:** running the built binary on the host (a manual smoke run was denied in this session). The in-process e2e test (`internal/slices/sentryalert_e2e_test.go`) builds the slices through `app.Build` exactly as `main` does and runs `app.Run`. The linux-arm64 binary is statically linked (`file`).
- **Review F1 (forced shutdown vs store close): kept as before, surfaced to the human.** A request still running when `-shutdown-timeout` expires is abandoned (as in the old Go, `TestShutdownCompletesWhenARequestOutlivesTheTimeout`); the store is then closed under it. This is safe: bbolt's `Close` waits for an in-progress write transaction and a later `Append` gets a clean `database not open` error (500, nothing stored; `TestStoreFailureIsInternalErrorWithoutDetails`), and the client never got a 2xx, so no accepted alert is lost. Possible follow-up if wanted: a bounded extra wait for in-flight handlers after `srv.Close()`.
- **Review F3:** the record handler hands `http.MaxBytesReader` the writer under Gin's (`Unwrap`), so net/http again closes the connection after a too-large body instead of draining it.
- `go mod tidy` also adds Gin's test-only deps (go.uber.org/mock etc.) to go.sum; they don't reach the binary.
