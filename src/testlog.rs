//! Captures the log lines a test produces, in the app's JSON format.

use std::io;
use std::time::Duration;

use serde_json::Value;
use tokio::sync::mpsc;
use tracing::subscriber::DefaultGuard;

/// How long a test waits for something before failing. It only guards
/// against hangs.
pub(crate) const WAIT_LIMIT: Duration = Duration::from_secs(5);

/// The log lines of the current thread, and of tasks spawned with its
/// subscriber, until dropped.
pub(crate) struct Logs {
    lines: mpsc::UnboundedReceiver<Value>,
    _guard: DefaultGuard,
}

/// Starts capturing log lines.
pub(crate) fn capture() -> Logs {
    let (tx, lines) = mpsc::unbounded_channel();
    let guard =
        tracing::subscriber::set_default(crate::logging::json(move || LineWriter(tx.clone())));
    Logs {
        lines,
        _guard: guard,
    }
}

impl Logs {
    /// Waits for the next line with `message` and returns it whole.
    pub(crate) async fn wait_for(&mut self, message: &str) -> Value {
        let found = tokio::time::timeout(WAIT_LIMIT, async {
            while let Some(line) = self.lines.recv().await {
                if line["message"] == message {
                    return Some(line);
                }
            }
            None
        })
        .await;
        match found {
            Ok(Some(line)) => line,
            _ => panic!("timed out waiting for log {message:?}"),
        }
    }
}

struct LineWriter(mpsc::UnboundedSender<Value>);

impl io::Write for LineWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let line = serde_json::from_slice(buf).map_err(io::Error::other)?;
        // The test may be done listening; that's fine.
        let _ = self.0.send(line);
        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
