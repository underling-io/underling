---
title: Deploy underling to AWS ECS with Bedrock
date: 2026-11-03
status: ready-for-signoff
type: feat
---

## Deploy underling to AWS ECS with Bedrock

Run underling on AWS: one ECS service behind an ALB, provisioned with OpenTofu, with Claude Code and Codex both talking to Bedrock through the task role instead of Anthropic and OpenAI keys. A post-deploy system test then drives the deployed service through its public webhook and checks the result in a real GitHub repo.

## Locked Decisions

- **Bedrock only in AWS.** In the deployment, neither `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` nor `OPENAI_API_KEY` exists.
- **Codex on Bedrock** uses Codex's built-in `amazon-bedrock` provider: https://help.openai.com/en/articles/20001253.
- **Fargate first.** The service runs on ECS Fargate. If bubblewrap can't run on Fargate, the switch to EC2-backed ECS is a follow-up task (016.05), not a rework of 016.03. Never ship with the sandbox off.
- **Sandbox vs. Fargate (found in 016.02, needs the user's call).** Locally the image's sandbox probe fails under Fargate-allowed settings (`bwrap: No permissions to create new namespace`). It passes only with unconfined seccomp plus unmasked `/proc`. Two alternatives: a privileged or unconfined container on EC2-backed ECS, or Claude Code's `enableWeakerNestedSandbox`, which lets sandboxed commands read `/proc/<underling pid>/environ` and with it `GITHUB_TOKEN`. Both are security trade-offs, so none is chosen here.
- **One task, ephemeral data.** Desired count 1, and the data dir is ephemeral container storage. EFS is a follow-up.
- **HTTPS.** OpenTofu creates a DNS-validated ACM cert for a domain in an existing Route 53 hosted zone. The domain and the zone are variables.
- **Secrets.** `GITHUB_TOKEN` and `SENTRY_CLIENT_SECRET` live in Secrets Manager. OpenTofu creates the secrets but never their values, so the values stay out of state.
- **Least privilege.** The task role may only invoke the configured Bedrock models.
- **Layout.** OpenTofu code lives in `infra/`.

## Tasks

| #   | Task                              | File                              | Status |
| --- | --------------------------------- | --------------------------------- | ------ |
| 01  | Bedrock mode for Claude and Codex | 016.01-bedrock-mode.md            | done   |
| 02  | Container image                   | 016.02-container-image.md         | done   |
| 03  | OpenTofu infra: ECS + ALB         | 016.03-opentofu-ecs-alb.md        | done   |
| 04  | Post-deploy system test           | 016.04-post-deploy-system-test.md | done   |

## Notes

- **016.03 (OpenTofu infra).** `make infra-check` (init, `fmt -check`, `validate`, and `tofu test` against a mocked AWS provider) passes without AWS credentials; that is the bar here. No AWS credentials were available, so these pass to the user: the first real apply (infra/README.md runbook), the Fargate probe, the two IAM points only a real run settles (the global profile's region-less foundation-model ARN under the `bedrock:InferenceProfileArn` condition, and whether `bedrock-mantle:Model` compares against the plain id), and whether ECS rotates task-role credentials more than `INVESTIGATION_TIMEOUT` before expiry. The Fargate probe is expected to fail (`bwrap: No permissions to create new namespace`, as in 016.02's smoke); then the rollout stops and 016.05 opens.
- **016.04 (post-deploy system test).** `make postdeploy-test` compiles under its tag and, with no inputs, fails at the input check naming all five variables; `make check` stays green. No deployed stack or tokens were available, so the real run passes to the user (infra/README.md § Post-deploy system test). It also settles whether underling can push a branch whose history holds `.github/workflows` without the workflow scope.
