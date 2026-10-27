---
title: System Test Against a Real Bug Fix with Real Claude
date: 2026-10-27
status: in-dev
type: feat
---

## System Test Against a Real Bug Fix with Real Claude

Add a local system test that replays a real upstream bug: it posts a Sentry alert to heinzel's webhook, lets the real local Claude Code investigate and fix the bug in a clone of the buggy repo, fakes GitHub, and then checks with upstream's own regression test that the bug is fixed.

### Acceptance Criteria

- **Target repo.** hashicorp/go-version, rolled back to the parent of c6a3b57 ("Fix Core panicking on a zero-value Version"). At start, the test fetches go-version from github.com and serves the parent commit from a local bare repo as the clone remote, without the fix commit or anything after it.
- **Wiring.** heinzel runs through `app.Run` with the real record slice. The investigate slice is built with `investigatesentryissue.New`, with its GitHub API and clone URLs pointed at the fakes. Production code gets no URL-override hook.
- **Sentry input.** The test posts an alert to `POST /sentry/callback` that describes the real panic: an index-out-of-range runtime error in `Version.Core` in `version.go`.
- **Precondition.** Before investigating, the test takes a scratch clone of the served parent, swaps in upstream's `version_test.go` from c6a3b57, and requires `TestCoreZeroValue` to run and fail. "No tests to run" doesn't count as a failure.
- **Real Claude.** heinzel runs the locally installed `claude` binary with the locally installed Codex and auto-task plugins: the newest version dir of each in `~/.claude/plugins/cache`. The test copies them to a temp dir outside HOME, because heinzel rejects plugin dirs under HOME. The test passes the real `HOME` and `PATH` to heinzel, and fails early if `go`, `node` or `codex` resolve under HOME, since the sandboxed child can't read them there.
- **Fake GitHub.** The push goes to the local bare remote. The PR call goes to a fake GitHub that records it. `GITHUB_TOKEN` is a dummy value.
- **Verdict.** The investigation ends in PullRequestOpened. The test then makes a fresh clone of the pushed branch from the bare remote and replaces `version_test.go` with upstream's copy from c6a3b57. In that clone, `TestCoreZeroValue` and go-version's full `go test ./...` must pass.
- **Comparison.** The test prints Claude's diff of `version.go` next to upstream's fix. It doesn't fail on differences.
- **Subscription auth.** heinzel accepts `CLAUDE_CODE_OAUTH_TOKEN` as an alternative to `ANTHROPIC_API_KEY`, and only the one that is set is passed to the child. With the OAuth token, the child runs without `--bare`, because `--bare` never reads OAuth. With the API key, it keeps `--bare`. In OAuth mode, the child must still ignore ambient config: no MCP servers from the clone (e.g. `--strict-mcp-config`), and no CLAUDE.md or plugins beyond the given plugin dirs, as far as Claude Code's flags and env allow. docs/event-model.md states exactly what each mode does and doesn't block. Setting both, or neither, is a config error that names both variables. The config, command and e2e tests cover both modes.
- **Opt-in.** Behind a `system` build tag, run by `make system-test` with `INVESTIGATION_TIMEOUT=30m` and `go test -timeout 45m`. If the Claude credential or `OPENAI_API_KEY` is missing, it fails with a message naming the variable.

### Notes

- First, check by hand that `TestCoreZeroValue` panics on the parent of c6a3b57 and passes on c6a3b57. This wasn't run while the task was being written. If the check fails, stop and report rather than picking another commit.
- Dropping `--bare` in OAuth mode was the user's call. Isolation still comes from the throwaway HOME, the child's own config dir and the sandbox.
- The OAuth token comes from `claude setup-token`. The child's throwaway HOME can't see the local login.

## Implementation Plan

### TLDR

Add OAuth support to heinzel's config and claude command (no `--bare`, explicit isolation flags), then add a `system`-tagged test package that mirrors go-version at c6a3b57's parent into a local bare remote, wires the record slice and `investigatesentryissue.New` (fake GitHub, local clone base) into `app.Run`, posts a signed alert, waits for the outcome, and checks the pushed branch with upstream's test file.

### Steps

1. **Pre-check (gate).** In a scratch clone of hashicorp/go-version, confirm `go test -run TestCoreZeroValue` panics on `c6a3b57^` with c6a3b57's `version_test.go` swapped in, and passes on c6a3b57. If not, stop and report.
2. **Credential config.** `loadConfig` reads `CLAUDE_CODE_OAUTH_TOKEN` too; exactly one of it and `ANTHROPIC_API_KEY` must be set — both or neither is one error naming both variables (joined with the other config errors, as today). The config carries a credential kind + value so the child gets only that variable. `config_test` covers API-key-only, token-only, both, neither.
3. **Child command per mode** (`claude.Config` gains the credential; `Command` branches):
   - API key: argv and env unchanged.
   - OAuth: argv drops `--bare` and adds `--strict-mcp-config`; everything else (`--setting-sources ""`, `--settings`, `--plugin-dir`, …) stays. Env carries `CLAUDE_CODE_OAUTH_TOKEN` instead of `ANTHROPIC_API_KEY`.
   - Try `--safe-mode` in OAuth mode to also block the clone's CLAUDE.md. Keep it only if a real run still loads the `--plugin-dir` plugins (`checkInit` passes) and their skills are usable; otherwise leave it out and document that the clone's CLAUDE.md is read in OAuth mode.
   - `command_test` asserts argv and env for both modes; the e2e env assertion covers both.
