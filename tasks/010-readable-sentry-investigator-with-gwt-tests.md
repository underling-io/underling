---
title: Readable Sentry Investigator with Given/When/Then Tests
date: 2026-10-25
status: done
type: tech
---

## Readable Sentry Investigator with Given/When/Then Tests

The `investigatesentryissue` slice is hard for humans to follow: business decisions are mixed with git, sandbox, process and HTTP mechanics, and its tests drive a re-exec'd fake `claude` and real git. Restructure it so the business flow reads top-down and is covered by Dilger-style given/when/then unit tests, e.g. given `SentryIssueAlertReceived`, when a PR can be opened, then `SentryIssueInvestigationPullRequestOpened` is recorded.

### Acceptance Criteria

- **Readable flow.** The slice package's production code holds the business flow only: which events an alert leads to (skipped stale or unmapped, duplicate ignored, started, then PR opened, no changes or failed with its stage). A reader sees the whole decision path in one place without git commands, sandbox settings, process handling or HTTP code.
- **Infrastructure behind interfaces.** Cloning and pushing, running Claude, and opening the PR sit behind small interfaces the flow depends on. Their implementations live in sub-package(s) of the slice.
- **Given/when/then tests.** A small helper lets each test read as: given prior events (optional), when an alert arrives and the infrastructure behaves a certain way, then exactly these events are on the issue's stream. They run in memory: no child processes, git or network. There's one test per outcome:
  - PR opened
  - no changes
  - skipped stale
  - skipped unmapped
  - alert for an issue already handled → no new events
  - failed for each stage: clone, claude, timeout, push (including CI config changes), pr
  - failed shutdown while running
  - alert picked up during shutdown → no events
- **Infrastructure keeps its tests.** The safety properties stay tested where they're implemented: sandbox settings, child env, token never on disk, git timeouts, stray branches, CI paths, PR title and body caps, work dir checks. Fake-`claude` end-to-end tests shrink to a small set (happy path to a draft PR, child env holding no heinzel secrets).
- **No behaviour change.** Event types and payloads, stream names, handler name, config variables, log lines and the e2e test stay as they are.

### Notes

- The user chose one slice with the logic split out, not splitting the flow into several Dilger slices and not adding tests only.
- The import guard already allows a slice to import its own sub-packages; sub-packages must not be picked up as slices by the generator or the slice-dir tests.
- Codex is unavailable in this environment (user said skip earlier); reviews run Claude-only.

## Implementation Plan

### TLDR

Keep the decisions (skip, dedupe, start, outcome and failure stage) in the slice package, behind two interfaces: one investigation run per alert, and a PR opener. Move git, claude/sandbox/process and the GitHub client into sub-packages. Drive the flow with in-memory given/when/then tests, and move the infrastructure tests next to the code they test.

### Steps

1. **Flow boundary.** The slice package defines what the flow needs, and nothing about paths, argv, env or HTTP:
   - An opener that, given ctx, target repo and branch, clones and returns a run, or an error (the clone stage).
   - The run, which can:
     - run Claude with a prompt, returning its summary, or an error the flow can tell apart as timeout vs other (the claude stage);
     - list stray branches;
     - count new commits;
     - list changed paths;
     - push;
     - report the default branch;
     - remove itself.
   - A PR opener: target repo plus title, head, base and body in, URL or error out.
   - Shutdown classification stays in the flow, as today: if ctx is done, any failure is stage shutdown.
   - The real run composes the workspace and claude sub-packages. That wiring lives in the slice package, next to `New`, not in the flow file.
