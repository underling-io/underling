---
title: Run the System Test Through heinzel's Main Entry Point
date: 2026-10-28
status: ready-for-dev
type: feat
---

## Run the System Test Through heinzel's Main Entry Point

Task 013's system test builds heinzel itself: it constructs the two Sentry slices by hand and calls `app.Run` in-process. That skips `cmd/heinzel`'s `main`, the flag parsing, the slice registry and `app.Build`, which is how heinzel actually starts. Change the test so it builds the real `cmd/heinzel` binary and runs it as a subprocess. Everything the test controls goes in through env vars and flags, just as in production.

### Acceptance Criteria

- **GitHub URL overrides.** The investigate slice's registered factory reads two optional env vars: `GITHUB_API_URL` (the REST API base, default `https://api.github.com`) and `GITHUB_CLONE_URL` (the clone base, default `https://github.com/`; a repo is cloned from `<base><owner>/<repo>.git`). An unset or empty var means the default. An invalid value is a config error naming the variable: the API URL must be an absolute http(s) URL, and the clone base must end in `/`. This deliberately reverses 013's "Production code gets no URL-override hook" (user's call, 2026-10-28). The config tests cover defaults, overrides and invalid values. docs/event-model.md's env var table lists both.
- **Real binary.** The system test runs `go build` on `./cmd/heinzel` into a temp dir and starts that binary. It passes `-addr 127.0.0.1:0` and `-data-dir <temp>`, and an explicit environment containing only the vars the test sets. That's the same set 013 used, plus `GITHUB_API_URL` and `GITHUB_CLONE_URL`. The test doesn't import `app`, `investigatesentryissue` or `recordsentryissuealert` any more.
- **Address discovery.** The test reads the binary's JSON log on stderr, takes the listen address from the "app started" record, and posts the signed alert to `http://<addr>/sentry/callback`. Stderr is also copied to the test's stderr.
- **Fake GitHub.** It's a real local HTTP server (`httptest`). `GITHUB_API_URL` points at it. It records requests and answers the PR call with 201 and a PR URL. `GITHUB_CLONE_URL` points at the local bare-remote base dir.
- **Outcome and shutdown.** The test waits for the "sentry issue investigation finished" log record, as before. On cleanup it sends SIGTERM, waits for the process to exit, and fails if the exit status isn't 0. If the process exits early, the test fails instead of hanging until its deadline.
- **Unchanged.** Precondition, verdict, comparison, plugin copying, tool and credential checks, the `system` build tag and `make system-test` all keep their 013 behaviour. `make check` stays green.

### Notes

- The in-memory pipe listener goes away: the binary binds a real loopback port. Running the system test therefore needs local port binding, just like `make check` already does.
- The primary checkout has an uncommitted edit to internal/systemtest/doc.go that dropped "make system-test" from the comment. It looks accidental. This task leaves the working-tree edit alone, but doc.go should still name `make system-test` once the task is done.
