---
title: Event-Sourced Agent Skeleton
date: 2026-10-24
status: ready-for-dev
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
