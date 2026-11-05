---
title: Infra Deploy Workflow
date: 2026-11-05
status: done
type: feat
---

## Infra Deploy Workflow

A GitHub Actions workflow that checks `infra/` on pull requests and deploys it to the AWS account on `main`, authenticating via OIDC as `arn:aws:iam::945752100213:role/github-actions/github-actions-deploy-role`.

### Acceptance Criteria

- `.github/workflows/infra.yml` runs `make infra-check` (no AWS credentials) on pull requests touching `infra/` or the workflow.
- On push to `main` touching `infra/` (and on manual dispatch from `main`) it assumes the role above through OIDC (`id-token: write`, no long-lived keys), then runs `tofu apply -auto-approve`. Deploys never run concurrently and never cancel mid-apply.
- State lives in S3: the workflow takes the bucket from the repo variable `TF_STATE_BUCKET` and fails with a clear message when it is unset (local state in CI would be lost).
- infra/README.md documents the workflow, the repo variable and the role's trust requirements.

### Notes (decisions made unattended)

- No GitHub `environment:` on the deploy job: it would change the OIDC `sub` claim to `environment:<name>`, and the role's trust policy is unknown. The job is gated on `refs/heads/main` instead.
- The image tag is whatever `infra/terraform.tfvars` holds; the workflow does not build or push images.
- The first deploy (README § First deploy) is still manual: it needs a targeted apply, an image push and secret values.
- Review: deploys trigger on `.tf`, tfvars, lock file and workflow changes only; the plan is printed and applied as a saved plan. Not done: SHA-pinning actions (tag pins kept), running fmt/validate before credentials.
- **Human call:** `infra/terraform.tfvars` still holds placeholders (domain, zone, image tag, Sentry repos). Set real values before setting `TF_STATE_BUCKET`, or the first deploy applies them.
