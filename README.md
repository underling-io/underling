# underling

underling turns Sentry alerts into draft pull requests. It receives a Sentry
issue alert on a webhook, clones the GitHub repo mapped to the Sentry
project, lets headless Claude Code (with the Codex and auto-task plugins)
fix the issue in a sandbox, and opens a draft PR with Claude's summary.

```
Sentry ──POST /sentry/callback──▶ underling ──clone──▶ GitHub repo
                                     │
                                     ├─ claude -p (sandboxed, in the clone)
                                     │
                                     └─ push underling/sentry-<issueId> ──▶ draft PR
```

It is a single static Go binary with an embedded event store (bbolt), built
as event-modeling vertical slices. Each alert ends in exactly one recorded
outcome: PR opened, no changes, skipped or failed. There is one
investigation per Sentry issue and no retry.

## How it works

Two slices, linked only by the `SentryIssueAlertReceived` event:

| Slice | Kind | What it does |
| --- | --- | --- |
| [`recordsentryissuealert`](internal/slices/recordsentryissuealert/) | state change | `POST /sentry/callback`: checks the HMAC signature, validates and dedupes the alert, records `SentryIssueAlertReceived` |
| [`investigatesentryissue`](internal/slices/investigatesentryissue/) | automation | reacts to the alert: clone, run Claude, refuse CI config changes, push, open a draft PR |

[docs/event-model.md](docs/event-model.md) is the full spec: events and
their payloads, the event store, configuration, Bedrock mode, the child's
isolation, the container image and how to add a slice.

## Requirements

- Go 1.27
- To run investigations: `git`, `claude` (Claude Code), `codex`, `node`, the
  Codex and auto-task plugins, and on Linux `bubblewrap` and `socat` for the
  sandbox ([§ Host prerequisites](docs/event-model.md#host-prerequisites)).
  The container image ships all of them.
- Docker, for the image; OpenTofu 1.12 and the AWS CLI, for the deploy.

## Running

```sh
go run ./cmd/underling -addr :8080 -data-dir ./data
```

Flags: `-addr` (default `:8080`), `-data-dir` (default `./data`) and
`-shutdown-timeout` (default `10s`). `GET /healthz` answers `ok` once it is
listening.

The minimum environment:

| Variable | Purpose |
| --- | --- |
| `SENTRY_CLIENT_SECRET` | the Sentry internal integration's client secret, to verify webhook signatures |
| `SENTRY_GITHUB_REPOS` | `<projectId>=<owner>/<repo>`, comma-separated |
| `GITHUB_TOKEN` | pushes branches and opens PRs; must not have the `workflow` scope |
| `CLAUDE_PLUGIN_DIRS` | the Codex and auto-task plugin dirs, outside `HOME` |
| `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`, and `OPENAI_API_KEY` | model access, unless in Bedrock mode |

Or set `CLAUDE_CODE_USE_BEDROCK=1` with `AWS_REGION`, `ANTHROPIC_MODEL` and
`CODEX_BEDROCK_MODEL` to run Claude Code and Codex on Amazon Bedrock with
underling's AWS credentials instead. All options are under
[§ Configuration](docs/event-model.md#configuration) and
[§ Bedrock mode](docs/event-model.md#bedrock-mode).

Run underling as a dedicated user (or in the container): the sandbox narrows
what Claude can touch, but it still runs as underling's user.

## Development

| Command | What it does |
| --- | --- |
| `make check` | `go vet` and `go test -race` over everything |
| `go generate ./...` | regenerates `internal/slices/slices_gen.go` after adding a slice |
| `make system-test` | replays a real upstream bug fix with the real Claude Code; costs tokens, up to 45 minutes |
| `make build-linux-arm64` | builds `bin/underling-linux-arm64` |
| `make image` / `make image-smoke` | builds the linux/arm64 image / smoke-tests it (Docker) |
| `make infra-check` | formats, validates and tests `infra/` against a mocked AWS provider |

The `Makefile` documents what each test target needs. Slices never import
each other; tests in `internal/slices` enforce that and the registration
rules ([§ Adding a slice](docs/event-model.md#adding-a-slice)).

## Deploying

[infra/README.md](infra/README.md) deploys the image to AWS as one ECS
Fargate task behind an HTTPS ALB with OpenTofu, step by step, and describes
`make postdeploy-test`, which drives the deployed service end to end against
a throwaway GitHub repo (destructive: it force-pushes that repo).

## Repository layout

```
cmd/underling/          main
internal/events/        event contracts shared between slices
internal/platform/      app (registration, router, lifecycle), eventstore, eventhandler
internal/slices/        one package per slice
internal/systemtest/    system and post-deploy tests (build tags system, postdeploy)
deploy/image/           Dockerfile, smoke test, sandbox probe
infra/                  OpenTofu for ECS Fargate + ALB
docs/                   the event model spec
tasks/                  the task history behind each change
```
