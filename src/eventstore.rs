//! The event store keeps every event in a single redb file.
//!
//! Events belong to streams. Each stream has a version, the number of events
//! in it, and every event also gets a global position: one more than the event
//! stored before it, in any stream. Appending checks the stream's version
//! first, so two writers can't both extend the same stream. Event handlers
//! remember how far they got as named checkpoints, and subscribers are woken
//! after every append.
//!
//! The file layout is a contract:
//!   - `events`:      global position -> JSON [`Event`]
//!   - `streams`:     stream ID -> current version
//!   - `checkpoints`: handler name -> last processed position

use std::path::Path;
use std::sync::{PoisonError, RwLock, RwLockReadGuard};

use chrono::{DateTime, Utc};
use redb::{Database, ReadableDatabase, ReadableTable, TableDefinition};
use serde::{Deserialize, Serialize};
use tokio::sync::watch;

/// The name of the redb file inside the data directory.
pub const FILE_NAME: &str = "heinzel.redb";

const EVENTS: TableDefinition<u64, &[u8]> = TableDefinition::new("events");
const STREAMS: TableDefinition<&str, u64> = TableDefinition::new("streams");
const CHECKPOINTS: TableDefinition<&str, u64> = TableDefinition::new("checkpoints");

/// A stored event.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Event {
    /// The stream the event belongs to.
    pub stream_id: String,
    /// The event's version within its stream, starting at 1.
    pub version: u64,
    /// The event's global position, starting at 1.
    pub position: u64,
    /// The event type, such as `SentryIssueAlertReceived`.
    #[serde(rename = "type")]
    pub event_type: String,
    /// When the event was appended, in UTC.
    pub timestamp: DateTime<Utc>,
    /// The event's JSON payload.
    pub payload: serde_json::Value,
}

/// An event that has not been stored yet.
#[derive(Debug, Clone, PartialEq)]
pub struct NewEvent {
    /// The event type, such as `SentryIssueAlertReceived`.
    pub event_type: String,
    /// The event's JSON payload.
    pub payload: serde_json::Value,
}

/// Where an append left its stream.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Appended {
    /// The stream's new version.
    pub version: u64,
    /// The global position of the last appended event.
    pub position: u64,
}

/// What can go wrong in the event store.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The stream's current version did not match the expected version.
    /// Nothing was written.
    #[error(
        "stream version conflict: stream {stream:?} is at version {actual}, expected {expected}"
    )]
    Conflict {
        /// The stream appended to.
        stream: String,
        /// The stream's current version.
        actual: u64,
        /// The version the caller expected.
        expected: u64,
    },
    /// Append was called without events.
    #[error("append: no events")]
    NoEvents,
    /// The store was closed.
    #[error("event store is closed")]
    Closed,
    /// The data directory could not be created.
    #[error("create data dir: {0}")]
    CreateDir(std::io::Error),
    /// The redb file failed.
    #[error("event store: {0}")]
    Storage(Box<redb::Error>),
    /// An event or a stored value could not be encoded or decoded.
    #[error("encode or decode event at position {position}: {source}")]
    Codec {
        /// The position of the event.
        position: u64,
        /// The JSON error.
        source: serde_json::Error,
    },
}

/// Wraps each redb error type the store meets in [`Error::Storage`].
macro_rules! from_redb {
    ($($redb_error:ty),*) => {$(
        impl From<$redb_error> for Error {
            fn from(err: $redb_error) -> Self {
                Error::Storage(Box::new(err.into()))
            }
        }
    )*};
}

from_redb!(
    redb::DatabaseError,
    redb::TransactionError,
    redb::TableError,
    redb::StorageError,
    redb::CommitError
);

type Clock = Box<dyn Fn() -> DateTime<Utc> + Send + Sync>;

/// A redb-backed event store. Share it as `Arc<Store>`.
pub struct Store {
    // None once closed. Every transaction holds the read lock, so close
    // waits for running transactions and later calls see `Closed`.
    db: RwLock<Option<Database>>,
    clock: Clock,
    appended: watch::Sender<()>,
}

impl std::fmt::Debug for Store {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Store").finish_non_exhaustive()
    }
}

/// Wake-ups after appends, from [`Store::subscribe`].
#[derive(Debug)]
pub struct Subscription(watch::Receiver<()>);