2. **Flow file.** `Handle` plus the investigate sequence read top-down as today's decision path:
   - stale, then unmapped, then shutdown before start, then Started at v0;
   - then clone, Claude, stray, commits, CI paths, push and PR, with the outcome at v1.
   
   Keep the outcome/record/append helpers, stream naming and the CI-path rule (it's a business rule) in the slice package. PR title and body text stay in the slice package in their own file, since the flow decides PR content. The truncation and fence helpers go with them.
3. **Sub-packages** under `internal/slices/investigatesentryissue/`, one each for:
   - git workspace: mirror, clone, branch, stray/commits/paths, push, remove, work-dir checks, git runner;
   - claude: command, sandbox settings, prompt-free argv/env, stream reading, init check, process run/kill, PATH/deny-path resolution helpers as they fit;
   - GitHub draft-PR client.

   Rules:
   - Sub-packages must not import the parent (that would be a cycle). The parent's `loadConfig` stays the single config parser and passes plain values or options structs down.
   - Export only what the parent uses.
   - The `_unix`/`_other` files go with their code. Keep `GOOS=linux GOARCH=arm64 go build ./...` and `GOOS=windows go vet ./...` green.
4. **GWT helper and tests** (package-internal test file in the slice package):
   - The helper reads as `given(events...)`, `when(alert)` with scripted fakes for opener, run and PR opener, then `then(events...)`.
   - It asserts exactly those events, types and decoded payloads, on the issue's stream.
   - It uses a real `eventstore` in `t.TempDir()`, with no processes, git or network.
   - Stale comes from `Now`. "Shutdown while running" is a fake Claude that cancels ctx and returns an error. "Picked up during shutdown" is an already-cancelled ctx.
   - Cover every outcome listed in the ACs. Where it's the point of the rule, also assert the fake saw no clone (skips, duplicate), no push (CI config) or no PR (no changes).
5. **Prune and relocate the existing tests.**
   - Delete fake-claude tests in `investigator_test.go` that the GWT tests now cover.
   - Keep two end-to-end tests through `New` with real git and the re-exec'd fake claude: the happy path to a draft PR, and the child env holding no heinzel secrets. The restart-dedupe and work-dir tests through `New` may stay if cheap.
   - Move these to the sub-packages as direct tests of that code: stray-branch semantics (other branch, renamed, deleted, merged helper), the child's git config not affecting the push, timeout and shutdown kill, session-init failure, grandchild and git pipe waits.
   - The re-exec fake-claude helper may be duplicated where both packages need it.
6. **Docs.** Update the package doc comment and the investigate section of `docs/event-model.md` where it names files or layout.

### Notes

- **No behaviour change means byte-for-byte.** That covers event payloads, failure reason strings, the log messages and attrs ("sentry issue investigation finished", "sentry issue already handled", the PATH warning), the handler name and `New(Params)`. The e2e test in `internal/slices` must pass untouched.
- Sub-packages are not slices. The generator and slice-dir rule only look at top-level dirs. Check `go generate ./...` leaves `slices_gen.go` unchanged.

## Implementation Notes

- **Layout.** `investigator.go` is the flow: `Handle`, `investigate` and the interfaces they need (`cloner`, `run`, `prOpener`, `pullRequest`, `timedOut`). `infrastructure.go` implements them with the sub-packages `workspace` (git), `claude` (sandboxed child) and `github` (draft-PR client). Recording helpers and `startedEvent` moved to `outcome.go`. PR text is in `pr.go`, the prompt in `prompt.go`, and the CI-path rule with `firstN` in `ciconfig.go`.
- **Timeout signal.** The flow spots a Claude timeout through an error with a `Timeout() bool` method (the net/os convention), checked by `timedOut(err)`. `claude.TimeoutError` provides it, so the flow file imports none of the sub-packages. Only `run.claude` errors go through that check, so git errors that wrap `context.DeadlineExceeded` can't be mistaken for a Claude timeout.
- **`clone` takes the issue ID.** It's only there so the real `remove()` can log "remove investigation workspace" with the same `issueId` and `dir` attributes as before. `remove()` logs its own failures and returns nothing, which keeps the flow's `defer run.remove()` to one line.
- **Path helpers live in `claude`.** `ResolvePath`, `Within`, `FirstContaining`, `LookPath` and `WithResolved` are exported from `claude`, because both its sandbox settings and the parent's `resolveReadPaths`/`deniedPathDirs` use them. Those two stay in `config.go`: their errors name the env vars, and the sub-package can't import the parent's constants. `claude.Command` takes `*workspace.Workspace` directly. The sibling import is allowed and avoids copying the layout into a second type.
- **One race-only difference.** Before, a failed Claude run was recorded with the stage `runClaude` chose. Now it goes through `fail()`, which switches to stage `shutdown` whenever ctx is done. The two differ only if shutdown begins between the child exiting and the stage check. They also differ for a `create pipe` or `start claude` error raised with ctx already done, which needs the clone to have succeeded under a cancelled ctx. The reason strings are unchanged.
- **The `then` step checks the events the alert added,** not the whole stream. With no given events these are the same; with given events, `then()` means "no new events" (Dilger style).
- **Tests deleted from the old `investigator_test.go`** (now `e2e_test.go`), because they're now covered by GWT and/or sub-package tests: unmapped, stale (fixed `max age`), no commits, Claude failure, session-init modes, other/renamed/deleted/helper branch, timeout, shutdown kill, picked up during shutdown, repeated alerts, repeated skipped alerts, clone/push/PR failure, hostile git config, CI config, other events, undecodable alert, Name. **Kept end to end** (real git plus the fake claude): happy path, child env, restart dedupe, the two clock-based stale tests (the "handled late" one now uses an unmapped project, so Claude doesn't run), the PATH warning, invalid config, and both work-dir tests in `workdir_unix_ext_test.go`. The parent's fake claude now has just `happy` and `noop`. The claude package has its own simpler re-exec fake, driven by an env var.
- **Moved:** the claude command/settings, stream and init tests went to `claude/`. The git, workspace, remove-tree and work-dir tests went to `workspace/`, and git pipe waits to `workspace/gitwait_unix_test.go`. Grandchild went to `claude/grandchild_unix_test.go`. The prompt tests went to `prompt_test.go` and the CI-path test to `ciconfig_test.go`.
- **New tests:** a `github` client test (headers, draft body, non-201, missing URL), `workspace` tests for clone failure cleanup, siblings, rename-as-delete+add in `ChangedPaths`, and token redaction in git errors, and `claude` tests for a start failure and stdin delivery.
- **Docs:** `docs/event-model.md` now names the investigator's file layout, and the "Adding a slice" import rule mentions sub-packages. The import guard already allowed them.