4. **Fake claude.** `TestMain` in `fakeclaude_test.go` detects the fake by `os.Args[1] == "--bare"`; make detection mode-independent and add one OAuth-mode e2e case.
5. **Docs.** docs/event-model.md: invocation block and "No ambient config" describe both modes, stating exactly what OAuth mode still blocks and what it doesn't. Env var table gains `CLAUDE_CODE_OAUTH_TOKEN`.
6. **System test package.** New package outside `internal/slices` (so the slice import guard doesn't apply), every file `//go:build system`.
   - Fail fast with a named message: Claude credential (either variable) and `OPENAI_API_KEY` set; `go`, `node`, `codex`, `claude` on PATH; `go`, `node`, `codex` not resolving under HOME (symlinks resolved). `claude` may live under HOME — heinzel already re-opens its dir.
   - Plugins: newest version dir of `openai-codex/codex` and `auto-task/at` under `~/.claude/plugins/cache`, copied into a `t.TempDir()` (outside HOME on macOS); `CLAUDE_PLUGIN_DIRS` points at the copies.
   - Remote: `git clone --bare` github.com/hashicorp/go-version into `<base>/hashicorp/go-version.git`, force the default branch to `c6a3b57^`, delete every other ref and tag so the fix isn't reachable, point HEAD at that branch.
   - Precondition: scratch clone of that remote, swap in c6a3b57's `version_test.go`, run `go test -run '^TestCoreZeroValue$' -v`; assert the output shows the test ran and failed, not just a non-zero exit.
7. **Wiring and run.**
   - Hand-built `app.Slices`: the record slice via `recordsentryissuealert.New`, and `investigatesentryissue.New` with `GitHubCloneURL` = `<base>/`, a fake `GitHubAPIURL`, and an `HTTPClient` whose transport records requests and answers 201 with a PR URL.
   - getenv supplies real `HOME`/`PATH`, `SENTRY_GITHUB_REPOS=<id>=hashicorp/go-version`, dummy `GITHUB_TOKEN` and `SENTRY_CLIENT_SECRET`, `INVESTIGATION_TIMEOUT=30m`, `INVESTIGATION_WORK_DIR` = a 0700 temp dir.
   - Serve with `app.Run` on an in-memory listener (copy the pipe listener from `sentryalert_e2e_test.go`; `_test` files can't be imported).
   - Post a signed `event_alert`: exception `runtime.Error` / "index out of range [0] with length 0", culprit `github.com/hashicorp/go-version.(*Version).Core`, frames ending in `version.go` `(*Version).Core`.
   - Wait on the investigation stream until the outcome event (deadline just under 45m). Anything other than PullRequestOpened fails with the event's stage and reason. Assert exactly one draft PR request for hashicorp/go-version.
8. **Verdict and comparison.** Fresh-clone the pushed `heinzel/sentry-<issueId>` branch from the bare remote, overwrite `version_test.go` with c6a3b57's (Claude's own test edits are dropped on purpose), run `go test -run '^TestCoreZeroValue$'` then `go test ./...`; both must pass. `t.Log` `git diff c6a3b57^ <branch> -- version.go` next to `git show c6a3b57 -- version.go`, no assertion.
9. **Make target.** `system-test`: `go test -tags system -timeout 45m -v -count=1` on the new package. `check` unchanged.

### Notes

- github.com is only fetched by the test's setup; heinzel never sees a github.com URL.
- `-count=1` and the build tag are load-bearing: a cached pass hides a real failure, and an untagged file would make `go test ./...` start a paid run.
- With OAuth and no `--bare`, the fresh `CLAUDE_CONFIG_DIR` is what keeps installed plugins and user settings out, which is why the plugin copies must arrive via `--plugin-dir`.
- The Go toolchain can't be downloaded inside the sandbox; go-version's `go.mod` says `go 1.16`, so it's fine today.

## Implementation Notes

- **Pre-check passed.** `TestCoreZeroValue` panics with `index out of range [0] with length 0` on c6a3b57's parent and passes on c6a3b57.
- **Not yet run for real.** No Claude or OpenAI credentials were available here. A dry run with dummy credentials covered the clone, the mirror, the precondition, the wiring, the plugin copies and the OAuth launch. It ended where it should: Claude got `401 Invalid bearer token`. The real run still needs `CLAUDE_CODE_OAUTH_TOKEN=… OPENAI_API_KEY=… make system-test`.
- **No `--safe-mode`.** The local experiment to see whether `--plugin-dir` plugins still load under it was blocked by a permission prompt. So the plan's fallback was taken: OAuth mode uses `--strict-mcp-config` only, and docs/event-model.md says the clone's CLAUDE.md is read in that mode. Follow-up: try `--safe-mode` in a real OAuth run and keep it if `checkInit` passes.
- **Deviation: waiting on the log, not the stream.** The test waits for the "sentry issue investigation finished" log record, not the event stream: `app.Run` owns the bbolt store, which can't be opened from outside while it runs. The record carries the same outcome, stage, reason and PR URL.
- **Fake claude.** It now recognises itself by `-p` as the second argument, which both modes pass, instead of by `--bare`.
- **Under HOME.** heinzel warns that `~/.local/bin` (where `claude` lives) is under HOME. That's fine: heinzel re-opens the claude binary's dir, and the test fails only if `go`, `node` or `codex` resolve under HOME.
- **Sandbox needs.** Running `make check` inside a socket-binding sandbox fails `recordsentryissuealert` (httptest binds a port). This is pre-existing. The system test needs github.com access for its setup clone.
