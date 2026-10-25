---
title: Readable Sentry Investigator with Given/When/Then Tests
date: 2026-10-25
status: ready-for-dev
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