### Review fixes

- **Real timeout → stage `timeout`.** `infrastructure_test.go` checks that `timedOut` holds for a wrapped `*claude.TimeoutError` and not for a plain claude failure. The claude package's timeout test also checks that the error has a `Timeout()` method that reports true. Mutation: making `TimeoutError.Timeout` return false fails both.
- **`then()` checks every stream.** It now compares all the events the store gained after `given`, so an alert that records on any other stream fails the scenario. Before, `TestAnUndecodableAlertIsLoggedAndSkipped` was vacuous. Mutation: without the `return` after "decode sentry issue alert", the zero-value alert records `skipped unmapped` on the empty issue's stream. The new `then()` catches it; the old one passed.
- **Git listing errors.** The fake can fail `strayBranches`, `newCommits` and `changedPaths`. New scenarios: a branch-listing or commit-count error fails stage `claude` with "collect claude's commits: <err>", and a changed-paths error fails stage `push` with the error text and pushes nothing. A further scenario checks the order of the checks: an alert that is both stale and for an unmapped project is skipped as stale.
- `TestClaudeRunningOutOfTimeFailsTheInvestigation` also checks that the workspace is removed.
- **Rejected:** the race where a claude failure during shutdown is recorded as stage `shutdown`. The window is microseconds, the reason string is unchanged, and `shutdown` is arguably the more correct stage.
- **Follow-ups** (both gaps pre-date this task): no test that push runs with hooks off (plant a pre-push hook in the mirror), and no test that cancelling in hang mode kills the process group.
