---
title: Deploy heinzel to AWS ECS with Bedrock
date: 2026-11-03
status: ready-for-dev
type: feat
---

## Deploy heinzel to AWS ECS with Bedrock

Run heinzel on AWS: one ECS service behind an ALB, provisioned with OpenTofu, with Claude Code and Codex both talking to Bedrock through the task role instead of Anthropic and OpenAI keys. A post-deploy system test then drives the deployed service through its public webhook and checks the result in a real GitHub repo.

## Locked Decisions

- **Bedrock only in AWS.** In the deployment, neither `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` nor `OPENAI_API_KEY` exists.
- **Codex on Bedrock** uses Codex's built-in `amazon-bedrock` provider: https://help.openai.com/en/articles/20001253.
- **Fargate first.** The service runs on ECS Fargate. If bubblewrap can't run on Fargate, the switch to EC2-backed ECS is a follow-up task (016.05), not a rework of 016.03. Never ship with the sandbox off.
- **One task, ephemeral data.** Desired count 1, and the data dir is ephemeral container storage. EFS is a follow-up.
- **HTTPS.** OpenTofu creates a DNS-validated ACM cert for a domain in an existing Route 53 hosted zone. The domain and the zone are variables.
- **Secrets.** `GITHUB_TOKEN` and `SENTRY_CLIENT_SECRET` live in Secrets Manager. OpenTofu creates the secrets but never their values, so the values stay out of state.
- **Least privilege.** The task role may only invoke the configured Bedrock models.
- **Layout.** OpenTofu code lives in `infra/`.

## Tasks

| #   | Task                              | File                              | Status |
| --- | --------------------------------- | --------------------------------- | ------ |
| 01  | Bedrock mode for Claude and Codex | 016.01-bedrock-mode.md            | done   |
| 02  | Container image                   | 016.02-container-image.md         | ready-for-dev |
| 03  | OpenTofu infra: ECS + ALB         | 016.03-opentofu-ecs-alb.md        | ready-for-dev |
| 04  | Post-deploy system test           | 016.04-post-deploy-system-test.md | ready-for-dev |
