---
title: Investigate Sentry Alerts with Claude and Open a Draft PR
date: 2026-10-25
status: ready-for-dev
type: feat
---

## Investigate Sentry Alerts with Claude and Open a Draft PR

Replace the placeholder Sentry alert handler. For each new Sentry issue, heinzel clones the mapped GitHub repo, runs headless Claude Code (with the Codex and auto-task plugins) in a locked-down child process to investigate and fix the issue, then pushes the result and opens a draft PR itself.

### Acceptance Criteria

- **Repo mapping.** Config maps a Sentry project id to a GitHub `owner/repo`. An alert for an unmapped project is logged and skipped, with no clone, no Claude run and no PR.
- **Issue details.** `SentryIssueAlertReceived` gains optional fields holding the exception details Sentry sends: culprit, exception type and value, and stack frames (file, function, line). They're filled for new alerts. Older stored events without them still decode and are handled.
- **One investigation per Sentry issue.** Repeated alerts for the same issue id (redeliveries, or the rule firing again) start at most one investigation, even across restarts.
- **Isolated Claude run.**
  - heinzel starts the `claude` CLI headlessly as a child process, with its working directory set to a fresh clone of the target repo on a new branch.
  - The child's environment holds only what it needs: `PATH`, a temporary `HOME`, its own Claude Code config dir with just the Codex and auto-task plugins, the Anthropic credential and the OpenAI credential for Codex. It never sees the GitHub token, the Sentry client secret or any other heinzel environment variable.
  - Claude Code's sandbox is on, and file writes are limited to the clone.
  - The prompt carries the issue details and asks Claude to investigate and fix the issue with auto-task, committing on the branch without pushing, merging or opening a PR.
  - The run has a configurable timeout, after which the child is killed.
- **Draft PR by heinzel.**
  - If the run succeeds and the branch has new commits, heinzel pushes the branch with its own GitHub token and opens a **draft** PR against the default branch.
  - The PR title names the Sentry issue. The body links the Sentry issue and includes Claude's final summary.
  - With no new commits there's no PR.
- **Outcome recorded.** Each investigation ends in an event: PR opened (with the PR URL), no changes, skipped (unmapped project), or failed (clone, Claude, push or PR error, or timeout, with the reason). A failure never stops the handler loop or later alerts.
- **Testable without the outside world.** Tests use a fake `claude` executable and a local bare git repo as the remote, and fake GitHub over an in-memory HTTP transport. They cover: the happy path to a draft PR, the child environment containing none of heinzel's secrets, unmapped project, no commits, Claude failure or timeout, and dedupe per issue.

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
11. **Sandbox evidence.** The fake `claude` asserts the generated settings enable the sandbox and limit writes to the clone. It also writes the environment it received to a file, which the test checks for absent secrets (`GITHUB_TOKEN`, `SENTRY_CLIENT_SECRET`, and any other variable heinzel's own environment has).
12. **Stdlib only.** Git runs through `os/exec`, and GitHub through `net/http` against the REST API, inside the slice. No new modules and no shared helper package, per the import guard.

### Notes

- Decisions by the user:
  - GitHub, with each Sentry project mapped to a repo.
  - heinzel, not Claude, holds the write token, pushes and opens the PR.
  - The `claude` CLI is driven from Go: the Agent SDK has no Go version, and the plain Go API SDK can't run Claude Code plugins.
  - One task, not an epic.
- The Codex plugin needs OpenAI credentials inside Claude's environment, so Claude can read them. That's accepted, and it's best to use a dedicated, low-limit key.
- The alert carries a Sentry project **id**, not a slug (`data.event.project`), so the mapping is keyed by id.
- New config follows the existing pattern: slices load their own config from `Getenv` in their constructor, and invalid config fails startup with "invalid configuration".
- Check the exact `claude` CLI flags (headless mode, stream-json output, settings and plugin dirs, permission mode, sandbox) against the installed Claude Code version and the docs. Don't rely on memory.
- A long run blocks only this handler's loop, and runs go one at a time, which is fine for now. If heinzel crashes mid-run, the investigation stays started and isn't retried; follow-up work can handle that.
- The binary ships as linux-arm64, so the host needs `git` and `claude` installed. Document that.
