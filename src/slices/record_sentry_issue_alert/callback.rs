//! The "Record Sentry issue alert" command, served on `POST /sentry/callback`.

use std::sync::Arc;

use axum::extract::Request;
use axum::http::header::CONTENT_TYPE;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use hmac::{Hmac, KeyInit, Mac};
use http_body_util::{BodyExt, LengthLimitError, Limited};
use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use super::exception;
use super::go_json::{Object, null_as_default, replace_lone_surrogates};
use crate::events::{self, SentryIssueAlertReceived};
use crate::eventstore::{self, NewEvent, Store};

/// The largest body accepted, as in Go.
const MAX_BODY_BYTES: usize = 10 << 20;

/// Handles one webhook delivery.
#[derive(Clone)]
pub(super) struct Callback {
    secret: Arc<[u8]>,
    store: Arc<Store>,
}

impl Callback {
    pub(super) fn new(secret: Vec<u8>, store: Arc<Store>) -> Callback {
        Callback {
            secret: secret.into(),
            store,
        }
    }

    /// Checks size, signature, resource and payload, in that order, then
    /// records the alert unless it was recorded before.
    pub(super) async fn record(self, request: Request) -> Response {
        let (parts, body) = request.into_parts();
        let body = match Limited::new(body, MAX_BODY_BYTES).collect().await {
            Ok(collected) => collected.to_bytes(),
            Err(err) if err.is::<LengthLimitError>() => {
                return error(StatusCode::PAYLOAD_TOO_LARGE, "body too large");
            }
            Err(_) => return error(StatusCode::BAD_REQUEST, "cannot read body"),
        };
        if !self.valid_signature(&parts.headers, &body) {
            return error(StatusCode::UNAUTHORIZED, "invalid signature");
        }
        if parts.headers.get("Sentry-Hook-Resource").map(|v| v.as_bytes()) != Some(b"event_alert")
        {
            return status("ignored");
        }
        // The signature covers the bytes as sent; decoding reads them as Go
        // would.
        let body = replace_lone_surrogates(&body);
        let Ok(Object(Webhook { data: Object(data) })) = serde_json::from_slice(&body) else {
            return error(StatusCode::BAD_REQUEST, "malformed event_alert payload");
        };
        let Object(event) = data.event;
        if blank(&event.event_id) || blank(&event.issue_id) || blank(&data.triggered_rule) {
            return error(
                StatusCode::BAD_REQUEST,
                "event_id, issue_id and triggered_rule are required",
            );
        }

        let stream = stream_id(&event.event_id, &data.triggered_rule);
        let exception = exception::details(&body);
        let alert = SentryIssueAlertReceived {
            event_id: event.event_id,
            issue_id: event.issue_id,
            project: event.project,
            title: event.title,
            level: event.level,
            web_url: event.web_url,
            triggered_rule: data.triggered_rule,
            culprit: exception::culprit(&event.culprit),
            exception_type: exception.exception_type,
            exception_value: exception.value,
            frames: exception.frames,
        };
        let payload = match serde_json::to_value(alert) {
            Ok(payload) => payload,
            Err(err) => return internal_error(err),
        };
        let new_event = NewEvent {
            event_type: events::SENTRY_ISSUE_ALERT_RECEIVED.to_owned(),
            payload,
        };
        match self.store.append(&stream, 0, &[new_event]) {
            Ok(_) => status("recorded"),
            Err(eventstore::Error::Conflict { .. }) => status("duplicate"),
            Err(err) => internal_error(err),
        }
    }

    /// Whether `Sentry-Hook-Signature` is the hex HMAC-SHA256 of `body` keyed
    /// with the client secret, compared in constant time.
    fn valid_signature(&self, headers: &HeaderMap, body: &[u8]) -> bool {
        let Some(signature) = headers
            .get("Sentry-Hook-Signature")
            .and_then(|v| hex::decode(v.as_bytes()).ok())
        else {
            return false;
        };
        let Ok(mut mac) = Hmac::<Sha256>::new_from_slice(&self.secret) else {
            return false;
        };
        mac.update(body);
        mac.verify_slice(&signature).is_ok()
    }
}

// The part of Sentry's event_alert payload we record, decoded by Go's rules
// (see `go_json`): a field of the wrong type rejects the payload.

#[derive(Default, Deserialize)]
struct Webhook {
    #[serde(default)]
    data: Object<Data>,
}

#[derive(Default, Deserialize)]
struct Data {
    #[serde(default)]
    event: Object<SentryEvent>,
    #[serde(default, deserialize_with = "null_as_default")]
    triggered_rule: String,
}

#[derive(Default, Deserialize)]
struct SentryEvent {
    #[serde(default, deserialize_with = "null_as_default")]
    event_id: String,
    #[serde(default, deserialize_with = "null_as_default")]
    issue_id: String,
    #[serde(default, deserialize_with = "null_as_default")]
    project: i64,
    #[serde(default, deserialize_with = "null_as_default")]
    title: String,
    #[serde(default, deserialize_with = "null_as_default")]
    level: String,
    #[serde(default, deserialize_with = "null_as_default")]
    web_url: String,
    #[serde(default, deserialize_with = "null_as_default")]
    culprit: String,
    // The exception is decoded on its own by `exception::details`, so an
    // unexpected one drops the details instead of the alert.
}

/// Whether `s` is empty or only whitespace. Values are trimmed only to
/// validate them; they are stored as sent.
fn blank(s: &str) -> bool {
    s.trim().is_empty()
}

/// Names the stream of one alert: one Sentry event triggering one rule. Both
/// parts are free text, so they are hashed rather than joined.
fn stream_id(event_id: &str, rule: &str) -> String {
    let hash = Sha256::new()
        .chain_update(event_id)
        .chain_update([0])
        .chain_update(rule)
        .finalize();
    format!("sentry-issue-alert-{}", hex::encode(hash))
}

fn internal_error(err: impl std::fmt::Display) -> Response {
    tracing::error!(error = %err, "record sentry issue alert");
    error(StatusCode::INTERNAL_SERVER_ERROR, "internal error")
}

fn status(status: &str) -> Response {
    json_reply(StatusCode::OK, &json!({ "status": status }))
}

fn error(code: StatusCode, message: &str) -> Response {
    json_reply(code, &json!({ "error": message }))
}

/// Writes `body` as Go's JSON encoder does, with a trailing newline.
fn json_reply(code: StatusCode, body: &Value) -> Response {
    (code, [(CONTENT_TYPE, "application/json")], format!("{body}\n")).into_response()
}
