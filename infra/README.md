# heinzel on AWS (OpenTofu)

This directory deploys the heinzel image ([docs/event-model.md § Container
image](../docs/event-model.md#container-image)) as one ECS Fargate task
behind an HTTPS ALB. Claude Code and Codex reach Bedrock through the task
role ([§ Bedrock mode](../docs/event-model.md#bedrock-mode), [§ AWS
needs](../docs/event-model.md#aws-needs)).

- **`module.base`** holds the VPC (two AZs, public subnets, no NAT), the ECR
  repository `heinzel`, the ECS cluster `heinzel`, the log group
  `/ecs/heinzel`, the secrets `heinzel/github-token` and
  `heinzel/sentry-client-secret` (OpenTofu never sets their values), the
  execution and task roles, and the probe task definition `heinzel-probe`.
- **`module.service`** holds the ACM certificate and its DNS validation, the
  ALB, the `heinzel` task definition and service, and the Route 53 alias.
  Port 80 redirects to 443. On 443 only `/sentry/callback` reaches heinzel;
  everything else gets the ALB's own 404.

The service runs one task. A deploy stops the old task before it starts the
new one, so two writers never share the event store. That leaves a short
window with no receiver. The event store is ephemeral task storage, so a new
task starts empty.

`make infra-check` runs `tofu init -backend=false`, `fmt -check`,
`validate` and `tofu test` (a plan against a mocked AWS provider). It needs
network access to the OpenTofu registry, but no AWS credentials.

## Prerequisites

- OpenTofu 1.12, AWS CLI v2, Docker and jq.
- AWS credentials for an account where you may create everything above. The
  plan also reads Bedrock's model and inference-profile lists, so they need
  `bedrock:ListFoundationModels` and `bedrock:ListInferenceProfiles`.
- A public Route 53 hosted zone that holds the domain.
- Bedrock model access for the Claude models you configure, enabled once per
  account, including Anthropic's use-case form.
- Bedrock's Mantle endpoint offering the Codex model (`codex_bedrock_model`)
  in its region (`codex_bedrock_region`, or `aws_region`).

## Inputs

Copy `terraform.tfvars.example` to `terraform.tfvars` (gitignored) and fill
it in. Each model, region, repo and timeout variable maps 1:1 onto the
container env var of the same name in upper case. A `null` optional leaves
the env var out, so the app's default holds.

The plan classifies `anthropic_model` and `anthropic_small_fast_model`
against what Bedrock offers in `aws_region`. A system-defined inference
profile gets its own ARN plus the foundation models it routes to, in every
region it routes to. A foundation-model id gets its regional ARN. Any other
id fails the plan with a message naming it and the region.

## State

State is local (`infra/terraform.tfstate`, gitignored) by default. To keep it
in S3 instead, without changing the code:

1. Create `infra/backend_override.tf` (gitignored):

   ```hcl
   terraform {
     backend "s3" {}
   }
   ```

   A backend's type can't come from `-backend-config` alone, which is why
   this file exists.

2. Create `infra/s3.tfbackend` (gitignored):

   ```hcl
   bucket       = "my-tofu-state"
   key          = "heinzel/terraform.tfstate"
   region       = "eu-central-1"
   use_lockfile = true
   ```

3. Run `tofu -chdir=infra init -migrate-state -backend-config=s3.tfbackend`.

## First deploy

Run these in order. The commands assume `infra/` as the working dir unless
they say `make`, which runs from the repo root, and `AWS_REGION` set to
`aws_region` for the AWS CLI.

### 1. Everything but the service

```sh
tofu init
tofu apply -exclude=module.service
```

This creates the network, ECR, the cluster, the roles, the log group, the
secrets (empty) and the probe task definition. Every output resolves after
it.

### 2. Push the image

From the repo root, with the same tag as `image_tag`:

```sh
make image IMAGE_TAG=<tag>
make image-push ECR_REPOSITORY_URL=$(tofu -chdir=infra output -raw ecr_repository_url) IMAGE_TAG=<tag>
```

### 3. The Fargate probe

Run the image's sandbox probe ([§ Sandbox
probe](../docs/event-model.md#sandbox-probe)) as a one-off Fargate task, with
the service's security settings:

```sh
cluster=$(tofu output -raw cluster_name)
task=$(aws ecs run-task --launch-type FARGATE --cluster "$cluster" \
  --task-definition "$(tofu output -raw probe_task_definition_arn)" \
  --network-configuration "$(tofu output -raw probe_network_configuration)" \
  --query 'tasks[0].taskArn' --output text)
aws ecs wait tasks-stopped --cluster "$cluster" --tasks "$task"
aws ecs describe-tasks --cluster "$cluster" --tasks "$task" \
  --query 'tasks[0].{exitCode: containers[0].exitCode, reason: containers[0].reason, stoppedReason: stoppedReason}'
aws logs tail /ecs/heinzel --since 1h --log-stream-names "probe/heinzel/${task##*/}"
```

- **Pass:** exit code 0, and the last log line is `sandbox probe: PASS`.
- **`sandbox probe: FAIL: …`:** stop the rollout here and open 016.05 (see
  the epic's "Sandbox vs. Fargate"). The local smoke predicts this outcome
  (`bwrap: No permissions to create new namespace`). Never ship with the
  sandbox off.
- **Anything else**, such as an image pull error or a missing log stream, is
  an infra issue to fix before going on.

### 4. Set the secrets

Create the Sentry internal integration; its client secret is the value of
`heinzel/sentry-client-secret`. `GITHUB_TOKEN` must be able to push branches
and open PRs on the mapped repos, and must not have the `workflow` scope.

Keep the values out of argv and shell history: read them without echo and
pass them through a private temp file.

```sh
put_secret() {
  (
    umask 077
    f=$(mktemp)
    trap 'rm -f "$f"' EXIT
    printf '%s: ' "$1" >&2
    IFS= read -rs v
    echo >&2
    printf '%s' "$v" > "$f"
    aws secretsmanager put-secret-value --secret-id "$2" --secret-string "file://$f" >/dev/null
  )
}
put_secret GITHUB_TOKEN "$(tofu output -raw github_token_secret_arn)"
put_secret SENTRY_CLIENT_SECRET "$(tofu output -raw sentry_client_secret_arn)"
```

ECS reads the secrets when a task starts. After changing one later, run
`aws ecs update-service --cluster heinzel --service heinzel --force-new-deployment`.

### 5. Apply the rest

```sh
tofu apply
```

This creates the certificate, the ALB, DNS and the service, and waits until
the service is steady.

### 6. Point Sentry at the webhook

Set the integration's webhook URL to `tofu output -raw webhook_url` and add
the integration as an alert-rule action. Then check:

```sh
curl -sI http://<domain>/                     # 301 to https
curl -s -o /dev/null -w '%{http_code}\n' https://<domain>/sentry/callback   # 405 from heinzel, not the ALB's 404
```

### 7. Roll out a new image tag

```sh
make image image-push ECR_REPOSITORY_URL=$(tofu -chdir=infra output -raw ecr_repository_url)
# set image_tag = "<new tag>" in infra/terraform.tfvars, then:
tofu -chdir=infra apply
```

`make image` tags with the 12-character `HEAD` commit; use that tag. Keep
it in terraform.tfvars, not `-var`: a later plain `tofu apply` would
otherwise roll the service back to the tag still in the file. To check
that the old task stopped before the new one started (`stoppedAt` of the old
earlier than `createdAt` of the new):

```sh
aws ecs describe-tasks --cluster heinzel \
  --tasks $(aws ecs list-tasks --cluster heinzel --family heinzel --desired-status STOPPED --query 'taskArns' --output text) \
          $(aws ecs list-tasks --cluster heinzel --family heinzel --query 'taskArns' --output text) \
  --query 'tasks[].{task: taskArn, image: containers[0].image, createdAt: createdAt, stoppedAt: stoppedAt}'
```

Expect a short window with no receiver. A failed deploy rolls back on its
own (circuit breaker). To roll back by hand, apply the old tag.

## Notes

- **Logs:** CloudWatch log group `/ecs/heinzel`. The service's streams start
  with `heinzel/`, the probe's with `probe/`.
- **Credentials:** the task role's credentials come from the ECS endpoint.
  Whether ECS hands out new ones more than `INVESTIGATION_TIMEOUT` before the
  old ones expire is unverified; if it doesn't, runs fail as stage `claude`
  around each rotation (docs/event-model.md § Bedrock mode).
- **Destroy:** empty the ECR repository first (`tofu destroy` doesn't force
  it). The secrets go into a 7-day recovery window, so recreating them under
  the same names within that window fails until you restore or force-delete
  them.
