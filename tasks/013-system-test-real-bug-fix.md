---
title: System Test Against a Real Bug Fix with Real Claude
date: 2026-10-27
status: ready-for-dev
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
