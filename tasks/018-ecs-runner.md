---
title: Run Investigations in a Separate ECS Runner Task
date: 2026-11-12
status: ready-for-dev
type: feat
---

## Run Investigations in a Separate ECS Runner Task

Each investigation's Claude step runs in its own ECS Fargate task instead of inside the underling process. underling keeps the webhook, the event store, the GitHub token, the checks, the push and the PR.

## Locked Decisions

- **Files, not a channel.** underling and the runner exchange files through S3, under `runs/<runId>/input/` and `runs/<runId>/output/`. In: a git bundle of the repo and the prompt. Out: a result file (status, Claude's summary, failure reason) and a git bundle of the runner's branches. No callback, queue or Step Functions.
- **The task stopping is the signal.** underling polls ECS for the task's state. A stopped task without a result file is a failure.
- **Presigned URLs only.** The runner reads and writes its files through presigned URLs it gets as environment overrides. Its role holds today's Bedrock permissions and nothing else. It never holds the GitHub token or the Sentry secret and never talks to GitHub.
- **Model settings travel as overrides; credentials don't.** underling passes the Bedrock model and region settings with each run. The runner gets its AWS credentials from its own task role and hands them to Claude as today.
- **Runner output is untrusted.** underling fetches the returned bundle into its mirror and runs today's stray-branch, no-commits and CI-config checks on it, unchanged, before pushing.
- **No Claude Code sandbox in the runner.** bubblewrap is expected not to run on Fargate (016), so the container and its network rules are the boundary. In-process mode keeps its sandbox, and the image keeps bubblewrap and the probe script.
- **Network: HTTPS egress, public IP.** The runner runs in today's public subnets with a public IP, no inbound, and outbound 443 only. So Claude in the runner can reach any HTTPS host; that is an accepted exposure the docs name. A subnet with no internet route and VPC endpoints is a follow-up.
- **In-process mode stays** the default, for local runs and tests. ECS mode is opt-in through configuration and needs Bedrock mode.
- **One image.** The runner command ships in today's image; the runner task definition only overrides the command.
- **Timeout.** The runner enforces `INVESTIGATION_TIMEOUT` on Claude. As a backstop underling stops a task still running 2 minutes after that, counted from when the task started running, and gives up on a task that hasn't started running after 10 minutes.
- **Events.** Persisted payloads don't change. One new failure stage, `runner`, for anything that fails before the runner's container runs; from then on failures keep today's stages.
- **Still one run at a time**, and a restart of underling still loses the run in flight (see Notes).
- **Nothing leaves the branch without the user.** No push, merge or apply: a push to `main` touching `infra/` deploys.

## Tasks

| #   | Task                                         | File                                | Status |
| --- | -------------------------------------------- | ----------------------------------- | ------ |
| 01  | Runner command and file contract             | 018.01-runner-command.md            | in-dev |
| 02  | ECS driver                                   | 018.02-ecs-driver.md                | ready-for-dev |
| 03  | Infra: bucket, runner task definition, roles | 018.03-runner-infra.md              | ready-for-dev |

## Notes

- **Resumable runs are a follow-up, not part of this epic.** They need a data dir that survives a restart first; today's is ephemeral container storage (016).
- **Human calls to raise at the end:** the runner's open HTTPS egress (see Network), and resumable runs being dropped from the scope first agreed.
