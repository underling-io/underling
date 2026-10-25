//! The exception details an alert records, and their caps. Alert details end
//! up in Claude's prompt and in pull requests, so their size stays bounded.

use serde::Deserialize;

use super::go_json::{Object, null_as_default};
use crate::events::SentryStackFrame;

/// How many stack frames are kept, counted from the newest.
const MAX_FRAMES: usize = 50;
/// Caps the culprit and the exception type and value, in chars.
const MAX_TEXT_CHARS: usize = 2000;
/// Caps a frame's file and function, in chars.
const MAX_FRAME_CHARS: usize = 500;

/// The outermost exception of an alert.
#[derive(Default)]
pub(super) struct Exception {
    pub(super) exception_type: String,
    pub(super) value: String,
    pub(super) frames: Vec<SentryStackFrame>,
}

/// Sentry's culprit, capped in length.
pub(super) fn culprit(culprit: &str) -> String {
    truncate(culprit, MAX_TEXT_CHARS)
}

/// Returns the outermost (last) exception in the webhook `body`'s
/// `data.event.exception`, with at most its last 50 frames and every text
/// capped in length. A missing or unexpectedly shaped exception gives empty
/// details: they are optional, so they never reject an alert.
///
/// It decodes the body again, reading only the exception, like Go decoding
/// a `json.RawMessage` on its own. So anything that fails in the exception,
/// even what Go would accept (invalid UTF-8, a lone surrogate escape), drops
/// only the details.
pub(super) fn details(body: &[u8]) -> Exception {
    let Ok(Object(decoded)) = serde_json::from_slice::<Object<Body>>(body) else {
        return Exception::default();
    };
    let Object(BodyData { event }) = decoded.data;
    let Object(BodyEvent { exception }) = event;
    let Object(decoded) = exception;
    let Some(Object(outer)) = decoded.values.last() else {
        return Exception::default();
    };
    let frames = &outer.stacktrace.0.frames;
    let newest = &frames[frames.len().saturating_sub(MAX_FRAMES)..];
    Exception {
        exception_type: truncate(&outer.exception_type, MAX_TEXT_CHARS),
        value: truncate(&outer.value, MAX_TEXT_CHARS),
        frames: newest
            .iter()
            .map(|Object(frame)| SentryStackFrame {
                file: truncate(&frame.filename, MAX_FRAME_CHARS),
                function: truncate(&frame.function, MAX_FRAME_CHARS),
                line: frame.lineno,
            })
            .collect(),
    }
}

/// `s` cut to at most `max` chars.
fn truncate(s: &str, max: usize) -> String {
    s.chars().take(max).collect()
}

// The path to the exception, and the part of it we record. Frame keys follow
// Sentry's stack trace interface: filename, function and lineno, ordered
// oldest call first.

#[derive(Default, Deserialize)]
struct Body {
    #[serde(default)]
    data: Object<BodyData>,
}

#[derive(Default, Deserialize)]
struct BodyData {
    #[serde(default)]
    event: Object<BodyEvent>,
}

#[derive(Default, Deserialize)]
struct BodyEvent {
    #[serde(default)]
    exception: Object<SentryException>,
}

#[derive(Default, Deserialize)]
struct SentryException {
    #[serde(default, deserialize_with = "null_as_default")]
    values: Vec<Object<SentryExceptionValue>>,
}

#[derive(Default, Deserialize)]
struct SentryExceptionValue {
    #[serde(rename = "type", default, deserialize_with = "null_as_default")]
    exception_type: String,
    #[serde(default, deserialize_with = "null_as_default")]
    value: String,
    #[serde(default)]
    stacktrace: Object<SentryStacktrace>,
}

#[derive(Default, Deserialize)]
struct SentryStacktrace {
    #[serde(default, deserialize_with = "null_as_default")]
    frames: Vec<Object<SentryFrame>>,
}

#[derive(Default, Deserialize)]
struct SentryFrame {
    #[serde(default, deserialize_with = "null_as_default")]
    filename: String,
    #[serde(default, deserialize_with = "null_as_default")]
    function: String,
    #[serde(default, deserialize_with = "null_as_default")]
    lineno: i64,
}
