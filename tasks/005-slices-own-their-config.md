---
title: Slices Own Their Config
date: 2026-10-25
status: ready-for-dev
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
