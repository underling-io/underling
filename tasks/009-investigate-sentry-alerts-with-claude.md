---
title: Investigate Sentry Alerts with Claude and Open a Draft PR
date: 2026-10-25
status: done
type: feat
---

## Investigate Sentry Alerts with Claude and Open a Draft PR

Replace the placeholder Sentry alert handler. For each new Sentry issue, underling clones the mapped GitHub repo, runs headless Claude Code (with the Codex and auto-task plugins) in a locked-down child process to investigate and fix the issue, then pushes the result and opens a draft PR itself.

### Acceptance Criteria

- **Repo mapping.** Config maps a Sentry project id to a GitHub `owner/repo`. An alert for an unmapped project is logged and skipped, with no clone, no Claude run and no PR.
- **Issue details.** `SentryIssueAlertReceived` gains optional fields holding the exception details Sentry sends: culprit, exception type and value, and stack frames (file, function, line). They're filled for new alerts. Older stored events without them still decode and are handled.
- **One investigation per Sentry issue.** Repeated alerts for the same issue id (redeliveries, or the rule firing again) start at most one investigation, even across restarts.
- **Isolated Claude run.**
  - underling starts the `claude` CLI headlessly as a child process, with its working directory set to a fresh clone of the target repo on a new branch.
  - The child's environment holds only what it needs: `PATH`, a temporary `HOME`, its own Claude Code config dir with just the Codex and auto-task plugins, the Anthropic credential and the OpenAI credential for Codex. It never sees the GitHub token, the Sentry client secret or any other underling environment variable.
  - Claude Code's sandbox is on, and file writes are limited to the clone.
  - The prompt carries the issue details and asks Claude to investigate and fix the issue with auto-task, committing on the branch without pushing, merging or opening a PR.
  - The run has a configurable timeout, after which the child is killed.
- **Draft PR by underling.**
  - If the run succeeds and the branch has new commits, underling pushes the branch with its own GitHub token and opens a **draft** PR against the default branch.
  - The PR title names the Sentry issue. The body links the Sentry issue and includes Claude's final summary.
  - With no new commits there's no PR.
- **Outcome recorded.** Each investigation ends in an event: PR opened (with the PR URL), no changes, skipped (unmapped project), or failed (clone, Claude, push or PR error, or timeout, with the reason). A failure never stops the handler loop or later alerts.
- **Testable without the outside world.** Tests use a fake `claude` executable and a local bare git repo as the remote, and fake GitHub over an in-memory HTTP transport. They cover: the happy path to a draft PR, the child environment containing none of underling's secrets, unmapped project, no commits, Claude failure or timeout, and dedupe per issue.

### Locked Decisions

These resolve the gaps a cold read found. The ones marked (judgment) are calls to surface to the user at the end.

1. **Automations get the store.**
   - `app.Automation` becomes a struct `{ Handler func(ctx context.Context, store *eventstore.Store) eventhandler.Handler }`, mirroring `StateChange`. `app.Run` builds each one after opening the store.
   - `ctx` is cancelled when shutdown begins, right before the handler runner is stopped.
   - `eventhandler` doesn't change. Docs and the existing registration code are updated to match.
2. **Event contracts** go in `internal/events`. They're persisted, so their shapes are locked:
   - `SentryIssueInvestigationStarted{issueId, project, repo, branch}`
   - `SentryIssueInvestigationPullRequestOpened{issueId, prUrl}`
   - `SentryIssueInvestigationNoChanges{issueId}`
   - `SentryIssueInvestigationSkipped{issueId, reason}`, where the reason is `unmapped` or `stale`
   - `SentryIssueInvestigationFailed{issueId, stage, reason}`, where the stage is `clone`, `claude`, `timeout`, `push`, `pr` or `shutdown`

   All of them go on stream `sentry-issue-investigation-<issueId>`. The first event (Started, or Skipped) is appended at expected version 0, so a version conflict means the issue was already handled and gets ignored. The outcome follows at version 1. The issue id goes into the stream name as-is if it's `[A-Za-z0-9_-]+`, and otherwise as its hex sha256.
3. **(Judgment) No replay of history.**
   - A new slice `internal/slices/investigatesentryissue/` with handler name `sentry-issue-investigator` replaces `handlesentryissuealert`, which is deleted along with its log placeholder.
   - An alert whose stored event time is older than `INVESTIGATION_MAX_AGE` (default `1h`) is recorded as Skipped `stale`, with no clone and no run. So the first deploy doesn't investigate past alerts.
4. **(Judgment) Shutdown kills the run.** When the ctx from decision 1 is cancelled, the `claude` child is killed (`exec.CommandContext`, or an explicit kill) and Failed `shutdown` is recorded. `app.Run` still returns promptly.
5. **Config.** The slice reads these from `Getenv`. Any invalid value means "invalid configuration" and exit 2.

   | Variable | Required | Format | Default |
   | --- | --- | --- | --- |
   | `SENTRY_GITHUB_REPOS` | yes | `<projectId>=<owner>/<repo>`, comma-separated, ids are ints | |
   | `GITHUB_TOKEN` | yes | | |
   | `ANTHROPIC_API_KEY` | yes | passed to the child | |
   | `OPENAI_API_KEY` | yes | passed to the child, for Codex | |
   | `CLAUDE_PLUGIN_DIRS` | yes | comma-separated plugin dirs (the Codex and auto-task plugins), each passed as `--plugin-dir` | |
   | `INVESTIGATION_TIMEOUT` | no | Go duration | `30m` |
   | `INVESTIGATION_MAX_AGE` | no | Go duration | `1h` |
   | `CLAUDE_BIN` | no | | `claude` |
   | `INVESTIGATION_WORK_DIR` | no | | OS temp dir |

   The GitHub API base URL (`https://api.github.com`) and the clone URL base (`https://github.com/`) are `New` parameters, with `init()` passing those defaults. Tests pass a local bare repo path and an in-memory HTTP client. The e2e test's env gets the new required variables.
