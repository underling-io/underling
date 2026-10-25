//! Given a Sentry webhook delivery, when it is posted to the callback, then
//! the reply and the events stored are these.

use std::io;
use std::sync::{Arc, Mutex};

use axum::body::Body;
use axum::extract::Request;
use axum::http::StatusCode;
use axum::http::header::CONTENT_TYPE;
use hmac::{Hmac, KeyInit, Mac};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tempfile::TempDir;

use super::callback::Callback;
use super::{Deps, new};
use crate::events::{self, SentryIssueAlertReceived};
use crate::eventstore::{Event, Store};

const SECRET: &str = "test-client-secret";

/// Shaped like Sentry's documented event_alert webhook.
const EVENT_ALERT: &str = include_str!("testdata/event_alert.json");

// Given

/// The slice's callback, backed by a fresh store, verifying with [`SECRET`].
struct Slice {
    store: Arc<Store>,
    callback: Callback,
    _dir: TempDir,
}

fn given_the_slice() -> Slice {
    let dir = TempDir::new().unwrap();
    let store = Arc::new(Store::open(dir.path()).unwrap());
    let callback = Callback::new(SECRET.into(), Arc::clone(&store));
    Slice {
        store,
        callback,
        _dir: dir,
    }
}

/// A webhook delivery. Headers left `None` are not sent.
struct Delivery {
    body: Vec<u8>,
    resource: Option<String>,
    signature: Option<String>,
}

/// A delivery of `body` with a valid signature and resource `event_alert`.
fn signed(body: impl Into<Vec<u8>>) -> Delivery {
    let body = body.into();
    Delivery {
        signature: Some(sign(SECRET, &body)),
        resource: Some("event_alert".into()),
        body,
    }
}

impl Delivery {
    fn with_signature(self, signature: Option<String>) -> Delivery {
        Delivery { signature, ..self }
    }

    fn with_resource(self, resource: Option<&str>) -> Delivery {
        Delivery {
            resource: resource.map(Into::into),
            ..self
        }
    }
}

fn sign(key: &str, body: impl AsRef<[u8]>) -> String {
    let mut mac = Hmac::<Sha256>::new_from_slice(key.as_bytes()).unwrap();
    mac.update(body.as_ref());
    hex::encode(mac.finalize().into_bytes())
}

/// The fixture with its event id and triggered rule replaced.
fn alert(event_id: &str, rule: &str) -> String {
    let mut body: Value = serde_json::from_str(EVENT_ALERT).unwrap();
    body["data"]["triggered_rule"] = json!(rule);
    body["data"]["event"]["event_id"] = json!(event_id);
    body.to_string()
}

/// A delivery body whose `data.event` is `event`, for rule `rule-a`.
fn alert_with_event(event: Value) -> String {
    json!({"data": {"event": event, "triggered_rule": "rule-a"}}).to_string()
}

// When

/// What the callback answered.
struct Reply {
    status: StatusCode,
    content_type: Option<String>,
    body: String,
}

impl Slice {
    async fn when_posted(&self, delivery: Delivery) -> Reply {
        let mut request = Request::post("/sentry/callback").header(CONTENT_TYPE, "application/json");
        if let Some(resource) = &delivery.resource {
            request = request.header("Sentry-Hook-Resource", resource);
        }
        if let Some(signature) = &delivery.signature {
            request = request.header("Sentry-Hook-Signature", signature);
        }
        let request = request.body(Body::from(delivery.body)).unwrap();

        let response = self.callback.clone().record(request).await;

        let status = response.status();
        let content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .map(|v| v.to_str().unwrap().to_owned());
        let body = axum::body::to_bytes(response.into_body(), usize::MAX)
            .await
            .unwrap();
        Reply {
            status,
            content_type,
            body: String::from_utf8(body.to_vec()).unwrap(),
        }
    }

    fn stored(&self) -> Vec<Event> {
        self.store.read_after(0, 1000).unwrap()
    }

