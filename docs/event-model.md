# Event Model

heinzel is organised as vertical slices, event-modeling style. Each slice reads as
**trigger → command → event → reaction**. Slices that belong to one feature live
together in one package under `internal/features/<name>/`.

## Sentry (`internal/features/sentry/`)

### State change: record issue alert

| Trigger | Command | Event |
| --- | --- | --- |
| `POST /sentry/callback` (Sentry internal-integration webhook) | Record issue alert | `SentryIssueAlertReceived` |

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

### Automation: log issue alert

| Event | Reaction |
| --- | --- |
| `SentryIssueAlertReceived` | `sentry-issue-alert-logger` writes one structured log line |

The log line holds `handler`, `position`, `issueId`, `title` and `sentryLevel`.
The alert level is logged as `sentryLevel` because `level` is the log record's
own level. The handler ignores all other event types. It logs a payload it
cannot decode as an error and skips it. Its name is its checkpoint key, so it
must never change.

## Adding a feature

- Put everything for the feature in one package, `internal/features/<name>/`:
  its HTTP routes, its command handling and its event handlers.
- Expose a constructor that returns an `agent.Feature`:
  `func(mux *http.ServeMux, store *eventstore.Store) []eventhandler.Handler`.
  It registers the routes on `mux` and returns the handlers to run. Pass it to
  `agent.Run` from `cmd/heinzel/main.go`, which also reads the feature's config.
- A feature may import only the stdlib, its own packages, `internal/events`,
  `internal/eventstore`, `internal/eventhandler` and `internal/agent`. It never
  imports another feature or a shared helper package.
  `internal/features/imports_test.go` enforces this.
- Event contracts that another feature reads go in `internal/events`: a type-name
  constant and a payload struct, with no funcs or methods. Their JSON is persisted,
  so treat it as locked.