6. **Child process.**
   - Run `claude --bare -p --output-format stream-json --no-session-persistence --setting-sources "" --settings <generated JSON> --permission-mode <non-interactive mode that allows edits within the sandbox> --plugin-dir <each> <prompt>`. Check every flag against the installed `claude --help`.
   - The generated settings turn the sandbox on, with file writes allowed only in the clone.
   - The environment is exactly `PATH`, `HOME=<temp dir>`, `CLAUDE_CONFIG_DIR=<temp dir>`, `ANTHROPIC_API_KEY` and `OPENAI_API_KEY`.
   - The host needs `git`, `claude` and `codex`; document this in `docs/event-model.md` or a README section.
7. **Exception fields** on `SentryIssueAlertReceived`, all `omitempty`:
   - `culprit`
   - `exceptionType` and `exceptionValue`, from the last (outermost) entry of `data.event.exception.values`
   - `frames: [{file, function, line}]`, from that entry's `stacktrace.frames`, in Sentry's order, keeping at most the last 50

   Update the payload example in `docs/event-model.md`.
8. **(Judgment) Failed is final.** There's no retry. One attempt per issue, consistent with the crash note below.
9. **Test levels.** Slice tests call `New` directly with a fake `claude` script, a local bare remote and an in-memory GitHub, and cover every outcome. The e2e test, through `app.Build`, posts an alert for an unmapped project and waits for the skipped outcome, so it needs no network.
10. **Success and summary.** A run succeeds when the child exits 0 and its last stream-json event is `{"type":"result","is_error":false,...}`. The PR body uses that event's `result` text as Claude's summary. The fake `claude` emits this shape.
11. **Sandbox evidence.** The fake `claude` asserts the generated settings enable the sandbox and limit writes to the clone. It also writes the environment it received to a file, which the test checks for absent secrets (`GITHUB_TOKEN`, `SENTRY_CLIENT_SECRET`, and any other variable underling's own environment has).
12. **Stdlib only.** Git runs through `os/exec`, and GitHub through `net/http` against the REST API, inside the slice. No new modules and no shared helper package, per the import guard.

### Notes

- Decisions by the user:
  - GitHub, with each Sentry project mapped to a repo.
  - underling, not Claude, holds the write token, pushes and opens the PR.
  - The `claude` CLI is driven from Go: the Agent SDK has no Go version, and the plain Go API SDK can't run Claude Code plugins.
  - One task, not an epic.
- The Codex plugin needs OpenAI credentials inside Claude's environment, so Claude can read them. That's accepted, and it's best to use a dedicated, low-limit key.
- The alert carries a Sentry project **id**, not a slug (`data.event.project`), so the mapping is keyed by id.
- New config follows the existing pattern: slices load their own config from `Getenv` in their constructor, and invalid config fails startup with "invalid configuration".
- Check the exact `claude` CLI flags (headless mode, stream-json output, settings and plugin dirs, permission mode, sandbox) against the installed Claude Code version and the docs. Don't rely on memory.
- A long run blocks only this handler's loop, and runs go one at a time, which is fine for now. If underling crashes mid-run, the investigation stays started and isn't retried; follow-up work can handle that.
- The binary ships as linux-arm64, so the host needs `git` and `claude` installed. Document that.

## Implementation Plan

### TLDR

First the app seam (automations get ctx and store), then the richer alert payload, then the new event contracts, then the new slice. The slice is built so that the config, the decision (dedupe, stale or unmapped), the workspace (git), the Claude runner and GitHub are each testable through `New`'s seams. The old logger slice and the e2e expectation come last, then docs. Already done in `6fde08b`: `internal/slices/handlesentryissuealert` moved to `internal/slices/investigatesentryissue` (contents still the old logger).

### Steps

1. **App seam (locked decision 1):**
   - `app.Automation` becomes `struct{ Handler func(ctx context.Context, store *eventstore.Store) eventhandler.Handler }`. `RegisterAutomation`'s factory returns it. `Build` doesn't change in shape.
   - `Run` derives a cancellable ctx and builds every automation's handler after opening the store. On shutdown it cancels that ctx after HTTP drain and **before** `runner.Stop()`. The order stays: HTTP drain, cancel automations, stop the runner, close the store.
   - Update `app_test.go` helpers and the registry tests. Add a test that an automation's ctx is cancelled when `Run` shuts down. Use the in-memory pipe listener from `internal/slices/sentryalert_e2e_test.go` (copy the helper) so the test can run here.
2. **Alert payload (decision 7):**
   - `recordsentryissuealert` decodes `data.event.culprit` and `data.event.exception.values[-1].{type,value,stacktrace.frames[].{filename,function,lineno}}`. Sentry's frame keys are `filename`, `function` and `lineno`; check them against Sentry's event payload docs and map them to the locked JSON names.
   - It persists them on `SentryIssueAlertReceived` with `omitempty`, keeping the last 50 frames.
   - Extend the callback tests: one with an exception and more than 50 frames (asserting the cap and order), and one without, where the payload has the same fields as before.
3. **Investigation contracts (decision 2):** add the five type constants and payload structs to `internal/events`, contracts only. The stream naming helper lives in the slice, not in `events`.
4. **Slice config (decision 5):** `New(getenv, logger, opts…)`, or a params struct. Parse `SENTRY_GITHUB_REPOS` strictly: a bad pair, a non-int id or a malformed `owner/repo` is an error naming the variable. Table-test every variable, including defaults and invalid durations.
5. **Handler flow, in order, one per alert event; ignore other event types:**
   1. Decode the alert. A decode error is logged and skipped, as today.
   2. Stale (event timestamp older than the max age): append Skipped `stale` at v0. Unmapped project: append Skipped `unmapped` at v0. A conflict at v0 means it was already handled, so log and return.
   3. Append Started at v0, with the branch `underling/sentry-<issueId>`. A conflict means return.
   4. Clone. Use `git clone` into a fresh dir under the work dir, and create the branch from the default branch (read `origin/HEAD` from the clone; no extra API call). **Auth must not persist in the clone:** pass the token to git only through the environment (`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0=http.extraHeader`/`GIT_CONFIG_VALUE_0=Authorization: Basic base64(x-access-token:<token>)`), never in argv, the remote URL or `.git/config`. A test asserts the clone's `.git/config` contains no token.
   5. Run Claude (decision 6) with ctx plus the timeout. Kill on timeout or ctx cancel, which records Failed `timeout` or `shutdown`. Parse stream-json lines, keep the last `result` event, and apply decision 10's success rule. Anything else is Failed `claude`, with the error or stderr tail.
   6. Check for new commits: `git rev-list --count <base>..HEAD`. Zero means NoChanges.
   7. Push the branch with the same env-only auth. On error, Failed `push`.
   8. Open a draft PR: `POST {apiBase}/repos/{owner}/{repo}/pulls` with `{title, head: branch, base: default, body, draft: true}` and `Authorization: Bearer <token>`. The title is `Fix Sentry issue <issueId>: <alert title>`. The body has the Sentry link (`webUrl`), the issue id and Claude's summary. A non-201 is Failed `pr`, with the status and a body excerpt. On success, record PullRequestOpened with `html_url`.
   9. Always remove the per-run temp dirs (clone, HOME, config dir).

   Log every outcome as one structured line with `handler`, `issueId` and `outcome`; the e2e test relies on the skipped line.
6. **Claude invocation:** build the argv and env in one function that tests can inspect.
   - Verify the settings JSON keys for the sandbox (enabled, filesystem write allowlist set to the clone, auto-allow sandboxed Bash) and a permission mode that works with `-p` and doesn't bypass permissions. Use the Claude Code settings docs (`code.claude.com/docs`) and `claude --help`.
   - Network for sandboxed commands must reach what Codex needs (the OpenAI API). Verify the network allowlist setting and include the minimum hosts.
   - Prompt: a fixed instruction block, then the issue details as data. Tell Claude to use `/at:auto-task` to investigate and fix, to commit on the current branch, and not to push, merge, open PRs or run `/at:ship-task`. It should end with a short summary of the root cause and the fix.
7. **Fake `claude` for tests:** the test binary re-execs itself (`os.Args[0]` with an env switch, the standard Go helper-process pattern), so there's no shell dependency. Modes:
   - happy: commit a file, then emit a result event
   - no-op: emit a result event with no commit
   - error: emit `is_error:true` and exit 1
   - hang: block until killed

   It writes its argv, env and settings to a file the test reads. Use `CLAUDE_BIN` or the `New` param to point at it. The fake remote is a `git init --bare` in `t.TempDir()`. Fake GitHub is an `http.RoundTripper` that records requests and returns a canned 201 or an error. Cover every outcome in the AC list, plus no secrets in the child env, no token in `.git/config`, dedupe (the same issue twice gives one Started), stale and shutdown-kills-run.
8. **Remove the logger:** delete the old logger files in the moved package, so the new handler replaces them. Update the e2e test: give it the new required env, post an alert whose project isn't in `SENTRY_GITHUB_REPOS`, and wait for the skipped log line (outcome `skipped`, reason `unmapped`, matching issueId).
9. **Docs:** update `docs/event-model.md`:
   - the automation section, with the flow, outcomes and stream
   - the new event contracts and the extended payload example
   - the config table
   - the host prerequisites (`git`, `claude`, `codex`, plugin dirs)
   - the isolation model: what the child can and can't see

   Also update "Adding a slice" for the new `Automation` shape.

### Notes

- Bash can't write in the worktree. Use Edit/Write for files, and for deletions run `git -C <worktree> rm` (you may need to run it with the sandbox disabled, which prompts the user). Go needs `GOCACHE=$TMPDIR/gocache GOTMPDIR=$TMPDIR`. Local `git init --bare` and clones inside `t.TempDir()` work in the sandbox; network doesn't.
- Tests mustn't call the real `claude`, git remotes or GitHub. One manual smoke run with the real `claude` is out of scope here and goes in the final report as a follow-up for the user.

## Implementation Notes

Unattended run. All decisions below were made without the user; the ones that touch security are worth a look.

### Claude invocation and settings (decision 6, plan step 6)

Checked against `claude --help` (2.1.310) and the docs at code.claude.com/docs: `settings-reference` (sandbox keys, sandbox path prefixes, `permissions.defaultMode`, `permissions.disableBypassPermissionsMode`), `sandboxing`, `permission-modes` and `headless`.

Argv, run in the work clone:

```
claude --bare -p --verbose --output-format stream-json --no-session-persistence
  --setting-sources "" --settings <json> --permission-mode acceptEdits
  --permission-prompts none --plugin-dir <each> <prompt>
```

Settings JSON (`claudeSettings` in `claude.go`):

```json
{"permissions":{"defaultMode":"acceptEdits","disableBypassPermissionsMode":"disable","deny":["Read(//proc/**)"]},
 "sandbox":{"enabled":true,"failIfUnavailable":true,"autoAllowBashIfSandboxed":true,"allowUnsandboxedCommands":false,
  "filesystem":{"allowWrite":["<work clone, symlinks resolved>"]},"network":{"allowedDomains":["api.openai.com"]}}}
```

Why each setting:
- `sandbox.enabled` turns on the Bash sandbox. `failIfUnavailable` makes Claude Code exit instead of silently running unsandboxed when bubblewrap or socat is missing on Linux. That run then ends as Failed `claude`.
- `allowUnsandboxedCommands: false` turns off the `dangerouslyDisableSandbox` retry ("strict sandbox mode"). The docs say a `false` from `--settings` also makes the sandbox admin-required, so repository settings can't widen it.
- `autoAllowBashIfSandboxed: true` (the default, stated explicitly) lets sandboxed commands run without a prompt. Nobody can answer a prompt in `-p`.
- `filesystem.allowWrite: [clone]`. Writing to the working directory is already the sandbox default, so this entry makes the intent explicit and gives the fake `claude` something to assert. The sandbox also always allows its per-user temp dir. I found no setting that removes that, so "writes limited to the clone" really means the clone plus the sandbox temp dir.
- `network.allowedDomains: ["api.openai.com"]` is the minimum Codex needs with an API key. Any other host would need a prompt, which is denied.
- The sandbox covers shell commands only. Claude's file tools follow permissions. `acceptEdits` auto-approves edits only inside the working directory and `additionalDirectories` (none here). Anything else that would prompt is denied in `-p`, and `--permission-prompts none` makes that explicit and tells Claude not to retry. I chose `acceptEdits` over `dontAsk`: under `dontAsk`, file edits would need explicit allow rules. I didn't use `auto` because it needs the classifier and bypasses less predictably. `bypassPermissions` was ruled out by the task. `disableBypassPermissionsMode: "disable"` also stops subagent frontmatter from switching to bypass.
- `deny: ["Read(//proc/**)"]` is defense in depth (my addition). It stops the Read tool from reading `/proc/<underling pid>/environ` on Linux, which would expose underling's environment. Bash is covered only as far as the sandbox isolates `/proc`, which I couldn't verify.
- `--setting-sources ""` (locked) loads no user, project or local settings files, so the cloned repo's `.claude/settings.json` can't change the run. `--bare` skips CLAUDE.md, hooks (plugin hooks included), installed plugins and MCP. Plugins come only from `--plugin-dir`.

Deviations from the locked argv:
- Added `--verbose`. Every docs example of `-p --output-format stream-json` includes it, and older versions rejected stream-json in print mode without it. It only adds events to the stream, and the last event is still the `result`.
- Added `--permission-prompts none`, which the headless docs recommend for unattended runs (2.1.259+).
- The prompt starts with `/at:auto-task `. Bare mode doesn't list skills to Claude, but `-p` expands a `/skill` at the start of the prompt (headless docs), so this is the reliable way to invoke auto-task. The instructions and the issue JSON follow as the skill's arguments.

### Workspace and token handling (plan step 5.4, 5.7)

- The plan pushed from Claude's clone, which is unsafe. Claude can write anywhere in the clone, including `.git/hooks` and `.git/config` (`url.<x>.insteadOf`, `core.sshCommand`, `credential.helper`, `http.proxy`). Any of those would run, or redirect the request, while underling pushes with the token. Instead, underling bare-clones GitHub into a **mirror** in the run dir (with the token), and Claude works in a second clone **of the mirror** (no GitHub remote, no token). Afterwards underling fetches only `refs/heads/<branch>` from Claude's clone into the mirror, counts `base..branch` there and pushes from the mirror, with `-c core.hooksPath=/dev/null --no-verify`. The mirror sits outside the clone, so the sandbox doesn't let Claude write to it. Fetching from the untrusted clone runs `git-upload-pack` against its config. Git ignores repo-level `uploadpack.packObjectsHook` for exactly this reason, so I judged that acceptable.
- The token reaches git only via `GIT_CONFIG_COUNT/KEY_0/VALUE_0` (`http.extraHeader: Authorization: Basic base64(x-access-token:<token>)`). underling's git commands get a minimal env (`PATH`, run-dir `HOME`, `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1`). Git error text is token-redacted.
- underling sets a repo-local git identity (`underling <underling@users.noreply.github.com>`) and `commit.gpgsign=false` in Claude's clone. The child's HOME is empty, so without them Claude's commits would fail. The plan didn't mention this.
- The default branch and base come from the mirror's `HEAD` (`symbolic-ref`), not `origin/HEAD`. That's equivalent and needs no API call.
- The run dir has its symlinks resolved (`EvalSymlinks`, macOS `/var` vs `/private/var`), so `allowWrite` names the real path.
- The child is started in its own process group, and cancel kills the group (`process_unix.go`), so Codex and Bash grandchildren die with it. `WaitDelay` is 10s.

### Other decisions

- `New` takes a `Params` struct (`Getenv`, `Logger`, `GitHubAPIURL`, `GitHubCloneURL`, `HTTPClient`, `Now`), which the plan allows. "Adding a slice" now mentions this option. The repo URL is `<GitHubCloneURL><owner>/<repo>.git`. Production uses an `http.Client` with a 30s timeout.
- `SentryIssueInvestigationStarted` also carries `project`, as the AC's `{issueId, project, repo, branch}` lists.
- Event version wording: "first event at expected version 0, outcome at expected version 1", matching the locked decision. The doc comment in `events.go` gives the resulting stored versions (1 and 2).
- Log outcome values: `pr_opened`, `no_changes`, `skipped`, `failed`. A version conflict logs `sentry issue already handled` with outcome `duplicate`. Failed outcomes log at WARN.
- Any failure while the shutdown ctx is cancelled is recorded as stage `shutdown`, whichever step was running. If shutdown begins between the automation ctx being cancelled and the runner stopping, a newly picked-up alert gets Started and then Failed `shutdown`, which is final. That window is narrow, and it's consistent with decision 8.
- `SentryIssueAlertReceived` exception details are decoded leniently from `data.event.exception` (as raw JSON), so an unexpected shape drops the details instead of rejecting the alert with 400. Frame keys `filename`/`function`/`lineno` and the oldest-first order come from Sentry's stack trace interface docs (develop.sentry.dev). Inner frame fields are not `omitempty`, so a missing `lineno` is stored as 0.
- Config: durations must be positive, project ids may be mapped only once, and `owner`/`repo` must match `[A-Za-z0-9_.-]+` and can't be `.` or `..`. `PATH` is read through `Getenv` and passed to the child. `CLAUDE_PLUGIN_DIRS` entries are not checked for existence. Claude Code reports missing ones in `system/init.plugin_errors`, which underling doesn't read yet.
- The fake `claude` is the test binary re-executed. It recognizes itself by its first argument, `--bare`. The child env holds only the five allowed variables, so the fake can't take its mode from an env switch, as the plan suggested. It reads the mode from, and appends each call to, files in `INVESTIGATION_WORK_DIR`, two levels above its cwd. Before replying it asserts the settings (sandbox on, unsandboxed retry off, `allowWrite == [cwd]`), records its argv and env, and lists every file in the run dir that contains the GitHub token. This covers the mirror's config and the clone's `.git/config`.
- The automation-ctx test in `internal/app` uses a minimal non-accepting in-memory listener rather than a full copy of the e2e pipe listener, because it needs no HTTP.

### Surprises

- The move commit `6fde08b` left the build broken: the package clause was still `handlesentryissuealert` and `slices_gen.go` still imported the old path. Fixed first in its own commit.
- A plain `git rm` in the worktree fails in the sandbox and needed the sandbox disabled, as expected.

### Open risks and follow-ups

- **Manual smoke run with the real `claude` is still needed.** Confirm that `--setting-sources ""` is accepted, that the `system/init` event shows both plugins loaded with no `plugin_errors`, that the sandbox is active, and that Codex works inside it. Nested sandboxes are a known risk: Codex starts its own sandbox (Seatbelt on macOS, Landlock/bubblewrap on Linux) inside Claude's, which may fail. If it does, Codex may need its own sandbox disabled, or `codex` may need to be listed in `sandbox.excludedCommands`, which weakens isolation. Decide deliberately.
- `-p` silently ignores a settings file that fails validation (from `claude --help`). A typo in the generated JSON would turn the sandbox off without any error. The keys used are all documented ones. The smoke run should check `/status`-equivalent output or the init event.
- The child can read `OPENAI_API_KEY` (accepted). On a host where underling shares its OS user with other processes, the child could inspect them. Running underling as a dedicated user or in a container limits that.
- Runs are serial and can block the handler for up to `INVESTIGATION_TIMEOUT`. A crash mid-run leaves the issue Started forever, with no retry (accepted).
- The four old `internal/app` Run tests that bind TCP ports still fail in this sandbox (`bind: operation not permitted`), as accepted. Everything else passes under `go vet ./... && go test -race ./...`.

### Review fixes

The accepted review findings A to M, one commit per finding (E and F together). Where these notes and the sections above disagree, these win; `docs/event-model.md` is current.

- **A. auto-task stays in place.** The prompt now overrides auto-task's own setup: every step runs in place in the current directory, with no git worktree and no new, switched or renamed branch, committing only on the investigation branch, and the ship and hand-off steps are skipped. After the run, underling fetches all of the clone's branches into the mirror (under `refs/underling/clone/*`, never pushed). Any branch other than the investigation branch with commits the mirror lacks fails the run as Failed `claude`, naming the branches.
- **B. CI configuration isn't pushed.** Before pushing, `git diff --name-only --no-renames -z base..branch` in the mirror. Paths under `.github/`, `.circleci/` or `.buildkite/`, or `.gitlab-ci.yml`, `Jenkinsfile`, `azure-pipelines.yml`, `.travis.yml` or `bitbucket-pipelines.yml` at the repo root, give Failed `push` with "changes CI configuration: <up to 5 paths>". The file names match only at the root, as the finding listed them. `--no-renames` makes a rename out of `.github/` count too. The docs say `GITHUB_TOKEN` must not have the `workflow` scope, and name the residual risk: build scripts that existing push CI runs. **Follow-up:** push to a fork and open the PR from there, so the branch never runs the upstream repo's push CI.
- **C. Stale is judged against the handler's start.** `Params.Now` is read once, when the automation's handler is built. An alert stored before `start - INVESTIGATION_MAX_AGE` is stale. One stored later is never stale, however late it's handled. Skipped `stale` is unchanged (decision 2).
- **D. underling's git has its own HOME.** `<run>/git-home` is never passed to the child and isn't in its `allowWrite`. The child can't read it either: there's a `Read` deny rule, and the sandbox denies the whole work dir. git also gets `GIT_CONFIG_GLOBAL=/dev/null` (`os.DevNull`).
- **E. Read denials.**
  - Checked against code.claude.com/docs (`sandboxing`, `settings-reference`, `permissions`, `headless`):
    - Sandbox paths use plain absolute paths, while permission rules use `//path`.
    - When `denyRead` and `allowRead` overlap, the narrower path wins: an `allowRead` inside a denied region re-opens it, but a `denyRead` inside an allowed region stays denied.
    - The "widening" locks drop only repository-settings entries, so our `--settings` `allowRead` entries stand.
    - `denyRead` doesn't affect Claude's file tools. Those follow `Read(...)` deny rules, which also feed the sandbox's `denyRead`. Deny rules can't be carved out for `//` or `~/` patterns.
    - The docs don't say whether `allowWrite` grants read under a `denyRead`, so the clone and the child's HOME are listed in `allowRead` explicitly.
  - Sandbox `denyRead`:
    - underling's HOME. It's read via `Getenv("HOME")`, which is what `os.UserHomeDir` reads on Unix, so it stays testable.
    - The new `INVESTIGATION_DENY_READ` (comma-separated absolute paths, default empty).
    - The work dir.
  - Sandbox `allowRead`: the clone, the child's HOME, each plugin dir and the claude binary's dir, all symlink-resolved.
  - File-tool `Read(//…/**)` denies: HOME and the extra paths. Deny rules can't be carved out, so instead of the whole work dir they deny every other entry in it at run start, plus this run's `mirror.git` and `git-home`.
  - `resolveReadPaths` (in `New`) rejects config the sandbox couldn't honour:
    - a work dir or plugin dir inside HOME or an extra deny path
    - a plugin dir overlapping the work dir in either direction
    - a denied path at or inside a plugin dir or the claude binary's dir
  - **Deviation:** the default work dir is now `underling-investigations` inside the OS temp dir, not the temp dir itself. Denying reads of the work dir would otherwise deny all of `/tmp` on Linux, which the sandbox and most tools need. The dir is created on demand (0700).
  - **Note for deployment:** plugins installed under `~/.claude/plugins` now fail startup. Put them outside HOME, e.g. `/opt/underling/plugins`. The docs recommend a dedicated OS user. `permissions.blockReadsOutsideWorkingDirectories` would be a stronger, documented alternative for both layers. I didn't adopt it because it re-opens only the plugin dirs under `~/.claude`, and it's unverified with `--plugin-dir` elsewhere.
- **F.** The sandbox's `allowWrite` is now `[<run>/repo, <run>/home]`, so Codex and tool caches can write to HOME.
- **G. Output pipes.**
  - underling creates the child's stdin, stdout and stderr with `os.Pipe` and hands the child its ends, so `exec` runs no copy goroutines and `Wait` returns as soon as claude exits. Goroutines write the prompt and drain stdout and stderr.
  - After `Wait`, on every path, `killGroup` sends SIGKILL to `-pgid`.
  - Then underling waits up to 5s (`drainTimeout`) for the pipe goroutines. If that runs out, it closes the pipes to unblock them, which only happens if something outside the process group holds them. I chose this over `io.Pipe` plus `WaitDelay`, because that would hold every run for the full `WaitDelay` (10s) whenever a grandchild kept stdout open.
  - On non-Unix systems, `killGroup` is a no-op (`process_other.go`).
- **H. Prompt on stdin, capped text.**
  - The prompt goes on stdin, and argv no longer carries it. `claude --help` (2.1.310) doesn't spell out stdin-only prompts. The headless docs say `-p` reads stdin (capped at 10 MB), and `echo … | claude -p` is the documented pipe pattern. **Smoke-test this:** check that a stdin-only prompt still expands the leading `/at:auto-task`. The docs promise expansion only for "the prompt string".
  - `recordsentryissuealert` caps `culprit`, `exceptionType` and `exceptionValue` at 2000 runes and frame `file` and `function` at 500, after the 50-frame cap. The prompt caps the title at 2000 runes.
- **I. PR text.** The title is capped at 200 runes and the body at 60000 bytes, cut at a rune boundary. The body is cut as a whole, so a huge summary loses the footer. Every `@` in the issue id, title and Claude's summary gets a U+200B after it. The Sentry link target is left alone.
- **J. git timeouts.** Clone, fetch and push get 10 minutes, everything else 1 minute (`gitRunner.networkTimeout` and `localTimeout`, injectable in tests). The fetches from Claude's clone are local but still count as "fetch". The commands that talk to GitHub also get `http.lowSpeedLimit=1000` and `http.lowSpeedTime=60`, through `GIT_CONFIG_*`.
- **K. Cleanup.** `removeTree` makes every directory `u+rwx` before `RemoveAll`. On the error path, `newWorkspace` removes the dir `MkdirTemp` created, even when `EvalSymlinks` fails.
- **L. Init check.**
  - The fields come from the headless docs ("Fail CI when a plugin or MCP server doesn't load") and the Agent SDK's `SDKSystemMessage`: `permissionMode`, `plugins[{name, path}]`, and `plugin_errors[{plugin, type, message, path?}]`, which is omitted when empty. The docs say nothing specific to 2.1.310, and claude wasn't run.
  - A plugin counts as loaded from a `--plugin-dir` if its `path` is that dir or inside it, because a folder of plugins loads each child.
  - The check runs only when claude exits 0. A failed check is Failed `claude` with "claude's session did not start as configured: …".
  - The init event doesn't report the sandbox, so the smoke run still has to confirm that.
- **M.** The work clone is cloned from the mirror with `--no-hardlinks`.

Tests that prove each fix, all in `internal/slices/investigatesentryissue` unless noted:

- A: `TestPromptOverridesAutoTasksWorktreeBranchAndShipSteps`, `TestCommitsOnAnotherBranchFailTheRun` (fake mode `other-branch`)
- B: `TestCIConfigPath`, `TestCIConfigChangesAreNotPushed` (fake mode `ci-config`)
- C: `TestALiveAlertHandledLateIsNotStale`, `TestAnAlertStoredLongBeforeTheHandlerStartedIsStale`
- D: `TestTheChildsGitConfigDoesNotAffectUnderlingsPush` (fake mode `hostile-gitconfig`), `TestGitNetworkCommandsAbortSlowTransfers`
- E: `TestClaudeSettingsSandboxTheClone`, `TestClaudeSettingsWithoutAParentHome`, `TestResolveReadPaths*`, `TestLoadConfig*`, the fake's settings check
- F: `TestClaudeSettingsSandboxTheClone`
- G: `TestALeftoverGrandchildNeitherDelaysNorOutlivesTheRun` (Unix only, fake mode `grandchild`)
- H: `TestClaudeCommandArgv`, `TestChildEnvironmentHoldsNoUnderlingSecrets` (the prompt arrives on stdin), `TestPromptCapsTheTitle`, and `recordsentryissuealert`'s `TestExceptionDetailsAreCappedInLength`
- I: `TestPRTitleIsCappedAndMentionsAreNeutralised`, `TestPRBodyNeutralisesMentionsAndKeepsTheSentryLink`, `TestPRBodyIsCappedAtARuneBoundary`
- J: `TestGitTimeouts`, `TestGitRunnerKillsCommandsAfterTheirTimeout`
- K: `TestRemoveTreeRemovesReadOnlyDirs`
- L: `TestReadStreamKeepsTheInitEvent`, `TestCheckInit`, `TestASessionThatDidNotStartAsConfiguredFailsTheRun` (fake modes `init-*`)
- M: `TestWorkspaceCloneSharesNoObjectFilesWithTheMirror`

The fake `claude` now reads its prompt from stdin, emits a `system/init` event that matches its `--plugin-dir` arguments, and checks `allowWrite == [clone, HOME]`, `denyRead ∋ work dir` and `allowRead` starting with `[clone, HOME]`.

Still open for the manual smoke run with the real `claude`, on top of the earlier list:

- a stdin prompt with `/at:auto-task` expansion
- the init event's real `plugins[].path` values for the configured dirs
- the read denials not breaking Codex, git or the plugins
- auto-task honouring the in-place override

### Review fixes, round 2

Ten more findings, one commit each. Where these notes and the sections above disagree, these win; `docs/event-model.md` is current.

1. **Claude config dir readable.** `sandbox.filesystem.allowRead` is now `[<run>/repo, <run>/home, <run>/claude-config, <plugin dirs…>, <claude binary dir>]`, so sandboxed commands can read shell snapshots and plugin data. The fake `claude` checks that `allowRead` starts with clone, `HOME` and `CLAUDE_CONFIG_DIR`.
2. **Stray-branch check.** The rev-list is now `rev-list --count <ref> --not --branches refs/underling/clone/<branch>`. A helper branch whose commits also reached the investigation branch (say by fast-forward) no longer fails the run.
3. **Per-user work dir.**
   - The default is now `underling-investigations-<uid>` in `os.TempDir()`. On Windows it's `underling-investigations`, since `%TEMP%` is already per-user there.
   - At the start of each run, after `MkdirAll(0700)`, `checkWorkDir` Lstats the work dir. The run fails as Failed `clone` unless the dir is a real directory (not a symlink), owned by `os.Getuid()`, with `mode & 0o077 == 0`. The reason names the problem. This is Unix only (`workdir_unix.go`); `workdir_other.go` is a stub.
   - I put the check at run start, not at config load. Config load never created the dir, and a tmp cleaner can delete and recreate it between runs, so a startup check alone wouldn't hold.
   - An existing dir with the wrong mode is never chmodded, only rejected.
   - The test harness now chmods its `t.TempDir()` work dir to 0700, because `t.TempDir()` subdirs are 0755.
4. **removeTree.** The chmod walk moved into `makeRemovable`, which sets every directory to a fixed 0o700. WalkDir still doesn't follow symlinks.
5. **Symlinked deny paths.** `withResolved` emits `HOME` and every `INVESTIGATION_DENY_READ` entry as given and, when it resolves to something different, in its `EvalSymlinks` form too. Duplicates are dropped, and empty entries are skipped. Both `denyRead` and the `Read(//…/**)` rules use it. The paths resolve when the settings are built, at each run.
6. **PATH warning.** `New` logs one WARN per PATH entry that lies, as given or resolved, inside `HOME`, a deny path or the work dir: "PATH entry lies under a path the child may not read; tools there won't run inside the sandbox" (`dir`, `denied`). An entry inside a plugin dir or the claude binary's dir is not warned about, because `allowRead` re-opens those. Empty entries are skipped. Startup doesn't fail.
7. **git WaitDelay.** `gitRunner.waitDelay` defaults to 10s and goes into `cmd.WaitDelay`. It's a field so the test can shorten it.
   - **Surprise:** a fake `git` script with a 100ms timeout didn't reproduce the hang. On macOS, the first exec of a freshly written script can take longer than 100ms, so it was killed before it forked. Also, a bare `sleep &` in `/bin/sh` didn't keep the pipe open the way `(sleep; echo) &` does. The test uses a 1s timeout and the subshell form.
8. **PR body cap.** The body is `header + summary + footer`, and only the summary is cut, to `60000 - len(header) - len(footer)` bytes at a rune boundary. If the cut leaves a ``` fence open (counted per line), it's closed with `\n```` within the budget. The header is cut only if it alone would crowd out the footer, which would take an enormous title or URL. There's no "truncated" marker.
9. **Shutdown pickup.** Right before appending Started, `Handle` checks `h.ctx.Err()`. If the context is cancelled, it logs "underling is shutting down; sentry issue alert left unhandled" and records nothing. This replaces the "Started then Failed `shutdown`" window described under Other decisions.
   - **Known limit:** `eventhandler.run` calls `SetCheckpoint` after every `Handle`, whatever happened inside it. The runner's own context is cancelled only in `Stop`, which comes after the automation context. So the checkpoint does advance past such an alert, and a restart won't redeliver it. The issue is investigated only if a later alert for it arrives (the rule firing again). It's no longer marked Failed for good, but it isn't retried automatically either. Fixing that would need the handler to tell the runner not to checkpoint, which changes the `eventhandler.Handler` contract, so I left it out of scope.
10. **Restart dedupe.** The harness keeps its automation, ctx and store dir. `restart()` closes and reopens the store and builds a new handler. This test passed straight away, since it pins existing behaviour.

Tests that prove each fix, all in `internal/slices/investigatesentryissue`:

1. `TestClaudeSettingsSandboxTheClone`, plus the fake's settings check (every fake-driven test, e.g. `TestHappyPathPushesTheBranchAndOpensADraftPR`)
2. `TestAHelperBranchMergedIntoTheInvestigationBranchIsNotStray` (fake mode `helper-branch`). `TestCommitsOnAnotherBranchFailTheRun` still passes.
3. `TestTheDefaultWorkDirIsPerUser`, `TestCheckWorkDirAcceptsAPrivateDirOfOurOwn`, `TestCheckWorkDirRejectsASymlink`, `TestCheckWorkDirRejectsADirOthersCanUse`, `TestCheckWorkDirRejectsAFile`, `TestAWorkDirOthersCanUseFailsTheRun` (Failed `clone`, no Claude run). These are Unix-only files.
4. `TestMakeRemovableSetsDirsToOwnerOnlyAndFollowsNoSymlinks`, `TestRemoveTreeRemovesReadOnlyDirs`
5. `TestClaudeSettingsDenySymlinkedPathsAndTheirTargets`
6. `TestNewWarnsAboutPATHDirsTheChildMayNotRead`
7. `TestGitRunnerWaitsOnlyBrieflyForPipesAChildStillHolds` (Unix only, about 1s)
8. `TestPRBodyCutsOnlyTheSummaryAtARuneBoundary`, `TestPRBodyKeepsACutCodeFenceClosed`, `TestPRBodyLeavesAShortSummaryWhole`
9. `TestAnAlertPickedUpDuringShutdownIsLeftUnhandled`. `TestShutdownKillsTheRun` still covers a cancel during the run.
10. `TestARepeatedAlertAfterARestartStartsNoSecondInvestigation`

Verification: `go vet ./...` is clean. `go test -race ./...` passes except the four accepted `internal/app` port-binding tests. `GOOS=linux GOARCH=arm64 go build ./...` and `GOOS=windows go vet ./internal/slices/investigatesentryissue/` pass.

### Review fixes, round 3 (gap-check)

Four findings, one commit each. Where these notes and the sections above disagree, these win.

1. **Token-on-disk checks.** They used to search only for the raw token. `TokenForms(token)` (exported from `workspace_test.go` for the external tests) gives three forms: the raw token, `base64("x-access-token:"+token)` (the git header value), and the bare base64 of the token. The fake's run-dir scan (`filesContaining`) and the happy path's remote-config check now look for all three. A new workspace test, `TestTheTokenIsInNeitherGitConfigAfterAPush`, runs clone, commit, stray check, count and push, then asserts that neither the clone's `.git/config` nor the mirror's `config` holds any form. This change is tests only. To prove it, I added `git config http.extraHeader "Authorization: Basic <b64>"` to the clone setup in a `$TMPDIR` copy. Both tests then failed, and the old raw-token test still passed. Since `"x-access-token:"` is 15 bytes, a multiple of 3, the bare base64 of the token also shows up inside the header's base64.
2. **Renamed or deleted investigation branch.** `strayBranches` now checks whether `refs/underling/clone/<branch>` is among the fetched refs. If it isn't, the ref stays out of the `--not` list, and the check sets `ws.branchGone`. `newCommits` then returns 0 without fetching. So a renamed branch with commits gives Failed `claude`, "committed on branches other than <branch>, which underling doesn't push: claude-renamed". A deleted branch with no commits elsewhere gives NoChanges. Before this fix, both failed with git's "unknown revision". This means `newCommits` must run after `strayBranches`, which `investigate` already does.
3. **Startup work-dir check.** If the work dir (the configured one or the default) already exists, `New` runs `checkWorkDir` on it and returns `INVESTIGATION_WORK_DIR: <reason>`, so startup fails with "invalid configuration" (exit 2). A missing dir still passes, and the check at the start of each run stays. On non-Unix systems `checkWorkDir` is still a no-op.
4. **`~~~` fences.** `openFence` now returns the opening run (```` ``` ```` or `~~~`, possibly longer) instead of a bool. As in CommonMark, a fence closes only on a run of the same character that is at least as long and has nothing after it. `truncateSummary` closes a cut fence with `"\n"+run`. It cuts again if the shorter cut leaves a longer run open, until the closing fence fits.

Tests, all in `internal/slices/investigatesentryissue`:

1. `TestTheTokenIsInNeitherGitConfigAfterAPush`, plus the stronger checks in `TestHappyPathPushesTheBranchAndOpensADraftPR`
2. `TestCommitsOnARenamedInvestigationBranchFailTheRun` (fake mode `rename-branch`), `TestADeletedInvestigationBranchWithoutCommitsMeansNoChanges` (fake mode `delete-branch`)
3. `TestNewRejectsAnExistingWorkDirOthersCanUse` (Unix only; an existing 0755 dir)
4. `TestPRBodyClosesACutTildeFenceWithTildes`, `TestOpenFence`

Verification: same as round 2. `go vet ./...` is clean, `go test -race ./...` passes except the four accepted `internal/app` port-binding tests, and the linux/arm64 build and the Windows vet pass.
