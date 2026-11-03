# Event Model

heinzel is organised as vertical slices, event-modeling style. A slice is
either a **state change** (trigger → command → event) or an **automation**
(event → reaction). A state change is an `app.StateChange` (one HTTP route,
served with [Gin](https://github.com/gin-gonic/gin)); an automation is an
`app.Automation` (one event handler). Each slice is one package,
`internal/slices/<slice>/`, holding everything it needs.
Slices never import each other; they are coupled only through the events in
`internal/events`. The shared platform they build on lives in
`internal/platform/`: `app` (slice registration, the router, the
lifecycle), `eventstore` and `eventhandler`.

heinzel is a single static binary (pure Go, no cgo). It needs nothing but
a data dir (`-data-dir`, default `./data`) and the slices' environment
variables. Other flags: `-addr` (default `:8080`) and `-shutdown-timeout`
(default `10s`). Invalid arguments or configuration exit with status 2, a
runtime error with 1.

Besides the slices' routes, the `app` router serves one platform route:
`GET /healthz` answers 200 with body `ok` once heinzel is listening. It has
no auth, writes no event and doesn't touch the store; another method on it
gets 405. The [container image](#container-image)'s health check uses it.

## Event store

The event store is embedded: [hallgren/eventsourcing](https://github.com/hallgren/eventsourcing)'s
bbolt event store, plus heinzel's own checkpoints. The data dir holds two
files:

- `events.db`, in hallgren's bbolt format. Each stream is one hallgren
  aggregate: `AggregateType` is always `heinzel`, `AggregateID` is the
  stream, `Version` the event's version in the stream, `Reason` the event
  type, `Data` the JSON payload as-is and `GlobalVersion` the global
  position. Timestamps are UTC with nanoseconds.
- `checkpoints.db`, a bbolt file with one bucket, `checkpoints`: handler
  name → the last position it processed, 8-byte big-endian.

Both files are opened with a one-second lock timeout, so a second heinzel
on the same data dir fails at startup. A `heinzel.db` from before this
store (the earlier hand-rolled layout) is never opened or migrated.

`eventstore.Store` is what slices see:

- `Append(stream, expectedVersion, events)` appends if the stream is at
  `expectedVersion` (0: the stream must not exist yet). Otherwise it
  returns `eventstore.ErrConflict` (match it with `errors.Is`) and stores
  nothing. It returns the stream's new version and the global position of
  the last event.
- `ReadAfter(position, limit)` returns stored events in global order.
- Each successful append wakes the event handlers.

`eventhandler` runs one goroutine per automation's handler: it resumes
after the handler's checkpoint, reads in batches, and checkpoints after
each event. Delivery is at least once (a crash between `Handle` and the
checkpoint repeats the event), so handlers make their appends idempotent
with expected versions. Stopping lets the current `Handle` finish and
leaves later events for the next start. heinzel doesn't use hallgren's
projections, which keep positions in memory, poll, and hold a read
transaction across callbacks.

On shutdown heinzel drains HTTP requests (closing them after
`-shutdown-timeout`), cancels the automations' context, stops the event
handlers and closes the store, in that order.

## Sentry

Two independent slices, linked by the `SentryIssueAlertReceived` event.

### State change: record Sentry issue alert (`internal/slices/recordsentryissuealert/`)

| Trigger | Command | Event |
| --- | --- | --- |
| `POST /sentry/callback` (Sentry internal-integration webhook) | Record Sentry issue alert | `SentryIssueAlertReceived` |

The slice's constructor loads the client secret from `SENTRY_CLIENT_SECRET` and
fails if it is missing or empty.

The command runs these checks in order:

1. **Size.** The body is limited to 10 MiB. A larger body gets 413.
2. **Signature.** `Sentry-Hook-Signature` must be the hex HMAC-SHA256 of the raw
   body, keyed with the client secret (`SENTRY_CLIENT_SECRET`). It is checked
   before the body is decoded. A missing or wrong signature gets 401.
3. **Resource.** Only `Sentry-Hook-Resource: event_alert` is recorded. Any other,
   missing or unknown resource gets 200 and nothing is stored.
4. **Validation.** `data.event.event_id`, `data.event.issue_id` (a string) and
   `data.triggered_rule` must be non-blank. Malformed JSON or a wrong field type
   also gets 400. Values are stored as sent, not trimmed.
5. **Dedupe.** Each alert gets its own stream,
   `sentry-issue-alert-<hex sha256(event_id + "\x00" + triggered_rule)>`, appended
   with expected version 0. A redelivery hits a version conflict and gets 200
   without storing anything. The same Sentry event triggering a different rule is
   a different alert.

`SentryIssueAlertReceived` payload (persisted, so the shape is locked):

```json
{
  "eventId": "aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4",
  "issueId": "1117540176",
  "project": 1,
  "title": "ReferenceError: blooopy is not defined",
  "level": "error",
  "webUrl": "https://sentry.io/organizations/test-org/issues/1117540176/events/aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4/",
  "triggeredRule": "Very Important Alert Rule!",
  "culprit": "?(runner)",
  "exceptionType": "ReferenceError",
  "exceptionValue": "blooopy is not defined",
  "frames": [
    {"file": "example.js", "function": "runner", "line": 3}
  ]
}
```

`culprit`, `exceptionType`, `exceptionValue` and `frames` are optional and
omitted when empty, so alerts stored before they existed still decode.
`culprit` is `data.event.culprit`. The exception fields come from the last
(outermost) entry of `data.event.exception.values`: its `type`, `value` and
`stacktrace.frames[].{filename, function, lineno}`, in Sentry's order (oldest
call first), keeping at most the last 50 frames. An exception of an unexpected
shape drops these details but still records the alert. The details end up in
Claude's prompt and in pull requests, so they are capped: `culprit`,
`exceptionType` and `exceptionValue` at 2000 runes, each frame's `file` and
`function` at 500.

### Automation: investigate Sentry issue (`internal/slices/investigatesentryissue/`)

| Event | Reaction | Events |
| --- | --- | --- |
| `SentryIssueAlertReceived` | `sentry-issue-investigator`: clone the mapped GitHub repo, let headless Claude Code fix the issue, push and open a draft PR | `SentryIssueInvestigationStarted`, then `…PullRequestOpened`, `…NoChanges` or `…Failed`; or `…Skipped` |

The steps below are the decision path in `investigator.go`, which reaches
git, Claude and GitHub only through small interfaces. `infrastructure.go`
implements them with the slice's sub-packages: `workspace` (the run dir,
clone, mirror and git commands), `claude` (the sandboxed child, its settings
and output) and `github` (the draft PR). `investigator_test.go` specifies
each outcome as an in-memory given/when/then test.

The handler's name is its checkpoint key, so it must never change. It ignores
other event types, and logs and skips a payload it cannot decode. For each
alert, in order:

1. **Stale.** An alert stored more than `INVESTIGATION_MAX_AGE` before the
   handler started (when heinzel built it at startup) is recorded as Skipped
   `stale`. The first deploy therefore doesn't investigate past alerts, while
   an alert stored after startup is never stale, however long a backlog of
   runs delays it.
2. **Unmapped.** An alert whose Sentry project id isn't in
   `SENTRY_GITHUB_REPOS` is recorded as Skipped `unmapped`.
3. **Started.** Otherwise Started is recorded with the repo and the branch
   `heinzel/sentry-<issueId>`.
4. **Clone.** heinzel makes a fresh run dir under `INVESTIGATION_WORK_DIR`,
   bare-clones the repo into it as a mirror, and clones the mirror (without
   hard links) into a work clone on the new branch, from the default branch.
   The work clone has a local git identity (`heinzel`) so Claude can commit.
   Clone, fetch and push time out after 10 minutes, every other git command
   after 1 minute, and transfers to GitHub abort below 1000 bytes/s for 60
   seconds. A timed-out command's output is read for at most 10 more
   seconds, so a helper it left holding the pipes (say `git-remote-https`)
   can't hold the run up.
5. **Claude.** In [Bedrock mode](#bedrock-mode), heinzel first gets the
   run's AWS credentials and writes the child's Codex config; either
   failing fails the run as stage `claude` before the child starts. heinzel
   then runs `claude` in the work clone (see
   [Isolation](#isolation)) with the prompt on stdin. The run succeeds when
   the child exits 0, its `system/init` event shows the session started as
   configured (permission mode `acceptEdits`, a plugin loaded from every
   `CLAUDE_PLUGIN_DIRS` entry, no `plugin_errors`), and its last stream-json
   event is a `result` with `is_error: false`; that event's `result` text is
   Claude's summary. It is killed (with its process group) after
   `INVESTIGATION_TIMEOUT`, or when heinzel shuts down. When it exits,
   whatever it left running in its process group is killed too.
6. **Commits.** heinzel fetches all of the work clone's branches into the
   mirror. If a branch other than the investigation branch holds commits
   that neither the mirror nor the investigation branch has (say auto-task
   made its own branch and never merged it back, or Claude renamed the
   investigation branch), the run fails as stage `claude` and names those
   branches. Otherwise heinzel counts the branch's commits on top of the
   default branch. None, or an investigation branch Claude deleted, means
   NoChanges.
7. **CI check.** If the branch changes CI configuration (anything under
   `.github/`, `.circleci/` or `.buildkite/`, or `.gitlab-ci.yml`,
   `Jenkinsfile`, `azure-pipelines.yml`, `.travis.yml` or
   `bitbucket-pipelines.yml` at the repo root), heinzel doesn't push it: the
   run fails as stage `push` with the reason "changes CI configuration".
8. **Push and PR.** heinzel pushes the branch from the mirror with its own
   token, then opens a draft PR against the default branch:
   `POST /repos/{owner}/{repo}/pulls` with title
   `Fix Sentry issue <issueId>: <alert title>` (at most 200 runes) and a body
   that links the Sentry issue and holds Claude's summary (at most 60000
   bytes in all; only the summary is cut, at a rune boundary, closing a
   ```` ``` ```` or `~~~` code fence the cut leaves open with the same
   characters, so the footer always survives). Every `@` in alert or Claude text gets a zero-width space after
   it, so neither can @mention anyone. A non-201 answer is a failure.
9. The run dir is always removed, read-only dirs in it included.

Every investigation ends in one recorded outcome, and a failure never stops
the handler or later alerts. Each outcome is logged as one line,
`sentry issue investigation finished`, with `handler`, `issueId`, `outcome`
(`pr_opened`, `no_changes`, `skipped` or `failed`) and `prUrl`, `reason` or
`stage`. Runs go one at a time and block only this handler's loop.

**One investigation per issue.** All events of an issue go on stream
`sentry-issue-investigation-<issueId>` (the id as-is if it is
`[A-Za-z0-9_-]+`, otherwise its hex sha256; the branch name uses the same
key). The first event, Started or Skipped, is appended at expected version 0,
so a repeated alert (a redelivery, or the rule firing again) hits a version
conflict, is logged as `duplicate` and ignored, even across restarts. The
outcome follows at expected version 1. Failed is final: there's no retry. If
heinzel crashes mid-run, the issue stays Started and isn't retried.

An alert the handler picks up after shutdown has begun (between the
automation's context being cancelled and the event handlers stopping) gets
no Started event and no investigation: it's logged and left unhandled, so
it isn't marked Failed for good. The handler runner still checkpoints past
it, though, so a restart doesn't pick it up again; only a later alert for
the same issue (the rule firing again) starts the investigation.

Event payloads (persisted, so the shapes are locked):

| Event | Payload |
| --- | --- |
| `SentryIssueInvestigationStarted` | `{"issueId", "project", "repo": "owner/repo", "branch"}` |
| `SentryIssueInvestigationPullRequestOpened` | `{"issueId", "prUrl"}` |
| `SentryIssueInvestigationNoChanges` | `{"issueId"}` |
| `SentryIssueInvestigationSkipped` | `{"issueId", "reason"}`, reason `unmapped` or `stale` |
| `SentryIssueInvestigationFailed` | `{"issueId", "stage", "reason"}`, stage `clone`, `claude`, `timeout`, `push`, `pr` or `shutdown` |

#### Configuration

The slice reads these when it is built. Any invalid value fails startup with
"invalid configuration" (exit 2).

| Variable | Required | Format | Default |
| --- | --- | --- | --- |
| `SENTRY_GITHUB_REPOS` | yes | `<projectId>=<owner>/<repo>`, comma-separated; ids are integers, each mapped once | |
| `GITHUB_TOKEN` | yes | token that can push branches and open PRs on the mapped repos; must not have the `workflow` scope (see [CI](#ci)) | |
| `ANTHROPIC_API_KEY` | one of these two, outside Bedrock mode | API key, passed to the `claude` child | |
| `CLAUDE_CODE_OAUTH_TOKEN` | one of these two, outside Bedrock mode | subscription token from `claude setup-token`, passed to the `claude` child instead of an API key; setting both is invalid | |
| `OPENAI_API_KEY` | yes, outside Bedrock mode | passed to the `claude` child, for Codex | |
| `CLAUDE_CODE_USE_BEDROCK` | no | `1` turns on [Bedrock mode](#bedrock-mode); unset or empty means off, and then the Bedrock variables below are ignored; any other value is invalid | |
| `CLAUDE_PLUGIN_DIRS` | yes | comma-separated plugin dirs (the Codex and auto-task plugins), each passed as `--plugin-dir` | |
| `INVESTIGATION_TIMEOUT` | no | positive Go duration | `30m` |
| `INVESTIGATION_MAX_AGE` | no | positive Go duration | `1h` |
| `CLAUDE_BIN` | no | path or name of the `claude` executable | `claude` |
| `INVESTIGATION_WORK_DIR` | no | directory for the per-run dirs, created (0700) if missing; the child may read only its own run's clone, HOME and Claude config dir in it | `heinzel-investigations-<uid>` in the OS temp dir (`heinzel-investigations` on Windows) |
| `INVESTIGATION_DENY_READ` | no | comma-separated absolute paths the child may not read, such as heinzel's data dir | |
| `GITHUB_API_URL` | no | absolute `http` or `https` URL of the GitHub REST API, without credentials, query or fragment; `GITHUB_TOKEN` goes to it, so use `http` only for a local test server | `https://api.github.com` |
| `GITHUB_CLONE_URL` | no | clone base ending in `/`: a URL or a local dir; repos are cloned (and pushed) at `<base><owner>/<repo>.git`; `GITHUB_TOKEN` goes to it, so use `http` or another host only for a local test remote | `https://github.com/` |

#### Bedrock mode

With `CLAUDE_CODE_USE_BEDROCK=1`, Claude Code and the Codex it calls both use
Amazon Bedrock with heinzel's AWS credentials, so no Anthropic or OpenAI key
is needed. `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` and
`OPENAI_API_KEY` must then be unset (or empty); setting any of them is
invalid. Bedrock mode reads:

| Variable | Required | Format | Default |
| --- | --- | --- | --- |
| `AWS_REGION` | yes | AWS region (lowercase, hyphenated, ending in a digit), Claude Code's Bedrock region | |
| `ANTHROPIC_MODEL` | yes | Bedrock model or inference-profile id (`[A-Za-z0-9._:/-]`), e.g. `global.anthropic.claude-sonnet-4-5-20250929-v1:0` | |
| `ANTHROPIC_SMALL_FAST_MODEL` | no | the model id Claude Code uses for background tasks; deprecated by Claude Code in favour of `ANTHROPIC_DEFAULT_HAIKU_MODEL` but still honoured | `ANTHROPIC_MODEL` |
| `CODEX_BEDROCK_MODEL` | yes | Codex's Bedrock model id, a plain regional id such as `openai.gpt-oss-120b` (Mantle has no cross-region inference) | |
| `CODEX_BEDROCK_REGION` | no | AWS region of Codex's Bedrock endpoint | `AWS_REGION` |

**Credentials.** heinzel resolves AWS credentials through the AWS SDK for
Go's default chain: its own process environment (`AWS_ACCESS_KEY_ID`, …,
`AWS_PROFILE`), the shared config and credentials files, or the ECS task
role's endpoint. The SDK reads these itself, not through the slice's
config. Credentials are cached, and each run gets them before the child
starts: credentials that would expire within `INVESTIGATION_TIMEOUT` are
refetched first, so a run never starts on credentials that could lapse
before it ends. That holds for env, profile/SSO and ECS container
credentials. With the EC2 instance-metadata provider the SDK adjusts
expiries itself: it skips the window when under 15 minutes remain,
extends credentials when a refresh fails, and caps them at an hour. So runs
may start on shorter-lived credentials, and with an `INVESTIGATION_TIMEOUT`
of an hour or more every run fails, with the hour cap reported as the
expiry; on ECS, use a task role. If freshly fetched credentials still
expire too soon, or none can be had within a minute, the run fails as
stage `claude` without starting the child; the reason names the expiry and
the timeout, or the SDK's error. Credentials without an expiry (long-term
keys) always pass. Unverified: whether the ECS credential endpoint hands
out new task-role credentials more than `INVESTIGATION_TIMEOUT` before the
old ones expire. If it doesn't, runs fail as stage `claude` in that window
of each rotation; a shorter timeout narrows it.
The child gets them as plain env vars, so it never needs heinzel's `HOME`
or a credential endpoint.

**Codex config.** Before each run, heinzel writes the child's
`HOME/.codex/config.toml` (dir 0700, file 0600), which selects Codex's
built-in `amazon-bedrock` provider (Bedrock's OpenAI-compatible Mantle
endpoint):

```toml
model = '<CODEX_BEDROCK_MODEL>'
model_provider = 'amazon-bedrock'

[model_providers]
[model_providers.amazon-bedrock]
[model_providers.amazon-bedrock.aws]
region = '<CODEX_BEDROCK_REGION>'
```

The region must be in the file: the child's `AWS_REGION` is Claude's, and
Codex reads the file's region first (it falls back to `AWS_REGION` only
without one). Codex signs its requests with the child's AWS env
credentials.

The child's environment, `allowedDomains` and the AWS permissions this mode
needs are under [Isolation](#isolation) and [AWS needs](#aws-needs).

#### Paths the child may not read

heinzel's own `HOME` is also denied to the child. Neither the work dir nor a
plugin dir may lie inside `HOME` or an `INVESTIGATION_DENY_READ` path, and no
denied path (the work dir included) may lie inside a plugin dir or the dir of
the `claude` binary; startup fails otherwise. So keep the plugins outside
`HOME`, e.g. in `/opt/heinzel/plugins`.

The same goes for tools on `PATH`: sandboxed commands can't run anything
from a `PATH` entry that lies, as given or with symlinks resolved, inside
`HOME`, an `INVESTIGATION_DENY_READ` path or the work dir, unless it is also
inside a plugin dir or the `claude` binary's dir. Startup logs one warning
per such entry ("tools there won't run inside the sandbox") but doesn't
fail. Install `git`, `codex` and the like outside `HOME`, e.g. in
`/usr/local/bin`.

On Unix, the work dir must be a real directory (not a symlink), owned by
heinzel's user, with no group or other permissions (`mode & 0o077 == 0`).
If it already exists at startup and fails this check, startup fails with
"invalid configuration". It is checked again at the start of each run;
there a failure fails the run as stage `clone` and names the problem. This keeps another user of a shared temp dir from creating the
dir first to read or plant run dirs. An existing dir is never chmodded.

#### Host prerequisites

The host (the binary ships as linux-arm64) needs on `PATH`:

- `git`
- `claude` (Claude Code; checked against 2.1.317, the version Bedrock
  mode's `--bare`, probe and alias behaviour were verified on)
- `codex` (the Codex CLI, used by the Codex plugin; Bedrock mode was
  verified on 0.165.0, so use that or later)
- `node`, which the Codex plugin's scripts run on
- on Linux, `bubblewrap` and `socat` for Claude Code's sandbox. The settings
  set `sandbox.failIfUnavailable`, so a run fails rather than going
  unsandboxed when they are missing.
- the target repo's toolchain (`go` for a Go repo), outside `HOME` and
  working offline. The child's env is an exact allowlist, so a variable like
  `GOTOOLCHAIN=local` set in heinzel's env never reaches the child's
  `go test`; set it in `$GOROOT/go.env` instead, or Go tries to download
  the toolchain the repo's `go.mod` asks for. The sandbox has no module proxy
  access either, so a target repo's tests can only use modules already
  present (vendored, or in the toolchain).

It also needs the Codex and auto-task plugins unpacked in the dirs named by
`CLAUDE_PLUGIN_DIRS`, outside heinzel's `HOME`.

Run heinzel as a dedicated OS user (or in a container) that owns nothing
else. The sandbox narrows what the child can read and write, but the child
still runs as heinzel's user.

#### Isolation

heinzel runs, in the work clone, with the prompt on stdin (`-p` reads it
there when argv has none):

```
claude --bare -p --verbose --output-format stream-json --no-session-persistence \
  --setting-sources "" --settings '<generated JSON>' \
  --permission-mode acceptEdits --permission-prompts none \
  --plugin-dir <each CLAUDE_PLUGIN_DIRS entry>
```

With `CLAUDE_CODE_OAUTH_TOKEN`, `--bare` is replaced by `--strict-mcp-config`:
`--bare` never reads an OAuth token. Bedrock mode keeps `--bare` and every
other flag of API-key mode: checked on Claude Code 2.1.317 against a local
Bedrock stub (`ANTHROPIC_BEDROCK_BASE_URL`, dummy credentials), `--bare`
still sends SigV4-signed Bedrock requests (see
[Bedrock stub checks](#bedrock-stub-checks)).

The run dir holds `repo` (the work clone), `home` and `claude-config` (the
child's `HOME` and `CLAUDE_CONFIG_DIR`), `mirror.git` and `git-home` (the
`HOME` of heinzel's own git commands).

- **Environment.** The child's environment is exactly `PATH`, `HOME` and
  `CLAUDE_CONFIG_DIR` (fresh, empty dirs in the run dir, outside the clone),
  `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` (whichever is set) and
  `OPENAI_API_KEY`. In Bedrock mode it is exactly `PATH`, `HOME`,
  `CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_USE_BEDROCK=1`, `AWS_REGION`,
  `ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`,
  `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL` and
  `ANTHROPIC_DEFAULT_FABLE_MODEL` (all `ANTHROPIC_MODEL`),
  `ANTHROPIC_DEFAULT_HAIKU_MODEL`
  (`ANTHROPIC_SMALL_FAST_MODEL`), `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY`, plus `AWS_SESSION_TOKEN` only when the
  credentials have one. It never sees `GITHUB_TOKEN`,
  `SENTRY_CLIENT_SECRET` or any other variable of heinzel's.
- **No ambient config.** `--bare` skips CLAUDE.md discovery, hooks, installed
  plugins and MCP servers; `--setting-sources ""` loads no user, project or
  local settings files, so the cloned repo's `.claude/` can't change the
  run. Only the generated settings and the given plugins apply.
  With an OAuth token there is no `--bare`. `--strict-mcp-config` still skips
  MCP servers (the clone's `.mcp.json` too), `--setting-sources ""` still
  skips settings files, and the fresh `CLAUDE_CONFIG_DIR` has no installed
  plugins. But the clone's CLAUDE.md files are read, and Claude Code's own
  background features (auto-memory, attribution, LSP) stay on; the sandbox
  bounds what they can touch.
- **Sandbox.** The generated settings are (`<run>` is the run dir, `<work>`
  the work dir, both with symlinks resolved):

  ```json
  {
    "permissions": {
      "defaultMode": "acceptEdits",
      "disableBypassPermissionsMode": "disable",
      "deny": [
        "Read(//proc/**)",
        "Read(/<heinzel's HOME>/**)",
        "Read(/<each INVESTIGATION_DENY_READ path>/**)",
        "Read(/<each other entry in <work>>/**)",
        "Read(/<run>/mirror.git/**)",
        "Read(/<run>/git-home/**)"
      ]
    },
    "sandbox": {
      "enabled": true,
      "failIfUnavailable": true,
      "autoAllowBashIfSandboxed": true,
      "allowUnsandboxedCommands": false,
      "filesystem": {
        "allowWrite": ["<run>/repo", "<run>/home"],
        "denyRead": ["<heinzel's HOME>", "<each INVESTIGATION_DENY_READ path>", "<work>"],
        "allowRead": ["<run>/repo", "<run>/home", "<run>/claude-config", "<each plugin dir>", "<dir of the claude binary>"]
      },
      "network": {"allowedDomains": ["api.openai.com"]}
    }
  }
  ```

  In Bedrock mode, `allowedDomains` is instead
  `["bedrock-mantle.<CODEX_BEDROCK_REGION>.api.aws"]`, the one host Codex's
  `amazon-bedrock` provider calls (per the Codex Bedrock docs and the stub
  check), and not `api.openai.com`. Claude Code's own Bedrock calls come
  from the `claude` process, outside the sandbox, so they need no entry.

  Shell commands run in the sandbox, which refuses to start unsandboxed and
  never retries a command outside it. They may write only to the work clone
  and the child's `HOME` (for Codex and tool caches; heinzel's git never uses
  it), plus the sandbox's own per-user temp dir, which Claude Code always
  allows, and reach only the OpenAI API (in Bedrock mode, Bedrock's Mantle
  endpoint), for Codex. They may not read
  heinzel's `HOME`, the `INVESTIGATION_DENY_READ` paths or the work dir
  (`HOME` and each `INVESTIGATION_DENY_READ` path are listed both as given
  and, when it differs, with symlinks resolved, in `denyRead` and in the
  `Read` deny rules alike); the
  narrower `allowRead` entries re-open the clone, the child's `HOME` and
  `CLAUDE_CONFIG_DIR` (shell snapshots, plugin data), the
  plugin dirs and the claude binary's dir (a narrower allow wins over a
  broader deny, never the other way round). Claude's file tools aren't
  sandboxed: `Read` deny rules keep them out of the same places, and since
  those rules can't be carved out, the work dir is denied piecemeal (every
  entry but this run's dir, plus the mirror and `git-home` in it).
  `acceptEdits` auto-approves edits only inside the working directory, and
  with `--permission-prompts none` anything else that would prompt is
  denied. Bypass mode is disabled. `-p` silently ignores invalid settings,
  so heinzel checks the `system/init` event (see step 5); that event doesn't
  report the sandbox itself.
- **The prompt** starts with `/at:auto-task` (bare mode doesn't list skills,
  so the command is named explicitly). It overrides auto-task's own setup:
  every step runs in place in the current directory, with no git worktree
  and no new or switched branch, committing only on the investigation
  branch, and auto-task's ship and hand-off steps are skipped (no push,
  merge, PR or `/at:ship-task`). It asks for a closing summary. The issue
  details follow as JSON data (title capped at 2000 runes), marked as not
  instructions.
- **The token stays with heinzel.** git gets the token only through
  `GIT_CONFIG_*` environment variables (`http.extraHeader`), never in argv, a
  remote URL or a config file. Claude's clone has no GitHub remote and no
  token. heinzel pushes from its own mirror, with hooks off, after fetching
  only the clone's branches out of it, so changes Claude makes to the
  clone's `.git` (hooks, config, remotes) never run with the token. heinzel's
  git runs with its own `HOME` (`git-home`, which the child can neither read
  nor write), `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1`, so a
  `.gitconfig` Claude plants in its `HOME` (say a `url.<x>.insteadOf`)
  doesn't reach the push.
- **Accepted exposure.** Claude can read `OPENAI_API_KEY`, which Codex needs;
  use a dedicated, low-limit key. In Bedrock mode it can read the AWS
  credentials instead (sandboxed commands see the child's env), so scope the
  role to Bedrock invoke only, as [AWS needs](#aws-needs) lists, and nothing
  else. The child runs as heinzel's OS user and
  can read whatever that user can outside the denied paths, and could
  inspect heinzel's processes; run heinzel as a dedicated user or in a
  container.

#### AWS needs

Exactly what Claude Code and Codex need in Bedrock mode, checked against
the AWS Service Authorization Reference, the Bedrock user guide, the Claude
Code and Codex Bedrock docs, and the stub checks below. 016.03 grants
exactly this. `<region>` is `AWS_REGION`, `<codex region>`
`CODEX_BEDROCK_REGION`, `<account>` the AWS account.

- **Claude Code, invoke:** `bedrock:InvokeModel` and
  `bedrock:InvokeModelWithResponseStream` on `ANTHROPIC_MODEL` and
  `ANTHROPIC_SMALL_FAST_MODEL`. Apart from the startup probes (see expected
  denials), only those two model ids are invoked by name: the child's
  `opus`, `sonnet`, `fable` and `haiku` aliases are pinned to them, so a subagent or
  skill with `model: sonnet` (such as the Codex plugin's `codex-rescue`)
  stays on `ANTHROPIC_MODEL` (stub check (d)). The resources, by id form:
  - a foundation-model id: `arn:aws:bedrock:<region>::foundation-model/<id>`;
  - a geo inference profile (`us.`, `eu.`, `apac.`, …):
    `arn:aws:bedrock:<region>:<account>:inference-profile/<profile id>`
    plus `arn:aws:bedrock:<each region it routes to>::foundation-model/<model id>`;
  - a global profile (`global.`):
    `arn:aws:bedrock:<region>:<account>:inference-profile/global.<model id>`,
    `arn:aws:bedrock:<region>::foundation-model/<model id>` and the
    region-less `arn:aws:bedrock:::foundation-model/<model id>`, which AWS
    requires for global routing (it is evaluated with
    `aws:RequestedRegion` = `unspecified`). The foundation-model
    statements can be conditioned on `bedrock:InferenceProfileArn`.
- **Claude Code, profile lookup:** `bedrock:ListInferenceProfiles` on
  `*`, since it supports no resource type, and `bedrock:GetInferenceProfile`
  on the configured `inference-profile/<id>` ARNs (Claude Code calls
  `GET /inference-profiles?type=SYSTEM_DEFINED` at startup). Whether a
  denied lookup would block a run is unverified, so grant both.
- **Claude Code, hosts:** `bedrock-runtime.<region>.amazonaws.com`
  (invoke) and `bedrock.<region>.amazonaws.com` (profile lookup), from the
  `claude` process itself.
- **Codex:** `bedrock-mantle:CreateInference` on
  `arn:aws:bedrock-mantle:<codex region>:<account>:project/*`, conditioned on
  `bedrock-mantle:Model` = `CODEX_BEDROCK_MODEL`. The stub check saw Codex
  call only `POST /openai/v1/responses`, signed for service
  `bedrock-mantle`, and no `/models`, so no `ListModels`/`GetModel`.
  Host: `bedrock-mantle.<codex region>.api.aws`, from sandboxed commands.
  Only a real run settles whether Codex needs anything more.
- **Not granted:** no `aws-marketplace:*` actions and nothing else. Enabling
  model access is a one-time account setup, not a runtime need.
- **Expected denials.** At startup Claude Code also probes `InvokeModel` on
  about ten other Claude models (the `<region>`-geo profiles of other Opus,
  Sonnet and Haiku versions). The role doesn't grant them, so they are
  denied, and the run carries on with `ANTHROPIC_MODEL` (stub check (b)).
  So the child does call models the role doesn't grant; they are just
  refused. Codex likewise tries `chatgpt.com` and `github.com`, which the
  sandbox's network allowlist refuses.

#### Bedrock stub checks

Run without an AWS account, with dummy credentials and a local stub
(results from 2026-11-03):

- **(a) `--bare` on Bedrock** (Claude Code 2.1.317, `ANTHROPIC_BEDROCK_BASE_URL`
  at the stub, `--bare -p` and the other API-key-mode flags). Requests
  arrive SigV4-signed (scope `<AWS_REGION>/bedrock`, with
  `X-Amz-Security-Token`): `GET /inference-profiles?type=SYSTEM_DEFINED`,
  then `POST /model/<id>/invoke` probes, then
  `POST /model/<ANTHROPIC_MODEL>/invoke-with-response-stream`.
- **(b) Startup probes.** With the stub answering 403
  `AccessDeniedException` for every model but `ANTHROPIC_MODEL`, Claude Code
  probed 11 other models (`eu.anthropic.claude-opus-5-5`,
  `…-haiku-4-5-…`, `…-opus-4-x…`, `…-3-5-haiku-…`, `…-sonnet-4-5-…`), got
  403s, and still sent the prompt to `ANTHROPIC_MODEL`. The probes don't
  block the run.
- **(c) Codex** (0.165.0, `codex exec`). With the generated config plus a
  `base_url` override at the stub: only `POST /openai/v1/responses`, signed
  with scope `<CODEX_BEDROCK_REGION>/bedrock-mantle` even though the env's
  `AWS_REGION` differed; without the file's region it used `AWS_REGION`.
  With the exact generated config and an HTTPS proxy at the stub, Codex
  connected to `bedrock-mantle.<CODEX_BEDROCK_REGION>.api.aws:443`.
- **(d) Model aliases** (Claude Code 2.1.317, an agent defined with
  `model: sonnet` run via `--agents` and `--agent`, as (a) otherwise).
  Without the `ANTHROPIC_DEFAULT_*_MODEL` vars its requests went to
  `eu.anthropic.claude-sonnet-4-5-20250929-v1:0`, Claude Code's own default
  for the alias, not `ANTHROPIC_MODEL`
  (`global.anthropic.claude-sonnet-4-5-20250929-v1:0`); with them they went
  to `ANTHROPIC_MODEL`.

#### CI

A branch heinzel pushes to the repo can trigger the repo's CI, which may run
with the repo's secrets. heinzel refuses to push a branch that changes CI
configuration (step 7), and `GITHUB_TOKEN` must not have the `workflow`
scope, so GitHub itself rejects a push that touches `.github/workflows/`.
The residual risk is build scripts that existing push-triggered CI runs
(say a `Makefile` or `package.json` script Claude edited): they run with
whatever secrets that CI exposes to pushes. Limit secrets to protected
branches or environments. The follow-up is to push to a fork and open the PR
from there, so the branch never runs the upstream repo's push CI.

## Container image

`deploy/image/Dockerfile` builds a linux/arm64 image that runs heinzel in
[Bedrock mode](#bedrock-mode) with every [host
prerequisite](#host-prerequisites). The Dockerfile is the one place
versions are pinned: base `node:22.23.4-bookworm-slim` pinned by its
multi-arch index digest, Claude Code 2.1.317 (the native binary,
sha256-checked against the release manifest), Codex 0.165.0 (npm), Go 1.27.3
(sha256-checked), bubblewrap 0.8.0 (build arg `BWRAP_VERSION`) and Debian's
`git`, `socat` and `curl`. The build asserts each tool's version, so it fails
if apt installs a bubblewrap other than the one the [sandbox
probe](#sandbox-probe) result holds for, and that `go env GOTOOLCHAIN` prints
`local` with an empty env.

- **User.** `heinzel`, uid/gid 10001, `HOME=/home/heinzel`. Every tool lives
  outside `HOME`: `/usr/local/bin` (`heinzel`, `claude`, `codex`,
  `heinzel-sandbox-probe`) and `/usr/local/go`, with `GOTOOLCHAIN=local` in
  `/usr/local/go/go.env`.
- **Paths.** `/var/lib/heinzel` (root, 0755) holds `data` (the event store)
  and `work` (the per-run dirs), both heinzel's and 0700. The Codex and
  auto-task plugins are in `/opt/heinzel/plugins/{codex,at}`, root-owned and
  world-readable.
- **ENV.** `HOME`, `PATH` (`/usr/local/go/bin` first, nothing under `HOME`),
  `INVESTIGATION_WORK_DIR=/var/lib/heinzel/work`,
  `INVESTIGATION_DENY_READ=/var/lib/heinzel/data` (exactly the data dir: a
  parent would contain the work dir and fail startup),
  `CLAUDE_PLUGIN_DIRS=/opt/heinzel/plugins/codex,/opt/heinzel/plugins/at`,
  `CLAUDE_CODE_USE_BEDROCK=1` and `GOTOOLCHAIN=local`. A deployment adds only
  `SENTRY_GITHUB_REPOS`, `GITHUB_TOKEN`, `SENTRY_CLIENT_SECRET`,
  `AWS_REGION`, `ANTHROPIC_MODEL`, `CODEX_BEDROCK_MODEL` and, optionally,
  the other Bedrock and investigation variables. Startup logs no "won't run
  inside the sandbox" warning.
- **Labels.** `heinzel.claude`, `heinzel.codex`, `heinzel.go`,
  `heinzel.bwrap`, `heinzel.plugins.codex` and `heinzel.plugins.at`, each the version inside.
- **Runtime.** `USER heinzel`, `EXPOSE 8080`, no entrypoint and
  `CMD ["heinzel","-addr",":8080","-data-dir","/var/lib/heinzel/data"]`, so a
  probe task only overrides the command with `["heinzel-sandbox-probe"]`.
  `HEALTHCHECK` runs `curl -fsS http://127.0.0.1:8080/healthz` every 10s
  (timeout 3s, start period 15s, 3 retries).
- **Writable root.** Don't set `readonlyRootFilesystem`: `/tmp`, `HOME`,
  `/var/lib/heinzel/data` and `/var/lib/heinzel/work` must be writable.

[infra/README.md](../infra/README.md) deploys the image to ECS Fargate
behind an HTTPS ALB with OpenTofu, including a Fargate run of the probe.

Make targets (all but `image-context` need Docker):

- `make image-context` stages the build context in `bin/image`: the
  linux-arm64 binary, the probe, and the newest version dir (`sort -V`, the
  system test's rule) of `openai-codex/codex` and `auto-task/at` under
  `PLUGIN_CACHE` (default `~/.claude/plugins/cache`), without their
  `.in_use/`. It records the two versions in `bin/image/plugin-versions.mk`.
- `make image` builds `$(IMAGE):$(IMAGE_TAG)` (defaults `heinzel` and the
  12-character `HEAD` commit, with a `-dirty` suffix when `git status
  --porcelain` shows uncommitted or untracked changes), passing the plugin
  versions as the required build args `CODEX_PLUGIN_VERSION` and
  `AT_PLUGIN_VERSION`.
- `make image-push ECR_REPOSITORY_URL=<account>.dkr.ecr.<region>.amazonaws.com/<repo>`
  logs in to that registry (the region comes from its host) with
  `aws ecr get-login-password`, tags and pushes `<url>:$(IMAGE_TAG)` and
  prints the ref. Without the variable it prints a usage message and fails.
  It refuses a `-dirty` tag unless `IMAGE_TAG` was set explicitly (on the
  command line or in the environment).
- `make image-smoke` builds, then runs `deploy/image/smoke.sh`.

### Sandbox probe

`heinzel-sandbox-probe` checks whether a container can run Claude Code's
sandbox. It fails when run as root or when `bwrap` or `socat` is missing.
Otherwise it runs, with a fresh `mktemp -d` dir `$d`, a bwrap argv that
mirrors claude 2.1.317's Linux sandbox invocation (namespaces, mounts,
userns disable), with a `--tmpfs` over `HOME` standing in for a deny-read
path:

```
bwrap --new-session --die-with-parent --unshare-user --disable-userns --unshare-pid --unshare-net \
  --ro-bind / / --bind "$d" "$d" --tmpfs "$HOME" --dev /dev --proc /proc --cap-drop ALL \
  -- bash -c '<checks>'
```

Inside, it checks that a write to `$d` succeeds; that writes to `/` and to
`$d`'s parent (`/tmp`, writable outside) fail; that `HOME` is a tmpfs and
empty; that the pid and net namespaces differ from the outer ones; that
`/proc` shows at most 3 pids; that `/proc/net/route` has no routes; and that
a nested `unshare -U` fails. bwrap's stderr passes through.
The last line is `sandbox probe: PASS` (exit 0) or
`sandbox probe: FAIL: <reason>` (exit 1). It checks namespaces and mounts
only: Claude Code's own sandbox starts only inside a model turn, which dummy
credentials can't produce.

A new net namespace isn't always just `lo`: on Docker Desktop's kernel it
also holds down fallback tunnel devices (`tunl0`, `gre0`, ...), so the probe
compares namespace ids and routes rather than listing interfaces.

### Smoke

`deploy/image/smoke.sh <image>` runs the image with `--network none
--cap-add SYS_PTRACE` and nothing else (no `--privileged`, no
`--security-opt`), the settings Fargate allows, and dummy config:
`SENTRY_GITHUB_REPOS=1=acme/app`, dummy `GITHUB_TOKEN`,
`SENTRY_CLIENT_SECRET`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`,
`AWS_REGION=eu-central-1`,
`ANTHROPIC_MODEL=global.anthropic.claude-sonnet-4-5-20250929-v1:0` and
`CODEX_BEDROCK_MODEL=openai.gpt-oss-120b`. It prints one PASS/FAIL line per
check:

1. `"msg":"app started"` is logged within 30s, with no "won't run inside
   the sandbox" line;
2. `GET /healthz` (via `docker exec … curl`) returns 200;
3. `heinzel-sandbox-probe` passes.

Checks 1–2 failing exit 1. A probe failure prints a warning that Fargate
likely can't run the sandbox and exits 0, unless `SMOKE_REQUIRE_PROBE=1`.
The container is always removed.

Recorded result (Docker 28.5.1, arm64, bubblewrap 0.8.0): checks 1–2 pass;
the probe fails with `bwrap: No permissions to create new namespace`, the
early warning that Fargate's settings can't run the sandbox. Under looser
local settings the probe gives:

| Docker settings | Probe |
| --- | --- |
| Default seccomp, with `SYS_PTRACE` (the smoke) | FAIL: `No permissions to create new namespace` |
| `seccomp=unconfined` | FAIL: `Can't mount proc on /newroot/proc` |
| `seccomp=unconfined` + `systempaths=unconfined` | PASS |

So even ECS on EC2 would need unconfined seccomp plus an unmasked `/proc`,
or Claude Code's `enableWeakerNestedSandbox` (`--bind /proc /proc`), which
lets sandboxed commands read `/proc/<heinzel pid>/environ`, `GITHUB_TOKEN`
included. That's a security decision for 016.03/016.05; the image enables
neither.

## Adding a slice

- Put the slice in its own package, `internal/slices/<slice>/`, named after its
  command or automation (`record…`, `investigate…`). One package holds exactly one
  slice, with everything it needs: its route or event handler, command
  handling, config, domain logic, infrastructure adapters and tests.
  Sub-packages (say for git or HTTP mechanics) belong to the slice.
- Expose a constructor for the slice's kind:
  - A state change has `New(...) app.StateChange`: a struct with the route's
    `Method` and Gin `Path` (e.g. `http.MethodPost` and `/sentry/callback`,
    or `/notes/:id` with a path parameter) and a
    `Handler func(store *eventstore.Store) gin.HandlerFunc`, which the app
    calls once at startup. The app's router answers 404 for an unknown path,
    405 for a known path with another method, and never redirects a
    trailing slash. Test a route through `app.Router(store, stateChanges)`,
    which builds the same Gin router `app.Run` serves, rather than building
    Gin in the slice.
  - An automation has `New(...) app.Automation`: a struct with a
    `Handler func(ctx context.Context, store *eventstore.Store) eventhandler.Handler`,
    which the app calls once at startup, after opening the store. `ctx` is
    cancelled when shutdown begins, after HTTP requests drain and before the
    event handlers are stopped, so a long-running `Handle` can abort.
- A slice loads its own config inside its constructor. A slice with config
  takes the env lookup as its first argument and also returns an error, e.g.
  `New(getenv func(string) string, …) (app.StateChange, error)`. It reads its
  settings from the lookup it's given and returns an error if they are invalid.
  A slice with many dependencies may take a params struct holding `Getenv`
  instead, like `investigatesentryissue.New(Params)`. A slice with no config
  keeps the plain constructor.
- Register the slice in an `init()` in its package, with a factory that calls
  `New` with what it needs from `app.Deps` (`Getenv`, `Logger`):
  `app.RegisterStateChange` for a state change, `app.RegisterAutomation`
  for an automation. At startup `main` calls `app.Build` with `os.Getenv`
  and its logger, and exits with "invalid configuration" on the first factory
  error, so it never knows a slice or its settings.
  The register call may sit in a helper that `init()` calls, but registering
  directly in `init()` is recommended.
  `internal/slices/registration_test.go` fails, naming the folder, if a slice
  has no `func init()` or never calls `app.RegisterStateChange` or
  `app.RegisterAutomation`. It doesn't check that `init()` actually reaches
  a helper's register call.
- Run `go generate ./...`. It rewrites `internal/slices/slices_gen.go`, which
  blank-imports every slice directory so its `init()` runs; `main` imports only
  `internal/slices`. A slice directory is any subdirectory of `internal/slices`
  that holds a non-test `.go` file the current build includes (build tags and
  `_<os>`/`_<arch>` file suffixes apply), except `testdata` and names starting
  with `.` or `_`. `internal/slices/generated_test.go` fails until the
  generated file matches the directories.
- A slice (its sub-packages and tests included) may import only the stdlib,
  third-party libraries, `internal/events`, the shared platform packages
  under `internal/platform/` and its own packages. It never imports another
  slice, the `internal/slices` parent or any other package of this module.
  The platform packages never import a slice, so slices cannot share code
  through them. `internal/slices/imports_test.go` enforces both rules.
- Event contracts that another slice reads go in `internal/events`: a type-name
  constant, a payload struct, an `EventType()` method returning the constant
  and an entry in the `decoders` map. Slices append with `events.Encode` and
  read with `events.Decode`, never by pairing a type name and a payload by
  hand. The package has no other funcs or methods, and imports only the
  stdlib and `internal/platform/eventstore`. Their JSON is persisted, so
  treat it as locked.