impl Subscription {
    /// Waits until an append commits after the last wake-up (or after
    /// subscribing). Wake-ups coalesce: many appends while nobody waits give a
    /// single wake-up, so the caller must re-read the store after each one.
    pub async fn wait(&mut self) {
        if self.0.changed().await.is_err() {
            // The store is gone, so no append will ever come.
            std::future::pending::<()>().await;
        }
    }
}

impl Store {
    /// Opens (creating if needed) the store in `dir`, timestamping events with
    /// the system clock.
    pub fn open(dir: impl AsRef<Path>) -> Result<Store, Error> {
        Store::open_with_clock(dir, Utc::now)
    }

    /// Opens (creating if needed) the store in `dir`, timestamping events with
    /// `clock`.
    pub fn open_with_clock(
        dir: impl AsRef<Path>,
        clock: impl Fn() -> DateTime<Utc> + Send + Sync + 'static,
    ) -> Result<Store, Error> {
        let dir = dir.as_ref();
        std::fs::create_dir_all(dir).map_err(Error::CreateDir)?;
        let db = Database::create(dir.join(FILE_NAME))?;
        let txn = db.begin_write()?;
        txn.open_table(EVENTS)?;
        txn.open_table(STREAMS)?;
        txn.open_table(CHECKPOINTS)?;
        txn.commit()?;
        Ok(Store {
            db: RwLock::new(Some(db)),
            clock: Box::new(clock),
            appended: watch::Sender::new(()),
        })
    }

    /// Closes the redb file, even while others still hold the store. Calls
    /// after that fail with [`Error::Closed`].
    ///
    /// It can't fail: redb has no fallible close and reports nothing when the
    /// file is dropped. Nothing is lost, since every commit is already
    /// durable.
    pub fn close(&self) {
        let mut db = self.db.write().unwrap_or_else(PoisonError::into_inner);
        *db = None;
    }

    /// Appends `events` to `stream` if its current version equals
    /// `expected_version` (0 means the stream must not exist yet). All events
    /// get the same timestamp.
    pub fn append(
        &self,
        stream: &str,
        expected_version: u64,
        events: &[NewEvent],
    ) -> Result<Appended, Error> {
        if events.is_empty() {
            return Err(Error::NoEvents);
        }
        let guard = self.lock();
        let db = guard.as_ref().ok_or(Error::Closed)?;
        let txn = db.begin_write()?;
        let appended = {
            let mut streams = txn.open_table(STREAMS)?;
            let mut stored = txn.open_table(EVENTS)?;
            let mut version = streams.get(stream)?.map_or(0, |v| v.value());
            if version != expected_version {
                return Err(Error::Conflict {
                    stream: stream.to_owned(),
                    actual: version,
                    expected: expected_version,
                });
            }
            let mut position = stored.last()?.map_or(0, |(k, _)| k.value());
            let timestamp = (self.clock)();
            for event in events {
                version += 1;
                position += 1;
                let data = serde_json::to_vec(&StoredEvent {
                    stream_id: stream,
                    version,
                    position,
                    event_type: &event.event_type,
                    timestamp,
                    payload: &event.payload,
                })
                .map_err(|source| Error::Codec { position, source })?;
                stored.insert(position, data.as_slice())?;
            }
            streams.insert(stream, version)?;
            Appended { version, position }
        };
        txn.commit()?;
        self.appended.send_replace(());
        Ok(appended)
    }

    /// Returns up to `limit` events with a global position greater than
    /// `position`, in global order.
    pub fn read_after(&self, position: u64, limit: usize) -> Result<Vec<Event>, Error> {
        let guard = self.lock();
        let db = guard.as_ref().ok_or(Error::Closed)?;
        let table = db.begin_read()?.open_table(EVENTS)?;
        let mut events = Vec::new();
        for entry in table.range(position.saturating_add(1)..)?.take(limit) {
            let (key, value) = entry?;
            let event = serde_json::from_slice(value.value()).map_err(|source| Error::Codec {
                position: key.value(),
                source,
            })?;
            events.push(event);
        }
        Ok(events)
    }

    /// Returns the last position processed by the named handler, or 0.
    pub fn checkpoint(&self, name: &str) -> Result<u64, Error> {
        let guard = self.lock();
        let db = guard.as_ref().ok_or(Error::Closed)?;
        let table = db.begin_read()?.open_table(CHECKPOINTS)?;
        Ok(table.get(name)?.map_or(0, |v| v.value()))
    }

