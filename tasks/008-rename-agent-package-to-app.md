---
title: Rename Agent Package to App
date: 2026-10-25
status: ready-for-dev
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