    /// Posts a signed delivery of `body`, requires it to be recorded and
    /// returns the stored payload.
    async fn recorded_payload(&self, body: impl Into<Vec<u8>>) -> Value {
        self.when_posted(signed(body)).await.then_is(StatusCode::OK, RECORDED);
        let stored = self.stored();
        assert_eq!(stored.len(), 1, "stored {stored:?}");
        stored[0].payload.clone()
    }
}

// Then

const RECORDED: &str = r#"{"status":"recorded"}"#;
const DUPLICATE: &str = r#"{"status":"duplicate"}"#;
const IGNORED: &str = r#"{"status":"ignored"}"#;
const INVALID_SIGNATURE: &str = r#"{"error":"invalid signature"}"#;
const MALFORMED: &str = r#"{"error":"malformed event_alert payload"}"#;
const REQUIRED: &str = r#"{"error":"event_id, issue_id and triggered_rule are required"}"#;
const TOO_LARGE: &str = r#"{"error":"body too large"}"#;
const INTERNAL: &str = r#"{"error":"internal error"}"#;

impl Reply {
    /// Requires the reply to be `status` with the JSON `body`, written as
    /// Go's encoder does: `application/json` with a trailing newline.
    #[track_caller]
    fn then_is(&self, status: StatusCode, body: &str) {
        assert_eq!(
            (self.status, self.body.as_str()),
            (status, format!("{body}\n").as_str())
        );
        assert_eq!(self.content_type.as_deref(), Some("application/json"));
    }
}

impl Slice {
    #[track_caller]
    fn then_nothing_is_stored(&self) {
        assert_eq!(self.stored(), []);
    }
}

fn stream_of(event_id: &str, rule: &str) -> String {
    let hash = Sha256::digest(format!("{event_id}\0{rule}"));
    format!("sentry-issue-alert-{}", hex::encode(hash))
}

// Scenarios

#[tokio::test]
async fn a_signed_alert_is_recorded_on_its_own_stream() {
    let slice = given_the_slice();

    let reply = slice.when_posted(signed(EVENT_ALERT)).await;

    reply.then_is(StatusCode::OK, RECORDED);
    let stored = slice.stored();
    assert_eq!(stored.len(), 1);
    let event = &stored[0];
    assert_eq!(event.event_type, events::SENTRY_ISSUE_ALERT_RECEIVED);
    assert_eq!(
        event.stream_id,
        stream_of("aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4", "Very Important Alert Rule!")
    );
    assert_eq!(event.version, 1);
    assert_eq!(
        event.payload,
        json!({
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
        })
    );
    let typed: SentryIssueAlertReceived = serde_json::from_value(event.payload.clone()).unwrap();
    assert_eq!(typed.project, 1);
}

#[tokio::test]
async fn a_bad_signature_is_unauthorized_and_stores_nothing() {
    let valid = sign(SECRET, EVENT_ALERT);
    let cases = [
        ("missing", None),
        ("not hex", Some(format!("zz{}", &valid[2..]))),
        ("wrong secret", Some(sign("other-secret", EVENT_ALERT))),
        ("other body", Some(sign(SECRET, format!("{EVENT_ALERT} ")))),
        ("truncated", Some(valid[..62].to_owned())),
    ];
    for (name, signature) in cases {
        let slice = given_the_slice();

        let reply = slice
            .when_posted(signed(EVENT_ALERT).with_signature(signature))
            .await;

        assert_eq!(reply.status, StatusCode::UNAUTHORIZED, "{name}");
        reply.then_is(StatusCode::UNAUTHORIZED, INVALID_SIGNATURE);
        slice.then_nothing_is_stored();
    }
}

#[tokio::test]
async fn the_signature_is_checked_before_decoding() {
    let slice = given_the_slice();
    let malformed = r#"{"data":"#;

    let reply = slice
        .when_posted(signed(malformed).with_signature(Some(sign("other-secret", malformed))))
        .await;

    reply.then_is(StatusCode::UNAUTHORIZED, INVALID_SIGNATURE);
}

