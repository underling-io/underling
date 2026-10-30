---
title: Run the System Test Through heinzel's Main Entry Point
date: 2026-10-28
status: done
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

## Implementation Plan

### TLDR

Fold the two GitHub bases into the investigate slice's env config (`loadConfig`) and drop them from `Params`. Then rewrite the system test's harness: build `./cmd/heinzel`, run it with an explicit env, discover its address from stderr JSON, fake GitHub with `httptest`, stop it with SIGTERM. The rest of 013's test body stays as is.

### Steps

1. **Config.** Parse `GITHUB_API_URL` and `GITHUB_CLONE_URL` in `loadConfig` (internal/slices/investigatesentryissue/config.go) through the existing `check(variable, err)` so errors join the others and name the variable. Empty → default (`https://api.github.com`, `https://github.com/`), like `parseDuration` treats `""`; don't trim.
   - API URL: `url.Parse` succeeds, scheme `http`/`https`, non-empty host. A trailing slash is fine (`github.Client` trims it).
   - Clone base: only rule is "ends in `/`". Don't require a URL — e2e_test.go and the system test use local directory paths.
2. **Drop `Params.GitHubAPIURL` / `Params.GitHubCloneURL`.** `New` wires the cloner base and `github.Client.APIURL` from the config; the registered factory loses its hard-coded URLs. Keep `HTTPClient` and `Now` in `Params`. Update internal/slices/investigatesentryissue/e2e_test.go to pass `GITHUB_API_URL=apiBase` and `GITHUB_CLONE_URL=<remotes>/` via its env map — its existing PR-path assertion then proves the override end to end.
3. **Config tests.** `TestLoadConfigDefaults` (whole-struct compare) gains both defaults. `TestLoadConfigReadsEveryVariable` sets both (include a local-path clone base like `/srv/remotes/`). The rejects table gains API URL `api.github.com`, `ftp://h`, `https://`, `/relative` and clone base `https://github.com`.
4. **Docs.** Two optional rows in docs/event-model.md's env var table with defaults and the format rules above; the clone row says repos are cloned (and pushed) at `<base><owner>/<repo>.git` and that the base may be a local dir ending in `/`.
5. **Build the binary** first in the system test (before the upstream clone, so a broken build fails fast): `go build -o <t.TempDir()>/heinzel ./cmd/heinzel`, `Dir` = module root, env = the test's own `os.Environ()` (needs GOCACHE etc.). `t.Fatalf` with combined output on failure.
6. **Fake GitHub** is an `httptest.NewServer`: handler records `METHOD /path` under a mutex and answers 201 with the same PR JSON. Existing assertion (exactly one `POST /repos/hashicorp/go-version/pulls`) stays.
7. **Start heinzel:** `exec.Command(bin, "-addr", "127.0.0.1:0", "-data-dir", <temp>)`. `cmd.Env` is exactly 013's map as `K=V` plus `GITHUB_API_URL=<server.URL>` and `GITHUB_CLONE_URL=<remotes>/` — non-nil, nothing from `os.Environ()`.
   - `cmd.Stderr` is an `io.Writer` (not `StderrPipe`, so `Wait` waits for the copy) that splits into lines, tees every byte to `os.Stderr`, decodes JSON lines into the record list and skips non-JSON lines (panic traces, usage). It must never return an error, or exec stops draining and heinzel blocks. The current log sink assumes one record per `Write`; that no longer holds.
   - Set `WaitDelay`. After `Start`, one goroutine runs `Wait`, stores the error, closes an "exited" channel.
8. **Waiting.** The record waiter selects on new records, its deadline and "exited"; on exit it fails immediately with the exit error. First wait for `"app started"` (short deadline, ~1 min) and take `addr`; `postAlert` posts to `http://<addr>/sentry/callback` with a plain `http.Client` with a timeout (body, headers, HMAC unchanged). Then wait for "sentry issue investigation finished" with the existing deadline and `issueId` match.
9. **Shutdown cleanup.** Register it after the data dir, work dir and fake server exist (cleanups run LIFO, so heinzel stops before they are torn down). If still running, SIGTERM; wait on "exited" bounded at ~30s (> the 10s shutdown timeout), else Kill and fail. `t.Error` unless the exit status is 0, including an early exit.
10. **Remove the dead in-process harness**: pipe listener/addr, in-process start, and the `app`, `investigatesentryissue`, `recordsentryissuealert` imports. Check: `go vet -tags system ./internal/systemtest` is clean and `grep -n 'heinzel/internal' internal/systemtest/*.go` prints nothing. Update comments that describe in-process startup; doc.go in this worktree already names `make system-test` — keep it, and leave the primary checkout's uncommitted edit alone.

### Notes

- The explicit env drops TMPDIR etc. That's safe: git and the claude child already get explicit envs (workspace/git.go, claude/command.go), and `INVESTIGATION_WORK_DIR` is set. Don't add vars "to be safe" — the AC forbids it.
- internal/slices/sentryalert_e2e_test.go builds via the real factory; project 1 is unmapped, so the new defaults never hit the network there. No change needed.
- Allowing `http` for the API URL means a misconfigured production would send `GITHUB_TOKEN` in cleartext; the AC allows http, so document only.
- The sandbox here denies local port binding, so `make check` fails in `internal/platform/app` and one `recordsentryissuealert` test at baseline (`bind: operation not permitted`). Environmental; use `GOCACHE=$TMPDIR/gocache`.

## Implementation Notes

- **Not run for real.** The system test was only vetted and compiled (`go vet -tags system`, `go test -tags system -run XXX`). It costs tokens to run, and this sandbox denies local port binding, which the binary and the fake GitHub server now both need. The new harness (build, address discovery, SIGTERM shutdown, early-exit detection) is still unexercised. It needs `CLAUDE_CODE_OAUTH_TOKEN=… OPENAI_API_KEY=… make system-test` on a machine that allows local binding.
- **Build order.** The binary is built after the cheap credential and tool checks but before the upstream clone. The plan only asked for "before the upstream clone".
- **Intermediate commit.** The commit that dropped `Params.GitHubAPIURL`/`GitHubCloneURL` left the `system`-tagged package uncompilable until the next commit rewrote it. `make check` doesn't build that tag, so it stayed green at every commit.
- **Log sink.** It splits stderr into lines, decodes only the lines that are JSON objects, and never returns an error. `waitFor` scans the records once more after the process exits, because `Wait` returns only after exec has copied all of stderr. So a record logged just before exit isn't missed.
- **`make check` here.** Same as baseline: `internal/platform/app` (5 tests) and `recordsentryissuealert`'s `TestOversizedBodyClosesTheConnection` fail with `bind: operation not permitted`. Every other package passes.
- Review follow-up (not fixed): heinzel now runs as a child process with no parent-death signal. If `go test` is SIGKILLed or panics on its timeout before cleanups run, heinzel is orphaned (holding its port and data dir). No portable fix on darwin (Linux could use `Pdeathsig`); `investigationDeadline` keeps the waits inside the test timeout, and Ctrl-C reaches heinzel through the foreground process group.
