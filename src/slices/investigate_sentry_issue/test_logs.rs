//! Log capture for the slice's tests: slices can't reach the crate's test
//! helpers, so this is a small local one. Its format is the app's
//! (`crate::logging::json`, which slices may not use either), repeated here.

use std::io;
use std::sync::{Arc, Mutex};

use serde_json::Value;

/// The log lines of the current thread, until dropped.
pub(super) struct Logs {
    buffer: Arc<Mutex<Vec<u8>>>,
    _guard: tracing::subscriber::DefaultGuard,
}

/// Starts capturing log lines.
pub(super) fn capture_logs() -> Logs {
    let buffer = Arc::new(Mutex::new(Vec::new()));
    let writer = Arc::clone(&buffer);
    let subscriber = tracing_subscriber::fmt()
        .json()
        .flatten_event(true)
        .with_target(false)
        .with_current_span(false)
        .with_span_list(false)
        .with_max_level(tracing::Level::INFO)
        .with_writer(move || SharedWriter(Arc::clone(&writer)))
        .finish();
    Logs {
        buffer,
        _guard: tracing::subscriber::set_default(subscriber),
    }
}

impl Logs {
    /// Every line so far, decoded.
    pub(super) fn lines(&self) -> Vec<Value> {
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