#[tokio::test]
async fn other_resources_are_acknowledged_and_store_nothing() {
    let resources = [
        None,
        Some(""),
        Some("issue"),
        Some("installation"),
        Some("error"),
        Some("EVENT_ALERT"),
        Some("event_alert "),
    ];
    for resource in resources {
        let slice = given_the_slice();

        let reply = slice
            .when_posted(signed(EVENT_ALERT).with_resource(resource))
            .await;

        assert_eq!(reply.status, StatusCode::OK, "{resource:?}");
        reply.then_is(StatusCode::OK, IGNORED);
        slice.then_nothing_is_stored();
    }
}

#[tokio::test]
async fn a_redelivered_alert_is_stored_once() {
    let slice = given_the_slice();

    let first = slice.when_posted(signed(alert("event-1", "rule-a"))).await;
    let second = slice.when_posted(signed(alert("event-1", "rule-a"))).await;

    first.then_is(StatusCode::OK, RECORDED);
    second.then_is(StatusCode::OK, DUPLICATE);
    assert_eq!(slice.stored().len(), 1);
}

#[tokio::test]
async fn the_same_event_triggering_another_rule_is_a_new_alert() {
    let slice = given_the_slice();

    let first = slice.when_posted(signed(alert("event-1", "rule-a"))).await;
    let second = slice.when_posted(signed(alert("event-1", "rule-b"))).await;

    first.then_is(StatusCode::OK, RECORDED);
    second.then_is(StatusCode::OK, RECORDED);
    let streams: Vec<String> = slice.stored().into_iter().map(|e| e.stream_id).collect();
    assert_eq!(
        streams,
        [stream_of("event-1", "rule-a"), stream_of("event-1", "rule-b")]
    );
}

#[tokio::test]
async fn ids_are_stored_as_sent() {
    let slice = given_the_slice();

    let payload = slice.recorded_payload(alert(" event-1 ", " rule-a ")).await;

    assert_eq!(payload["eventId"], " event-1 ");
    assert_eq!(payload["triggeredRule"], " rule-a ");
}

