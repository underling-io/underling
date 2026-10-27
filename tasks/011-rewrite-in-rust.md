---
title: Rewrite heinzel in Rust
date: 2026-10-25
status: rejected
type: tech
---

## Rewrite heinzel in Rust

Rewrite heinzel in idiomatic Rust, with the same behaviour as the Go version: HTTP API, event contracts, config, logs, slice architecture and safety properties. The Rust crate grows next to the Go code until it reaches parity. The last sub-task then deletes the Go code.

## Locked Decisions

- **Layout.** One Cargo package `heinzel` (lib + bin) at the repo root (`Cargo.toml`, `src/`, `tests/`), next to the Go tree until cutover. `target/` is gitignored.
- **Crates.** Mainstream picks, no hand-rolling:
  - `tokio` for async, `tokio-util` for its `CancellationToken`;
  - `axum` for HTTP, `reqwest` with rustls for the GitHub API;
  - `serde`/`serde_json`, `tracing` + `tracing-subscriber` (JSON to stderr), `clap` (derive);
  - `thiserror` in the library, `anyhow` only in `main`;
  - `redb` for the event store; `hmac`/`sha2`/`hex` for crypto.
  
  Add others when a mainstream crate clearly fits.
- **Fresh store.** `redb` replaces bbolt, and existing bbolt data files aren't read. The store keeps the Go store's semantics:
  - a global position;
  - per-stream versions;
  - append with an expected version, where a mismatch is a conflict error;
  - read after a position;
  - named handler checkpoints;
  - a wake-up notification on append;
  - an injectable clock for event timestamps.
- **Same contracts.** Persisted event types and JSON payloads are identical to `internal/events` (camelCase, optional fields omitted when empty). So are:
  - the HTTP routes, status codes and headers;
  - the env vars and defaults (durations accept the Go formats used in the docs, e.g. `30m`, `1h`);
  - the CLI flags (`--addr`, `--data-dir`, `--shutdown-timeout`; Go's single-dash spelling may stay supported);
  - the exit codes (2 for invalid arguments/config, 1 for runtime errors);
  - the log message strings and field names, and the handler and checkpoint names.
- **Slices.** These carry over from the Go version: vertical slices, state change vs automation, slices own their config, wired automatically, coupled only through events.
  - Each slice is a module dir `src/slices/<slice>/`.
  - A `build.rs` scans `src/slices/*/` and generates the module list and registration, so adding a slice dir is enough. Rust has no `init()`.
  - A slice may use only std, external crates, `crate::events`, `crate::eventstore`, `crate::eventhandler`, `crate::app` and its own sub-modules. A guard test enforces this, as `imports_test.go` does today.
- **Shutdown order** as in Go:
  1. stop accepting and drain HTTP (forced after the shutdown timeout);
  2. cancel the automations' token;
  3. stop the event handlers;
  4. close the store.
  
  Every step runs; the first error is returned.
- **Rust practice:**
  - `cargo fmt`, and `cargo clippy --all-targets -- -D warnings` clean;
  - `#![forbid(unsafe_code)]` unless a sub-task justifies a narrow exception;
  - typed errors, no `unwrap` outside tests;
  - small modules with doc comments in the codebase's plain style;
  - tests next to code (`#[cfg(test)]`) plus integration tests in `tests/`;
  - given/when/then tests for slice logic, as in task 010.
- **Verification** for every sub-task: `cargo fmt --check && cargo clippy --all-targets -- -D warnings && cargo test`, plus the Go check (`go vet ./... && go test ./...`) until the Go code is deleted.
- **Toolchain.** Rust is installed via Homebrew (no rustup), so extra targets can't be added here. Cross-compiling linux-arm64 is handled at cutover.
- **Commits.** One squash commit per sub-task on main, as usual.
- **Accepted deviations** (from 011.01):
  - The JSON log envelope is tracing's: `timestamp`, `message` and `level` instead of slog's `time`, `msg` and `level`. Message strings and field names are unchanged. *To be confirmed by the user.*
  - No single-dash flags (`-addr`); only `--addr` and friends.
  - A bare `:port` binds IPv4 only (`0.0.0.0:port`), not dual-stack.
  - The CLI flags' durations (`--shutdown-timeout`) are parsed by humantime (`10s`, `30m`, `1h`, `1m30s`), so Go's fractional `1.5h` isn't accepted there. Slice env durations (011.03's `INVESTIGATION_TIMEOUT` and `INVESTIGATION_MAX_AGE`) use a port of Go's `time.ParseDuration` and take Go's format exactly.

## Tasks

| #   | Task                                   | File                             | Status |
| --- | -------------------------------------- | -------------------------------- | ------ |
| 01  | Foundation: store, runner, app, wiring | 011.01-rust-foundation.md        | done |
| 02  | Record Sentry issue alert slice        | 011.02-rust-record-sentry-alert.md | done |
| 03  | Investigate Sentry issue slice         | 011.03-rust-investigate-sentry-issue.md | done |
| 04  | Cutover: e2e, build, docs, delete Go   | 011.04-rust-cutover.md           | new    |

> Superseded by task 012 (rewrite in Go with mainstream frameworks).
