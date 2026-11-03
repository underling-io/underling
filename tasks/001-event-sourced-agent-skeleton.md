---
title: Event-Sourced Agent Skeleton
date: 2026-10-24
status: done
type: feat
---

## Event-Sourced Agent Skeleton

A Go agent, shipped as a single binary, that accepts commands over HTTP, stores the events they produce in a bbolt file, and processes those events asynchronously. It runs end to end with one placeholder command so the real domain can be added later.

### Acceptance Criteria

- A build target cross-compiles a static linux/arm64 binary with CGO off. It runs with nothing else on disk except its data directory.
- The listen address and the data directory are set with command-line flags, and both have sensible defaults.
- `POST /commands` with `{"type": "RegisterNote", "payload": {"id": "...", "text": "..."}}` runs the command handler, which appends a NoteRegistered event to stream `id`. The response is 201 with `{"streamId": "...", "version": 1, "position": N}`.
- `id` and `text` must both be non-empty. Malformed JSON, failed validation or an unknown command type returns 400 with `{"error": "..."}`, and no event is stored.
- Registering an `id` that already exists returns 409 with `{"error": "..."}`. The stream must be new, so the store's expected-version check rejects the append.
- When concurrent requests register the same `id`, exactly one gets 201 and the rest get 409. Global positions stay gapless.
- Each event is stored with a stream ID, its version within the stream, a global position, a type, a timestamp and a payload. Stream versions and global positions start at 1.
- Event handlers process each stored event after the command has returned, in global order. The placeholder handler writes one structured log line per event with its position, stream ID, version and type.
- Each handler has its own named checkpoint in the same bbolt file as the events. After a restart a handler resumes from its checkpoint, so events it already processed are not logged again. Delivery is at-least-once if the agent crashes between handling an event and saving the checkpoint.
- On SIGTERM/SIGINT the agent does the following, in order, even if the timeout expires:
  1. Stops accepting new requests and lets in-flight requests finish within a timeout (a flag, with a sensible default).
  2. Stops the handlers. Each finishes and checkpoints the event it's on, and anything still unprocessed is picked up on the next start.
  3. Closes the store.

### Notes

- Use the standard library for HTTP and logging. The only expected external dependency is `go.etcd.io/bbolt`.
- Keep the command side, the event store and the event handlers separate, so it's easy to add more commands and handlers later.
- Handler error handling (retry, skip or halt) is out of scope. The placeholder handler can't fail.
- The concurrency, resume and shutdown criteria need automated tests, not just a manual smoke test.

## Implementation Plan

### TLDR

A stdlib-only Go agent whose single external dependency is `go.etcd.io/bbolt`. It has three parts:
- A command side: a registry of command handlers.
- A bbolt event store that runs the expected-version check and assigns positions in one write transaction.
- Event handlers, each running its own checkpointed loop and woken after every commit.

The module is `underling`, built from `./cmd/underling`. The 409 and gapless positions come from bbolt's single-writer transaction. Don't add app-level locks or a pre-check in a separate read, because that would race.

### Steps

1. **Module and build.** Use module `underling` (go 1.27) and the latest v1 of `go.etcd.io/bbolt`.
   - Add a Makefile with:
     - a cross-build target, `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`, output `bin/underling-linux-arm64`;
     - a `check` target that runs `go vet ./...` and `go test -race ./...`.
   - `bin/` is gitignored.
   - Check: `file` reports a statically linked aarch64 ELF.
   - The binary needs nothing on disk except the data dir. Use UTC timestamps, so no tzdata.
2. **Persisted layout. This is a locked contract.** The file is `<data-dir>/underling.db`. Create the directory if it's missing. Open with a short bbolt timeout, so a second instance fails fast instead of hanging. Buckets:
   - `events`: the key is the global position as an 8-byte big-endian uint64. The value is JSON `{"streamId","version","position","type","timestamp","payload"}`. `timestamp` is UTC RFC3339Nano, set by the store. `payload` is raw JSON.
   - `streams`: the key is the stream ID. The value is the current version as an 8-byte big-endian uint64.
   - `checkpoints`: the key is the handler name. The value is the last processed position as an 8-byte big-endian uint64. A missing key means 0.
3. **Store operations.**
   - **Append(stream, expectedVersion, events).** All of it runs in one `Update` transaction:
     - Expected 0 means the stream must not exist. A mismatch returns a sentinel conflict error the caller can check with `errors.Is`, and nothing is written.
     - Versions continue from the stream's current version. Positions continue from the last `events` key, so both start at 1.
     - It returns the last version and position.
     - After a commit, it sends a non-blocking, coalescing notification to each subscriber (a buffered channel of size 1).
   - **Also needed:** read up to N events after a position, and get or set a named checkpoint.
4. **Command side.**
   - The registry maps a command type to a handler. A handler decodes and validates its payload and returns the stream, the expected version and the events.
   - The dispatcher maps outcomes as follows: validation error → 400, conflict → 409, anything else → 500.
   - RegisterNote:
     - `id` and `text` must be non-empty after trimming whitespace.
     - The stream is `id` and the expected version is 0.
     - It emits one NoteRegistered event with payload `{"id","text"}`.
   - The registry is injectable, so tests can add commands.
5. **HTTP.** Use the stdlib mux pattern `POST /commands`. Wrap the body in `http.MaxBytesReader` (1 MiB). All responses are `application/json`.
   - Return 400 `{"error"}` for: malformed JSON, trailing data after the object, a missing or non-object `payload`, an empty or unknown `type`, or failed validation.
   - Return 201 `{"streamId","version","position"}` on success.
   - Return 409 or 500 `{"error"}` for a conflict or any other failure. A 500 must not leak bbolt internals.
