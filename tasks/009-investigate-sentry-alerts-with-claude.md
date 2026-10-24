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