#[tokio::test]
async fn an_undecodable_alert_is_a_bad_request_and_stores_nothing() {
    let cases = [
        ("malformed JSON", r#"{"data":"#.to_owned()),
        ("empty body", String::new()),
        ("not an object", "[]".to_owned()),
        ("data not an object", r#"{"data":[]}"#.to_owned()),
        (
            "event not an object",
            json!({"data": {"event": ["e1", "1"], "triggered_rule": "rule-a"}}).to_string(),
        ),
        (
            "numeric issue id",
            alert_with_event(json!({"event_id": "e1", "issue_id": 1})),
        ),
        (
            "string project",
            alert_with_event(json!({"event_id": "e1", "issue_id": "1", "project": "1"})),
        ),
    ];
    for (name, body) in cases {
        let slice = given_the_slice();

        let reply = slice.when_posted(signed(body)).await;

        assert_eq!(reply.status, StatusCode::BAD_REQUEST, "{name}");
        reply.then_is(StatusCode::BAD_REQUEST, MALFORMED);
        slice.then_nothing_is_stored();
    }
}

#[tokio::test]
async fn an_alert_without_its_ids_is_a_bad_request_and_stores_nothing() {
    let cases = [
        ("missing event id", alert_with_event(json!({"issue_id": "1"}))),
        (
            "blank event id",
            alert_with_event(json!({"event_id": " \t", "issue_id": "1"})),
        ),
        ("missing issue id", alert_with_event(json!({"event_id": "e1"}))),
        (
            "blank issue id",
            alert_with_event(json!({"event_id": "e1", "issue_id": "  "})),
        ),
        (
            "null issue id",
            alert_with_event(json!({"event_id": "e1", "issue_id": null})),
        ),
        (
            "missing triggered rule",
            json!({"data": {"event": {"event_id": "e1", "issue_id": "1"}}}).to_string(),
        ),
        (
            "blank triggered rule",
            json!({"data": {"event": {"event_id": "e1", "issue_id": "1"}, "triggered_rule": " "}})
                .to_string(),
        ),
        ("missing data", r#"{"action":"triggered"}"#.to_owned()),
        ("null data", r#"{"data":null}"#.to_owned()),
        ("null body", "null".to_owned()),
    ];
    for (name, body) in cases {
        let slice = given_the_slice();

        let reply = slice.when_posted(signed(body)).await;

        assert_eq!(reply.status, StatusCode::BAD_REQUEST, "{name}");
        reply.then_is(StatusCode::BAD_REQUEST, REQUIRED);
        slice.then_nothing_is_stored();
    }
}

#[tokio::test]
async fn an_oversized_body_is_rejected_and_stores_nothing() {
    let slice = given_the_slice();
    let body = alert_with_event(
        json!({"event_id": "e1", "issue_id": "1", "title": "x".repeat(10 << 20)}),
    );

    let reply = slice.when_posted(signed(body)).await;

    reply.then_is(StatusCode::PAYLOAD_TOO_LARGE, TOO_LARGE);
    slice.then_nothing_is_stored();
}

#[tokio::test]
async fn a_body_of_exactly_10_mib_is_accepted() {
    let slice = given_the_slice();
    let mut body = alert("event-1", "rule-a");
    body.push_str(&" ".repeat((10 << 20) - body.len()));

    let reply = slice.when_posted(signed(body)).await;

    reply.then_is(StatusCode::OK, RECORDED);
}

#[tokio::test]
async fn a_store_failure_is_an_internal_error_without_details() {
    let slice = given_the_slice();
    slice.store.close();
    let logs = capture_logs();

    let reply = slice.when_posted(signed(EVENT_ALERT)).await;

    reply.then_is(StatusCode::INTERNAL_SERVER_ERROR, INTERNAL);
    let logged = logs.lines();
    let line = logged
        .iter()
        .find(|line| line["message"] == "record sentry issue alert")
        .unwrap_or_else(|| panic!("no log line in {logged:?}"));
    assert_eq!(line["level"], "ERROR");
    assert_eq!(line["error"], "event store is closed");
}

#[tokio::test]
async fn exception_details_come_from_the_outermost_exception_and_keep_the_last_50_frames() {
    let slice = given_the_slice();
    let frames: Vec<Value> = (1..=60)
        .map(|i| json!({"filename": format!("f{i}.go"), "function": format!("fn{i}"), "lineno": i, "in_app": true}))
        .collect();
    let event = json!({
        "event_id": "e1",
        "issue_id": "1",
        "culprit": "app.handler",
        "exception": {"values": [
            {"type": "InnerError", "value": "cause", "stacktrace": {"frames": [{"filename": "inner.go", "function": "inner", "lineno": 1}]}},
            {"type": "OuterError", "value": "wrapped", "stacktrace": {"frames": frames}},
        ]},
    });

    let payload = slice.recorded_payload(alert_with_event(event)).await;

    assert_eq!(payload["culprit"], "app.handler");
    assert_eq!(payload["exceptionType"], "OuterError");
    assert_eq!(payload["exceptionValue"], "wrapped");
    let frames = payload["frames"].as_array().unwrap();
    assert_eq!(frames.len(), 50);
    assert_eq!(
        frames[0],
        json!({"file": "f11.go", "function": "fn11", "line": 11})
    );
    assert_eq!(
        frames[49],
        json!({"file": "f60.go", "function": "fn60", "line": 60})
    );
}

#[tokio::test]
async fn exception_details_are_capped_in_length() {
    let slice = given_the_slice();
    let long = |n: usize| "ü".repeat(n);
    let event = json!({
        "event_id": "e1",
        "issue_id": "1",
        "culprit": long(2100),
        "exception": {"values": [{
            "type": long(2100),
            "value": long(2100),
            "stacktrace": {"frames": [{"filename": long(600), "function": long(600), "lineno": 1}]},
        }]},
    });

    let payload = slice.recorded_payload(alert_with_event(event)).await;

    assert_eq!(payload["culprit"], long(2000));
    assert_eq!(payload["exceptionType"], long(2000));
    assert_eq!(payload["exceptionValue"], long(2000));
    assert_eq!(payload["frames"][0]["file"], long(500));
    assert_eq!(payload["frames"][0]["function"], long(500));
}

#[tokio::test]
async fn an_alert_without_exception_details_has_only_the_original_fields() {
    let slice = given_the_slice();
    let event = json!({"event_id": "e1", "issue_id": "1", "project": 3, "title": "t", "level": "error", "web_url": "https://sentry.io/x"});

    let payload = slice.recorded_payload(alert_with_event(event)).await;

    assert_eq!(
        payload,
        json!({"eventId": "e1", "issueId": "1", "project": 3, "title": "t", "level": "error", "webUrl": "https://sentry.io/x", "triggeredRule": "rule-a"})
    );
}

#[tokio::test]
async fn malformed_exception_details_are_dropped_not_rejected() {
    let cases = [
        json!({"values": "not a list"}),
        json!({"values": [{"type": 1}]}),
        json!({"values": [{"type": "T", "stacktrace": {"frames": [{"lineno": "1"}]}}]}),
        json!("not an object"),
        json!(["values"]),
        json!({"values": [["T", "v"]]}),
    ];
    for exception in cases {
        let slice = given_the_slice();
        let event = json!({"event_id": "e1", "issue_id": "1", "exception": exception});

        let payload = slice.recorded_payload(alert_with_event(event)).await;

        assert_eq!(payload.get("exceptionType"), None, "{exception}");
        assert_eq!(payload.get("frames"), None, "{exception}");
    }
}

/// A webhook body for event `e1` of issue `1`, rule `rule-a`, with `extra`
/// spliced in as the event's first field.
fn alert_with_raw_event_field(extra: &[u8]) -> Vec<u8> {
    [
        br#"{"data":{"event":{"#.as_slice(),
        extra,
        br#","event_id":"e1","issue_id":"1"},"triggered_rule":"rule-a"}}"#,
    ]
    .concat()
}

/// JSON nested `depth` arrays deep.
fn nested(depth: usize) -> String {
    format!("{}{}", "[".repeat(depth), "]".repeat(depth))
}

#[tokio::test]
async fn what_serde_json_cannot_read_in_an_ignored_field_does_not_matter() {
    let deep = format!(r#""ignored":{}"#, nested(200));
    let cases: [(&str, &[u8]); 3] = [
        ("lone surrogate", br#""ignored":"\udc00""#),
        ("invalid UTF-8", b"\"ignored\":\"\xff\""),
        ("nesting deeper than 128", deep.as_bytes()),
    ];
    for (name, extra) in cases {
        let slice = given_the_slice();

        let reply = slice
            .when_posted(signed(alert_with_raw_event_field(extra)))
            .await;

        assert_eq!(reply.status, StatusCode::OK, "{name}: {}", reply.body);
        reply.then_is(StatusCode::OK, RECORDED);
    }
}

#[tokio::test]
async fn invalid_utf8_in_the_exception_drops_only_the_details() {
    let slice = given_the_slice();
    let extra = b"\"exception\":{\"values\":[{\"type\":\"A\xff\"}]}";

    let payload = slice
        .recorded_payload(alert_with_raw_event_field(extra))
        .await;

    assert_eq!(payload.get("exceptionType"), None);
}

#[tokio::test]
async fn a_lone_surrogate_in_a_field_read_is_recorded_as_the_replacement_character() {
    let slice = given_the_slice();

    let payload = slice
        .recorded_payload(alert_with_raw_event_field(br#""title":"A\udc00""#))
        .await;

    assert_eq!(payload["title"], "A\u{FFFD}");
}

#[tokio::test]
async fn a_lone_surrogate_in_the_exception_keeps_the_details_with_the_replacement_character() {
    let slice = given_the_slice();
    let extra = br#""exception":{"values":[{"type":"A\ud83d","value":"\ude00!"}]}"#;

    let payload = slice
        .recorded_payload(alert_with_raw_event_field(extra))
        .await;

    assert_eq!(payload["exceptionType"], "A\u{FFFD}");
    assert_eq!(payload["exceptionValue"], "\u{FFFD}!");
}

#[tokio::test]
async fn a_surrogate_pair_stays_one_character() {
    let slice = given_the_slice();

    let payload = slice
        .recorded_payload(alert_with_raw_event_field(br#""title":"\ud83d\ude00""#))
        .await;

    assert_eq!(payload["title"], "\u{1F600}");
}

#[tokio::test]
async fn an_escaped_backslash_before_u_is_kept_as_text() {
    let slice = given_the_slice();

    let payload = slice
        .recorded_payload(alert_with_raw_event_field(br#""title":"\\udc00""#))
        .await;

    assert_eq!(payload["title"], r"\udc00");
}

#[tokio::test]
async fn the_signature_covers_the_body_as_sent() {
    let slice = given_the_slice();
    let sent = alert_with_raw_event_field(br#""title":"A\udc00""#);
    let rewritten = alert_with_raw_event_field(br#""title":"A\ufffd""#);

    let reply = slice
        .when_posted(signed(sent).with_signature(Some(sign(SECRET, rewritten))))
        .await;

    reply.then_is(StatusCode::UNAUTHORIZED, INVALID_SIGNATURE);
}

#[tokio::test]
async fn deep_nesting_in_an_ignored_exception_field_keeps_the_details() {
    let slice = given_the_slice();
    let extra = format!(
        r#""exception":{{"values":[{{"type":"T","ignored":{}}}]}}"#,
        nested(200)
    );

    let payload = slice
        .recorded_payload(alert_with_raw_event_field(extra.as_bytes()))
        .await;

    assert_eq!(payload["exceptionType"], "T");
}

#[tokio::test]
async fn null_fields_count_as_empty() {
    let slice = given_the_slice();
    let event = json!({
        "event_id": "e1", "issue_id": "1", "project": null, "title": null, "culprit": null,
        "exception": {"values": [{"type": "T", "value": null, "stacktrace": null}]},
    });

    let payload = slice.recorded_payload(alert_with_event(event)).await;

    assert_eq!(payload["project"], 0);
    assert_eq!(payload["title"], "");
    assert_eq!(payload.get("culprit"), None);
    assert_eq!(payload["exceptionType"], "T");
    assert_eq!(payload.get("frames"), None);
}

// Config

fn env(vars: &'static [(&'static str, &'static str)]) -> impl Fn(&str) -> Option<String> {
    move |key| {
        vars.iter()
            .find(|(k, _)| *k == key)
            .map(|(_, v)| (*v).to_owned())
    }
}

#[test]
fn the_slice_serves_post_sentry_callback_given_a_client_secret() {
    let getenv = env(&[("SENTRY_CLIENT_SECRET", "s3cret")]);

    let state_change = new(&Deps { getenv: &getenv }).unwrap();

    assert_eq!(state_change.method(), axum::routing::MethodFilter::POST);
    assert_eq!(state_change.path(), "/sentry/callback");
}

#[test]
fn the_slice_needs_a_client_secret() {
    for vars in [&[][..], &[("SENTRY_CLIENT_SECRET", "")][..]] {
        let getenv = env(vars);

        let err = new(&Deps { getenv: &getenv }).unwrap_err();

        assert_eq!(err.to_string(), "SENTRY_CLIENT_SECRET must be set");
    }
}

// Log capture: slices can't reach the crate's test helpers, so this is a
// small local one, in the app's JSON format.

struct Logs {
    buffer: Arc<Mutex<Vec<u8>>>,
    _guard: tracing::subscriber::DefaultGuard,
}

fn capture_logs() -> Logs {
    let buffer = Arc::new(Mutex::new(Vec::new()));
    let writer = Arc::clone(&buffer);
    let subscriber = tracing_subscriber::fmt()
        .json()
        .flatten_event(true)
        .with_writer(move || SharedWriter(Arc::clone(&writer)))
        .finish();
    Logs {
        buffer,
        _guard: tracing::subscriber::set_default(subscriber),
    }
}

impl Logs {
    fn lines(&self) -> Vec<Value> {
        let buffer = self.buffer.lock().unwrap();
        buffer
            .split(|b| *b == b'\n')
            .filter(|line| !line.is_empty())
            .map(|line| serde_json::from_slice(line).unwrap())
            .collect()
    }
}

struct SharedWriter(Arc<Mutex<Vec<u8>>>);

impl io::Write for SharedWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