    /// Records `position` as the last one processed by the named handler.
    pub fn set_checkpoint(&self, name: &str, position: u64) -> Result<(), Error> {
        let guard = self.lock();
        let db = guard.as_ref().ok_or(Error::Closed)?;
        let txn = db.begin_write()?;
        txn.open_table(CHECKPOINTS)?.insert(name, position)?;
        txn.commit()?;
        Ok(())
    }

    /// Returns a subscription that wakes after every committed append.
    pub fn subscribe(&self) -> Subscription {
        Subscription(self.appended.subscribe())
    }

    fn lock(&self) -> RwLockReadGuard<'_, Option<Database>> {
        self.db.read().unwrap_or_else(PoisonError::into_inner)
    }
}

/// The stored form of an event, borrowing from the append's input.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct StoredEvent<'a> {
    stream_id: &'a str,
    version: u64,
    position: u64,
    #[serde(rename = "type")]
    event_type: &'a str,
    timestamp: DateTime<Utc>,
    payload: &'a serde_json::Value,
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;
    use serde_json::json;
    use tempfile::TempDir;

    fn note(text: &str) -> NewEvent {
        NewEvent {
            event_type: "NoteRegistered".into(),
            payload: json!({ "text": text }),
        }
    }

    fn fixed_clock() -> DateTime<Utc> {
        Utc.with_ymd_and_hms(2026, 10, 24, 11, 0, 0).unwrap() + chrono::Duration::nanoseconds(5)
    }

    fn open(dir: &TempDir) -> Store {
        Store::open(dir.path()).unwrap()
    }

    fn appended(version: u64, position: u64) -> Appended {
        Appended { version, position }
    }

    #[test]
    fn append_to_new_stream_starts_at_version_and_position_one() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);

        let got = store.append("a", 0, &[note("hi")]).unwrap();

        assert_eq!(got, appended(1, 1));
    }

    #[test]
    fn append_rejects_expected_version_mismatch_without_writing() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        store.append("a", 0, &[note("first")]).unwrap();

        let err = store.append("a", 0, &[note("second")]).unwrap_err();

        assert!(
            matches!(&err, Error::Conflict { stream, actual: 1, expected: 0 } if stream == "a"),
            "got {err:?}"
        );
        assert_eq!(
            err.to_string(),
            r#"stream version conflict: stream "a" is at version 1, expected 0"#
        );
        let got = store.append("b", 0, &[note("third")]).unwrap();
        assert_eq!(got, appended(1, 2), "the rejected append took a position");
    }

    #[test]
    fn read_after_returns_stored_events_in_global_order() {
        let dir = TempDir::new().unwrap();
        let store = Store::open_with_clock(dir.path(), fixed_clock).unwrap();
        store.append("a", 0, &[note("one"), note("two")]).unwrap();
        let got = store.append("a", 2, &[note("three")]).unwrap();
        assert_eq!(got, appended(3, 3));

        let second = store.read_after(1, 1).unwrap();
        assert_eq!(
            second,
            vec![Event {
                stream_id: "a".into(),
                version: 2,
                position: 2,
                event_type: "NoteRegistered".into(),
                timestamp: fixed_clock(),
                payload: json!({ "text": "two" }),
            }]
        );

        let all = store.read_after(0, 10).unwrap();
        let positions: Vec<u64> = all.iter().map(|e| e.position).collect();
        assert_eq!(positions, [1, 2, 3]);

        assert_eq!(store.read_after(3, 10).unwrap(), []);
    }

    #[test]
    fn positions_are_global_and_gapless_across_streams() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);

        store.append("a", 0, &[note("1")]).unwrap();
        store.append("b", 0, &[note("2"), note("3")]).unwrap();
        store.append("a", 1, &[note("4")]).unwrap();

        let events = store.read_after(0, 10).unwrap();
        let got: Vec<(&str, u64, u64)> = events
            .iter()
            .map(|e| (e.stream_id.as_str(), e.version, e.position))
            .collect();
        assert_eq!(got, [("a", 1, 1), ("b", 1, 2), ("b", 2, 3), ("a", 2, 4)]);
    }

    #[test]
    fn checkpoint_defaults_to_zero_and_persists() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        assert_eq!(store.checkpoint("event-logger").unwrap(), 0);
        store.set_checkpoint("event-logger", 7).unwrap();
        drop(store);

        let store = open(&dir);
        assert_eq!(store.checkpoint("event-logger").unwrap(), 7);
        assert_eq!(store.checkpoint("other").unwrap(), 0);
    }

    #[test]
    fn events_survive_close_and_reopen() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        store.append("a", 0, &[note("hi")]).unwrap();
        store.close();

        let store = open(&dir);
        assert_eq!(store.read_after(0, 10).unwrap().len(), 1);
        let got = store.append("a", 1, &[note("again")]).unwrap();
        assert_eq!(got, appended(2, 2));
    }

    #[test]
    fn persisted_layout() {
        let dir = TempDir::new().unwrap();
        let store = Store::open_with_clock(dir.path(), fixed_clock).unwrap();
        store.append("a", 0, &[note("hi")]).unwrap();
        store.set_checkpoint("event-logger", 1).unwrap();
        drop(store);

        let db = Database::create(dir.path().join(FILE_NAME)).unwrap();
        let txn = db.begin_read().unwrap();
        let event = txn.open_table(EVENTS).unwrap().get(1).unwrap().unwrap();
        let event: serde_json::Value = serde_json::from_slice(event.value()).unwrap();
        assert_eq!(
            event,
            json!({
                "streamId": "a",
                "version": 1,
                "position": 1,
                "type": "NoteRegistered",
                "timestamp": "2026-10-24T11:00:00.000000005Z",
                "payload": { "text": "hi" },
            })
        );
        let version = txn.open_table(STREAMS).unwrap().get("a").unwrap().unwrap();
        assert_eq!(version.value(), 1);
        let checkpoints = txn.open_table(CHECKPOINTS).unwrap();
        assert_eq!(checkpoints.get("event-logger").unwrap().unwrap().value(), 1);
    }

    #[test]
    fn open_fails_fast_when_already_open() {
        let dir = TempDir::new().unwrap();
        let _held = open(&dir);

        assert!(Store::open(dir.path()).is_err());
    }

    #[test]
    fn open_creates_missing_data_dir() {
        let dir = TempDir::new().unwrap();
        let nested = dir.path().join("nested").join("data");

        let _store = Store::open(&nested).unwrap();

        assert!(nested.join(FILE_NAME).is_file());
    }

    #[test]
    fn calls_after_close_fail_with_closed() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        store.close();

        assert!(matches!(
            store.append("a", 0, &[note("hi")]),
            Err(Error::Closed)
        ));
        assert!(matches!(store.read_after(0, 1), Err(Error::Closed)));
        assert!(matches!(store.checkpoint("x"), Err(Error::Closed)));
        assert!(matches!(store.set_checkpoint("x", 1), Err(Error::Closed)));
        // Closing frees the file even though `store` is still alive.
        let _reopened = open(&dir);
    }

    #[test]
    fn append_wakes_subscribers_with_coalesced_wakeups() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        let mut first = store.subscribe();
        let mut second = store.subscribe();

        store.append("a", 0, &[note("a")]).unwrap();
        store.append("b", 0, &[note("b")]).unwrap();

        for sub in [&mut first, &mut second] {
            assert!(take_wakeup(sub), "subscriber not woken");
            assert!(!take_wakeup(sub), "wake-ups not coalesced");
        }
        assert!(store.append("a", 0, &[note("again")]).is_err());
        assert!(!take_wakeup(&mut first), "woken after a rejected append");
    }

    /// Reports whether a wake-up is pending, and consumes it.
    fn take_wakeup(sub: &mut Subscription) -> bool {
        let pending = sub.0.has_changed().unwrap();
        sub.0.mark_unchanged();
        pending
    }

    #[test]
    fn append_requires_events() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);

        assert!(matches!(store.append("a", 0, &[]), Err(Error::NoEvents)));
        assert_eq!(store.append("a", 0, &[note("hi")]).unwrap(), appended(1, 1));
    }

    #[test]
    fn concurrent_appends_to_new_stream_admit_exactly_one() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        const WRITERS: usize = 50;

        let results: Vec<Result<Appended, Error>> = std::thread::scope(|s| {
            let handles: Vec<_> = (0..WRITERS)
                .map(|_| s.spawn(|| store.append("same", 0, &[note("x")])))
                .collect();
            handles.into_iter().map(|h| h.join().unwrap()).collect()
        });

        let ok = results.iter().filter(|r| r.is_ok()).count();
        let conflicts = results
            .iter()
            .filter(|r| matches!(r, Err(Error::Conflict { .. })))
            .count();
        assert_eq!((ok, conflicts), (1, WRITERS - 1));
    }
}