6. **Event handlers.** Each handler has a fixed name, which is its checkpoint key. The placeholder handler is `event-logger`. The loop:
   - Load the checkpoint.
   - Read a batch of events after it, then close the read transaction.
   - Handle each event, then save its checkpoint in its own `Update`.
   - When a batch comes back empty, wait for a notification or a stop. Always re-read after a notification. That way a coalesced wakeup can't be missed, and catch-up at startup is just the first pass.
   - Never hold a transaction while the handler runs.
   - On stop, finish and checkpoint the current event, then return.
   - The logger is `log/slog` JSON to stderr, injected. It writes one line per event with `handler`, `position`, `streamId`, `version` and `type`.
7. **Lifecycle.** Flags: `-addr` (`:8080`), `-data-dir` (`./data`), `-shutdown-timeout` (`10s`).
   - The whole lifecycle runs behind a function that takes a context, the config, a listener, the registry, the handlers and a logger. Tests drive it in-process. `main` only wires up flags, `signal.NotifyContext(SIGINT, SIGTERM)` and the exit code.
   - On cancel, do the following in order and unconditionally:
     1. Call `Server.Shutdown(timeout)`. If it times out, call `Server.Close`.
     2. Stop all handler loops and wait for them.
     3. Close the store.
   - Return the first error, but never skip a later step because of it.

### Notes

- Tests run with `-race` against a real bbolt file in `t.TempDir()`. The scenarios the ACs need:
  - **Concurrency, same `id`:** about 50 concurrent POSTs give exactly one 201 and the rest 409.
  - **Concurrency, distinct `id`s:** after many concurrent POSTs, the `events` keys are exactly 1..M.
  - **Resume:** handle K events, restart on the same dir, append more. Only the new positions are handled, and the persisted checkpoint value is correct.
  - **Shutdown:** use a blocking test command and a test handler that blocks mid-event.
    - An in-flight request completes.
    - The mid-event handler is checkpointed at that event, and later events stay unprocessed until a restart.
    - Afterwards the store reopens with a short timeout.
    - Repeat with a request that outlives the timeout: run still returns.
  - **400 table test:** each 400 case, with no events stored.
- `bbolt.Close` waits for open transactions, so closing the store can briefly block behind a request that `Server.Close` cut off. That's acceptable.
- The handler contract may have no error return. Failure handling is out of scope.

## Implementation Notes

- **Layout.** The packages are `internal/eventstore` (bbolt store), `internal/command` (registry and RegisterNote), `internal/httpapi` (POST /commands), `internal/eventhandler` (checkpointed runner and `event-logger`) and `internal/agent` (`Run` lifecycle and `ParseFlags`). `cmd/underling` only wires flags, signals and the exit code.
- **Where the dispatcher lives.** It is in `httpapi`. That package decodes the request, calls `command.Registry.Decide`, appends to the store and maps errors to status codes: `command.ErrInvalid` → 400, `eventstore.ErrConflict` → 409, anything else → 500 with a generic `"internal error"`, and the cause is logged. This keeps HTTP status codes out of the `command` package. Command handlers return a `command.Decision` (stream, expected version, events), and `command.NewRegistry()` is empty while `command.DefaultRegistry()` adds RegisterNote.
- **Flag parsing.** It lives in `agent.ParseFlags(args, output)` rather than in `main`, so the defaults are unit-tested. It also rejects positional arguments.
- **Stored values are not trimmed.** RegisterNote trims `id` and `text` only to validate them and stores them exactly as sent. So `" a "` and `"a"` are different streams.
- **`Append` with no events returns an error.** This guard was not in the plan.
- **The payload-object check is tested separately.** RegisterNote would reject a missing, null or non-object payload on its own, so the check that `payload` must be a JSON object is tested with a permissive test command, to prove the HTTP layer enforces it for every command.
- **A shutdown test race, found and fixed.** If a test released a mid-event handler before `Runner.Stop` had cancelled, the handler could go on to process later events. `Runner.Stop` now logs `"stopping event handlers"` after it cancels, and both stop tests wait for that line before releasing the handler.
- **Detecting that the listener is closed.** Polling with `net.Dial` exhausted ephemeral ports on macOS (`can't assign requested address`) and let a mutant survive. The agent tests now wrap the listener and get a signal when it is closed. The reordering mutants are killed: stopping the handlers before HTTP, or closing the store before the handlers, each fails `TestShutdownDrainsRequestsThenStopsHandlersThenClosesStore`.
- **What `Run` returns when the shutdown timeout expires.** It returns the `Server.Shutdown` timeout error after it has finished stopping the handlers and closing the store, so `main` exits 1 in that case.
- **Manual smoke test.** The implementer could not run it, but the task review later ran it on a darwin build. It got 201, then 409 for the duplicate and 400 for an unknown type. The event-logger wrote its log line, SIGTERM shut the agent down cleanly, and the only file created was `data/underling.db`.
- **Follow-ups from review** (not blocking):
  - If the shutdown timeout expires and `Server.Close` cuts the connection, a command handler can keep running after `Run` returns. Today there is no data-integrity impact, because an append after the store closes fails. Revisit when real commands do slow work. (Raised by Codex.)
  - The 400 and 409 bodies echo raw Go messages: `encoding/json` type errors and the stream's current version. An oversized body is reported as "malformed JSON".
  - The release build keeps debug info. `-ldflags="-s -w"` would shrink it.
