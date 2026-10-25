//! Reads claude's stream-json output: one JSON event per line.

use serde::Deserialize;
use serde_json::value::RawValue;
use tokio::io::{AsyncBufRead, AsyncBufReadExt};

/// The part of a stream-json event heinzel reads.
#[derive(Debug, Default, Deserialize)]
struct StreamEvent {
    #[serde(rename = "type", default)]
    event_type: Option<String>,
    #[serde(default)]
    subtype: Option<String>,
    #[serde(default)]
    is_error: Option<bool>,
    #[serde(default)]
    result: Option<String>,
}

/// The part of the `system/init` event heinzel checks: the session's
/// permission mode and which plugins loaded. Field names follow the Agent
/// SDK's `SDKSystemMessage`, which stream-json emits.
#[derive(Debug, Default, Deserialize)]
pub(super) struct InitEvent {
    #[serde(rename = "permissionMode", default)]
    pub(super) permission_mode: Option<String>,
    #[serde(default)]
    pub(super) plugins: Option<Vec<InitPlugin>>,
    /// Omitted when no plugin failed to load. Each kept as its raw JSON.
    #[serde(default)]
    pub(super) plugin_errors: Option<Vec<Box<RawValue>>>,
}

#[derive(Debug, Default, Deserialize)]
pub(super) struct InitPlugin {
    #[serde(default)]
    pub(super) path: Option<String>,
}

/// What heinzel takes from claude's stream-json output, as far as it was
/// read.
#[derive(Debug, Default)]
pub(super) struct StreamOutput {
    /// The first `system/init` event, if any.
    pub(super) init: Option<InitEvent>,
    last: Option<StreamEvent>,
}

impl StreamOutput {
    /// The last event's result text, if the last event is a result.
    pub(super) fn summary(&self) -> &str {
        match &self.last {
            Some(e) if e.event_type.as_deref() == Some("result") => {
                e.result.as_deref().unwrap_or_default()
            }
            _ => "",
        }
    }

    /// Whether the last event is a successful result.
    pub(super) fn ok(&self) -> bool {
        matches!(&self.last, Some(e) if e.event_type.as_deref() == Some("result") && e.is_error != Some(true))
    }
}

/// Reads a stream-json output to its end into `out`, so a caller that gives
/// up early keeps what was read. Lines may be of any length; lines that
/// aren't JSON objects with a `type` are skipped.
pub(super) async fn read_stream(
    mut reader: impl AsyncBufRead + Unpin,
    out: &mut StreamOutput,
) -> std::io::Result<()> {
    let mut line = Vec::new();
    loop {
        line.clear();
        if reader.read_until(b'\n', &mut line).await? == 0 {
            return Ok(());
        }
        let trimmed = line.trim_ascii();
        let Some(event) = decode_event(trimmed) else {
            continue;
        };
        if out.init.is_none()
            && event.event_type.as_deref() == Some("system")
            && event.subtype.as_deref() == Some("init")
        {
            out.init = Some(serde_json::from_slice(trimmed).unwrap_or_default());
        }
        out.last = Some(event);
    }
}

/// Decodes one line as an event: a JSON object with a non-empty `type`.
fn decode_event(line: &[u8]) -> Option<StreamEvent> {
    if line.is_empty() {
        return None;
    }
    let value: serde_json::Value = serde_json::from_slice(line).ok()?;
    if !value.is_object() {
        return None;
    }
    let event: StreamEvent = serde_json::from_value(value).ok()?;
    event
        .event_type
        .as_deref()
        .is_some_and(|t| !t.is_empty())
        .then_some(event)
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn read(stream: &str) -> StreamOutput {
        let mut out = StreamOutput::default();
        read_stream(stream.as_bytes(), &mut out).await.unwrap();
        out
    }

    #[tokio::test]
    async fn the_last_event_decides_the_result() {
        let cases = [
            ("success", "{\"type\":\"system\",\"subtype\":\"init\"}\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"Fixed it.\"}\n", "Fixed it.", true),
            ("no trailing newline", r#"{"type":"result","is_error":false,"result":"ok"}"#, "ok", true),
            ("error result", "{\"type\":\"result\",\"is_error\":true,\"result\":\"boom\"}\n", "boom", false),
            ("result not last", "{\"type\":\"result\",\"is_error\":false,\"result\":\"ok\"}\n{\"type\":\"assistant\"}\n", "", false),
            ("no events", "", "", false),
            ("noise is skipped", "{\"type\":\"result\",\"is_error\":false,\"result\":\"ok\"}\nnot json\n\n[\"result\"]\n{\"result\":\"x\"}\n", "ok", true),
            ("null fields", "{\"type\":\"result\",\"is_error\":null,\"result\":null}\n", "", true),
        ];
        for (name, stream, summary, ok) in cases {
            let out = read(stream).await;

            assert_eq!((out.summary(), out.ok()), (summary, ok), "{name}");
        }
    }

    #[tokio::test]
    async fn long_lines_are_read() {
        let long = format!("{{\"type\":\"user\",\"content\":\"{}\"}}\n", "x".repeat(5 << 20));
        let out = read(&format!("{long}{{\"type\":\"result\",\"is_error\":false,\"result\":\"ok\"}}\n")).await;

        assert_eq!((out.summary(), out.ok()), ("ok", true));
    }

    #[tokio::test]
    async fn the_first_init_event_is_kept() {
        let stream = concat!(
            "{\"type\":\"system\",\"subtype\":\"api_retry\"}\n",
            "{\"type\":\"system\",\"subtype\":\"init\",\"cwd\":\"/w/run/repo\",\"permissionMode\":\"acceptEdits\",\"plugins\":[{\"name\":\"codex\",\"path\":\"/p/codex\"}],\"plugin_errors\":[{\"plugin\":\"at\", \"type\":\"x\",\"message\":\"m\"}]}\n",
            "{\"type\":\"system\",\"subtype\":\"init\",\"permissionMode\":\"default\"}\n",
            "{\"type\":\"result\",\"is_error\":false,\"result\":\"ok\"}\n",
        );

        let out = read(stream).await;

        let init = out.init.unwrap();
        assert_eq!(init.permission_mode.as_deref(), Some("acceptEdits"));
        let plugins = init.plugins.unwrap();
        assert_eq!(plugins.len(), 1);
        assert_eq!(plugins[0].path.as_deref(), Some("/p/codex"));
        let errors = init.plugin_errors.unwrap();
        assert_eq!(errors.len(), 1);
        assert_eq!(errors[0].get(), r#"{"plugin":"at", "type":"x","message":"m"}"#);
    }
}
