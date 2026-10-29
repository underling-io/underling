---
title: Functional Test of the heinzel Binary
date: 2026-10-29
status: new
type: feat
---

## Functional Test of the heinzel Binary

Add a black-box functional test that builds the real heinzel binary and drives it only through its HTTP port, env vars, flags and the fakes it talks to.

### Acceptance Criteria

- **Isolated.** Its own package, `internal/functionaltest`, behind a `functional` build tag, run by `make functional-test`. `go test ./...`, `make check` and `make system-test` don't run it, and it doesn't run them.
- **Self-contained.** It needs only Go and git on the host. No network beyond loopback, no credentials, nothing read from the real HOME, no Docker. It cleans up every process and temp dir it started, also when it fails.
- **Black box.** It builds `./cmd/heinzel` and runs it as a subprocess with an explicit env (nothing inherited but `PATH`), `-addr 127.0.0.1:0`, a temp `-data-dir` and its own 0700 `INVESTIGATION_WORK_DIR`. It imports nothing from heinzel's `internal/` packages. It observes heinzel only through HTTP answers, the JSON log on stderr, the exit status and what the fakes recorded.
- **Fake GitHub.** One local HTTP server the test starts, used as both `GITHUB_API_URL` and `GITHUB_CLONE_URL`:
  - serves real git repos over smart HTTP (git's own http-backend);
  - answers `POST /repos/{owner}/{repo}/pulls` with 201 and an `html_url`;
  - requires `Authorization` with the test's dummy `GITHUB_TOKEN` on the push and the PR call, so the test proves the token reaches both.
- **Fake Claude.** A small Go program the test builds and passes as `CLAUDE_BIN`. It speaks Claude Code's stream-json well enough for heinzel and acts on the work clone according to a scenario marker in the alert title, and records each run it makes.
- **Fake Sentry.** The test plays Sentry: it posts `event_alert` webhooks signed with `SENTRY_CLIENT_SECRET`, shaped like Sentry's real payload.
- **Scenarios.** Each starts a fresh heinzel, fake GitHub and repo:
  - **PR opened.** Fake Claude commits a change. The branch `heinzel/sentry-<issueId>` lands in the fake remote with that commit; exactly one draft PR request names that head, the default branch as base, the issue id in the title and Claude's summary in the body; the log reports `pr_opened` with the fake's PR URL.
  - **No changes.** Fake Claude commits nothing. Nothing is pushed, no PR request, log reports `no_changes`.
  - **Claude fails.** Fake Claude exits non-zero. No push, no PR, log reports `failed` with stage `claude`.
  - **PR rejected.** Fake GitHub answers the PR call with 422. Log reports `failed` with stage `pr`.
  - **Bad signature.** A wrongly signed alert gets 401 and starts no investigation.
  - **Redelivery.** The same alert posted twice gets 200 both times and leads to one investigation and one PR request.
  - **Restart.** After a PR-opened run, heinzel is stopped and started again on the same data dir, and nothing is re-posted for that issue; it doesn't investigate the issue again.
  - **Bad config.** Without `GITHUB_TOKEN`, heinzel exits with status 2 and never logs "app started".
- **Proving a negative.** Bad signature, redelivery and restart each end by posting a valid alert for a different issue and waiting for its outcome. Only then does the test assert that the first issue got no further investigation: no new start in the log, no new fake-Claude run, no new PR request.
- **Shutdown.** Every run ends with SIGTERM, sent only after the scenario's outcome is logged, and must exit 0. A heinzel that exits early fails the scenario at once instead of hanging until the deadline.
- **Fast.** The whole suite finishes in under 2 minutes on a laptop.

### Notes

- Depends on task 014 (unmerged, ready-for-signoff): it adds the `GITHUB_API_URL` and `GITHUB_CLONE_URL` overrides this test needs. Branch from 014 or wait for it to land.
- The record and investigate slices already have in-process e2e tests (`internal/slices`); this test is the outside view of the built binary and shouldn't duplicate their edge cases.
