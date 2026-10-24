# Event Model

heinzel is organised as vertical slices, event-modeling style. A slice is
either a **state change** (trigger → command → event) or an **automation**
(event → reaction). A state change is an `agent.StateChange` (one HTTP route);
an automation is an `agent.Automation` (one event handler). Each slice is one
package, `internal/slices/<slice>/`.
Slices never import each other; they are coupled only through the events in
`internal/events`.

## Sentry

Two independent slices, linked by the `SentryIssueAlertReceived` event.

### State change: record Sentry issue alert (`internal/slices/recordsentryissuealert/`)

| Trigger | Command | Event |
| --- | --- | --- |
| `POST /sentry/callback` (Sentry internal-integration webhook) | Record Sentry issue alert | `SentryIssueAlertReceived` |

The slice also loads the client secret from `SENTRY_CLIENT_SECRET`.

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
  "triggeredRule": "Very Important Alert Rule!"
}
```

### Automation: handle Sentry issue alert (`internal/slices/handlesentryissuealert/`)

| Event | Reaction |
| --- | --- |
| `SentryIssueAlertReceived` | TBD; for now `sentry-issue-alert-logger` writes one structured log line |

The real reaction is not defined yet. Until it is, the log line is a
placeholder. It holds `handler`, `position`, `issueId`, `title` and
`sentryLevel`. The alert level is logged as `sentryLevel` because `level` is
the log record's own level. The handler ignores all other event types. It logs
a payload it cannot decode as an error and skips it. Its name is its checkpoint
key, so it must never change.

## Adding a slice

- Put the slice in its own package, `internal/slices/<slice>/`, named after its
  command or automation (`record…`, `handle…`). One package holds exactly one
  slice: its route and command handling, or its event handler.
- Expose a constructor for the slice's kind:
  - A state change has `New(...) agent.StateChange`: a struct with the route's
    `Pattern` (e.g. `POST /sentry/callback`) and a
    `Handler func(store *eventstore.Store) http.Handler`, which the agent calls
    once at startup.
  - An automation has `New(...) agent.Automation`, an alias for
    `eventhandler.Handler`.

  Add it to the matching list in the `agent.Slices` that
  `cmd/heinzel/main.go` passes to `agent.Run`. `main` also reads the slice's
  config.
- A slice may import only the stdlib, its own package, `internal/events`,
  `internal/eventstore`, `internal/eventhandler` and `internal/agent`. It never
  imports another slice or a shared helper package.
  `internal/slices/imports_test.go` enforces this.
- Event contracts that another slice reads go in `internal/events`: a type-name
  constant and a payload struct, with no funcs or methods. Their JSON is
  persisted, so treat it as locked.
