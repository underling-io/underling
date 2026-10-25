//! The log format: one JSON object per line, with the message and its fields
//! at the top level, at level INFO and above.

use tracing::Subscriber;
use tracing_subscriber::fmt::MakeWriter;

/// Returns a subscriber that writes JSON log lines to `writer`.
pub fn json<W>(writer: W) -> impl Subscriber + Send + Sync + 'static
where
    W: for<'w> MakeWriter<'w> + Send + Sync + 'static,
{
    tracing_subscriber::fmt()
        .json()
        .flatten_event(true)
        .with_target(false)
        .with_current_span(false)
        .with_span_list(false)
        .with_max_level(tracing::Level::INFO)
        .with_writer(writer)
        .finish()
}
