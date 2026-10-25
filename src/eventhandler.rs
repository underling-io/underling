//! Event handlers react to stored events, one event at a time.
//!
//! The runner gives each handler its own task. A task reads the events after
//! the handler's checkpoint in global order, hands them over one by one and
//! saves the checkpoint after each, so a restart resumes where it left off.
//! When it has caught up it waits for the store's next append.

use std::any::Any;
use std::panic::AssertUnwindSafe;
use std::sync::Arc;

use async_trait::async_trait;
use futures_util::FutureExt;
use tokio::task::JoinHandle;
use tokio_util::sync::{CancellationToken, DropGuard};
use tracing::instrument::WithSubscriber;

use crate::eventstore::{self, Event, Store, Subscription};

const BATCH_SIZE: usize = 100;

/// Processes stored events.
///
/// `handle` is async so it can do long I/O (processes, HTTP) without blocking
/// other handlers. The runner waits for it to finish, even on stop; a handler
/// that must give up early on shutdown watches a cancellation token of its
/// own, like the one the app hands each automation.
#[async_trait]
pub trait Handler: Send {
    /// The handler's checkpoint key. It must never change.
    fn name(&self) -> &str;

    /// Handles one event. It can't fail; a handler deals with its own errors.
    /// A panic stops the handler, logged, without checkpointing the event,
    /// and [`Runner::panicked`] completes.
    async fn handle(&mut self, event: Event);
}

/// Why a handler stopped before it was asked to.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The handler's checkpoint could not be loaded.
    #[error("load checkpoint: {0}")]
    LoadCheckpoint(eventstore::Error),
    /// The events after `position` could not be read.
    #[error("read events after {position}: {error}")]
    Read {
        /// The position read after.
        position: u64,
        /// The store's error.
        error: eventstore::Error,
    },
    /// The checkpoint could not be saved after handling the event at
    /// `position`.
    #[error("save checkpoint {position}: {error}")]
    SaveCheckpoint {
        /// The position of the handled event.
        position: u64,
        /// The store's error.
        error: eventstore::Error,
    },
    /// The handler panicked; the event it was handling is not checkpointed.
    #[error("event handler panicked: {0}")]
    Panicked(String),
}

/// Runs one task per handler until stopped. Dropping it without
/// [`Runner::stop`] still signals the tasks to stop, but doesn't wait.
#[derive(Debug)]
pub struct Runner {
    stop: CancellationToken,
    _stop_on_drop: DropGuard,
    panicked: CancellationToken,
    tasks: Vec<(String, JoinHandle<Result<(), Error>>)>,
}

impl Runner {
    /// Starts a task for each handler. Each catches up from its checkpoint,
    /// then follows new events. Must be called inside a tokio runtime.
    pub fn start(store: Arc<Store>, handlers: Vec<Box<dyn Handler>>) -> Runner {
        let stop = CancellationToken::new();
        let panicked = CancellationToken::new();
        let tasks = handlers
            .into_iter()
            .map(|mut handler| {
                let name = handler.name().to_owned();
                let store = Arc::clone(&store);
                let stop = stop.clone();
                let wake = store.subscribe();
                let panicked = panicked.clone();
                let task_name = name.clone();
                let task = async move {
                    let result = AssertUnwindSafe(run(&store, handler.as_mut(), wake, &stop))
                        .catch_unwind()
                        .await
                        .unwrap_or_else(|panic| Err(Error::Panicked(panic_message(panic))));
                    if let Err(err) = &result {
                        if matches!(err, Error::Panicked(_)) {
                            panicked.cancel();
                        }
                        log_stopped(&task_name, err);
                    }
                    result
                };
                (name, tokio::spawn(task.with_current_subscriber()))
            })
            .collect();
        Runner {
            _stop_on_drop: stop.clone().drop_guard(),
            stop,
            panicked,
            tasks,
        }
    }

    /// Completes once a handler has panicked. A panic is a bug, so the app
    /// shuts down on it, as the Go version crashed; a store error only stops
    /// that handler.
    pub async fn panicked(&self) {
        self.panicked.cancelled().await;
    }

    /// Signals every task to stop and waits for them. A task that is handling
    /// an event finishes and checkpoints it first; later events stay
    /// unprocessed. Returns every error a handler stopped with, in start
    /// order. Each was logged when its handler stopped.
    pub async fn stop(self) -> Result<(), Vec<Error>> {
        self.stop.cancel();
        tracing::info!("stopping event handlers");
        let mut errors = Vec::new();
        for (name, task) in self.tasks {
            match task.await {
                Ok(Ok(())) => {}
                Ok(Err(err)) => errors.push(err),
                // The task catches panics, so this is unexpected (an abort);
                // nothing logged it yet.
                Err(join) => {
                    let err = Error::Panicked(join.to_string());
                    log_stopped(&name, &err);
                    errors.push(err);
                }
            }
        }
        if errors.is_empty() {
            Ok(())
        } else {
            Err(errors)
        }
    }
}

fn log_stopped(handler: &str, err: &Error) {
    tracing::error!(handler, error = %err, "event handler stopped");
}

/// Returns a panic's message, if it has one.
fn panic_message(panic: Box<dyn Any + Send>) -> String {
    match panic.downcast::<String>() {
        Ok(message) => *message,
        Err(panic) => match panic.downcast::<&'static str>() {
            Ok(message) => (*message).to_owned(),
            Err(_) => "panic without a message".to_owned(),
        },
    }
}

async fn run(
    store: &Store,
    handler: &mut dyn Handler,
    mut wake: Subscription,
    stop: &CancellationToken,
) -> Result<(), Error> {
    let name = handler.name().to_owned();
    let mut position = store.checkpoint(&name).map_err(Error::LoadCheckpoint)?;
    loop {
        let events = store
            .read_after(position, BATCH_SIZE)
            .map_err(|error| Error::Read { position, error })?;
        if events.is_empty() {
            tokio::select! {
                () = wake.wait() => continue,
                () = stop.cancelled() => return Ok(()),
            }
        }
        for event in events {
            if stop.is_cancelled() {
                return Ok(());
            }
            let handled = event.position;
            // A panic unwinds out of here to the task, skipping the
            // checkpoint.
            handler.handle(event).await;
            store
                .set_checkpoint(&name, handled)
                .map_err(|error| Error::SaveCheckpoint {
                    position: handled,
                    error,
                })?;
            position = handled;
            // A handler that doesn't await (or whose awaits are always
            // ready) would otherwise hold its worker thread for the whole
            // catch-up, starving HTTP requests and the other handlers.
            tokio::task::yield_now().await;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::eventstore::NewEvent;
    use crate::testlog::{self, WAIT_LIMIT};
    use tempfile::TempDir;
    use tokio::sync::{mpsc, oneshot};
    use tokio::time::timeout;

    /// Forwards every handled event's position to a channel.
    struct Recorder {
        name: &'static str,
        handled: mpsc::UnboundedSender<u64>,
    }

    #[async_trait]
    impl Handler for Recorder {
        fn name(&self) -> &str {
            self.name
        }

        async fn handle(&mut self, event: Event) {
            let _ = self.handled.send(event.position);
        }
    }

    /// What a handler handled, as seen by the test.
    struct Handled(mpsc::UnboundedReceiver<u64>);

    impl Handled {
        async fn expect(&mut self, want: &[u64]) {
            for &w in want {
                let got = timeout(WAIT_LIMIT, self.0.recv()).await;
                match got {
                    Ok(Some(got)) => assert_eq!(got, w, "handled position {got}, want {w}"),
                    _ => panic!("timed out waiting for position {w}"),
                }
            }
        }

        fn expect_nothing_more(&mut self) {
            if let Ok(got) = self.0.try_recv() {
                panic!("unexpectedly handled position {got}");
            }
        }
    }

    fn recorder(name: &'static str) -> (Box<dyn Handler>, Handled) {
        let (handled, rx) = mpsc::unbounded_channel();
        (Box::new(Recorder { name, handled }), Handled(rx))
    }

    /// Blocks while handling its first event until released.
    struct Blocker {
        entered: Option<oneshot::Sender<u64>>,
        release: Option<oneshot::Receiver<()>>,
        handled: mpsc::UnboundedSender<u64>,
    }

    #[async_trait]
    impl Handler for Blocker {
        fn name(&self) -> &str {
            "blocker"
        }

        async fn handle(&mut self, event: Event) {
            if let (Some(entered), Some(release)) = (self.entered.take(), self.release.take()) {
                let _ = entered.send(event.position);
                let _ = release.await;
            }
            let _ = self.handled.send(event.position);
        }
    }

    /// The test's side of a [`Blocker`].
    struct BlockerControl {
        entered: oneshot::Receiver<u64>,
        release: oneshot::Sender<()>,
        handled: Handled,
    }

    fn blocker() -> (Box<dyn Handler>, BlockerControl) {
        let (entered_tx, entered) = oneshot::channel();
        let (release, release_rx) = oneshot::channel();
        let (handled_tx, handled) = mpsc::unbounded_channel();
        let handler = Blocker {
            entered: Some(entered_tx),
            release: Some(release_rx),
            handled: handled_tx,
        };
        let control = BlockerControl {
            entered,
            release,
            handled: Handled(handled),
        };
        (Box::new(handler), control)
    }

    fn open(dir: &TempDir) -> Arc<Store> {
        Arc::new(Store::open(dir.path()).unwrap())
    }

    fn append_notes(store: &Store, streams: &[&str]) {
        for stream in streams {
            let event = NewEvent {
                event_type: "NoteRegistered".into(),
                payload: serde_json::json!({}),
            };
            store.append(stream, 0, &[event]).unwrap();
        }
    }

    #[tokio::test]
    async fn catches_up_then_follows_new_events() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a", "b"]);
        let (rec, mut handled) = recorder("recorder");

        let runner = Runner::start(Arc::clone(&store), vec![rec]);
        handled.expect(&[1, 2]).await;
        append_notes(&store, &["c"]);
        handled.expect(&[3]).await;
        runner.stop().await.unwrap();

        assert_eq!(store.checkpoint("recorder").unwrap(), 3);
        handled.expect_nothing_more();
    }

    #[tokio::test]
    async fn resumes_from_checkpoint_after_restart() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a", "b"]);
        let (first, mut handled) = recorder("recorder");
        let runner = Runner::start(Arc::clone(&store), vec![first]);
        handled.expect(&[1, 2]).await;
        runner.stop().await.unwrap();
        store.close();

        let store = open(&dir);
        append_notes(&store, &["c", "d"]);
        let (second, mut handled) = recorder("recorder");
        let runner = Runner::start(Arc::clone(&store), vec![second]);
        handled.expect(&[3, 4]).await;
        runner.stop().await.unwrap();

        handled.expect_nothing_more();
        assert_eq!(store.checkpoint("recorder").unwrap(), 4);
    }

    #[tokio::test]
    async fn each_handler_keeps_its_own_checkpoint() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a", "b"]);
        store.set_checkpoint("ahead", 1).unwrap();
        let (behind, mut behind_handled) = recorder("behind");
        let (ahead, mut ahead_handled) = recorder("ahead");

        let runner = Runner::start(Arc::clone(&store), vec![behind, ahead]);
        behind_handled.expect(&[1, 2]).await;
        ahead_handled.expect(&[2]).await;
        runner.stop().await.unwrap();

        ahead_handled.expect_nothing_more();
    }

    #[tokio::test]
    async fn a_busy_handler_does_not_hold_up_the_others() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a", "b"]);
        let (busy, control) = blocker();
        let (rec, mut handled) = recorder("recorder");

        let runner = Runner::start(Arc::clone(&store), vec![busy, rec]);
        control.entered.await.unwrap();
        handled.expect(&[1, 2]).await;
        control.release.send(()).unwrap();
        runner.stop().await.unwrap();
    }

    #[tokio::test]
    async fn stop_finishes_and_checkpoints_the_current_event_only() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a", "b", "c"]);
        let mut logs = testlog::capture();
        let (busy, mut control) = blocker();
        let runner = Runner::start(Arc::clone(&store), vec![busy]);
        control.entered.await.unwrap();

        let stopped = tokio::spawn(runner.stop().with_current_subscriber());
        logs.wait_for("stopping event handlers").await;
        assert!(
            !stopped.is_finished(),
            "stop returned while the handler was mid-event"
        );
        control.release.send(()).unwrap();
        timeout(WAIT_LIMIT, stopped)
            .await
            .unwrap()
            .unwrap()
            .unwrap();

        assert_eq!(store.checkpoint("blocker").unwrap(), 1);
        control.handled.expect(&[1]).await;
        control.handled.expect_nothing_more();
        store.close();

        let store = open(&dir);
        let (rec, mut handled) = recorder("blocker");
        let runner = Runner::start(Arc::clone(&store), vec![rec]);
        handled.expect(&[2, 3]).await;
        runner.stop().await.unwrap();
    }

    #[tokio::test]
    async fn a_store_error_stops_the_handler_and_is_logged() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a"]);
        let mut logs = testlog::capture();
        let (busy, control) = blocker();
        let runner = Runner::start(Arc::clone(&store), vec![busy]);
        control.entered.await.unwrap();

        store.close();
        control.release.send(()).unwrap();

        let line = logs.wait_for("event handler stopped").await;
        assert_eq!(line["level"], "ERROR");
        assert_eq!(line["handler"], "blocker");
        assert_eq!(line["error"], "save checkpoint 1: event store is closed");
        assert!(
            runner.panicked().now_or_never().is_none(),
            "a store error was treated as a panic"
        );
        let errors = runner.stop().await.unwrap_err();
        assert!(
            matches!(errors[..], [Error::SaveCheckpoint { position: 1, .. }]),
            "got {errors:?}"
        );
    }

    /// Panics while handling the event at `at`.
    struct Panicker {
        name: &'static str,
        at: u64,
    }

    #[async_trait]
    impl Handler for Panicker {
        fn name(&self) -> &str {
            self.name
        }

        async fn handle(&mut self, event: Event) {
            if event.position == self.at {
                panic!("boom at {}", event.position);
            }
        }
    }

    #[tokio::test]
    async fn a_panic_stops_the_handler_without_checkpointing_the_event() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a", "b", "c"]);
        let mut logs = testlog::capture();
        let panicker = Box::new(Panicker {
            name: "panicker",
            at: 2,
        });

        let runner = Runner::start(Arc::clone(&store), vec![panicker]);

        let line = logs.wait_for("event handler stopped").await;
        assert_eq!(line["level"], "ERROR");
        assert_eq!(line["handler"], "panicker");
        assert_eq!(line["error"], "event handler panicked: boom at 2");
        assert_eq!(store.checkpoint("panicker").unwrap(), 1);
        assert!(
            runner.panicked().now_or_never().is_some(),
            "the runner didn't signal the panic"
        );
        let errors = runner.stop().await.unwrap_err();
        assert!(
            matches!(&errors[..], [Error::Panicked(msg)] if msg == "boom at 2"),
            "got {errors:?}"
        );
    }

    #[tokio::test]
    async fn stop_returns_every_handler_error() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a", "b"]);
        let first = Box::new(Panicker {
            name: "first",
            at: 1,
        });
        let second = Box::new(Panicker {
            name: "second",
            at: 2,
        });
        let mut logs = testlog::capture();
        let runner = Runner::start(Arc::clone(&store), vec![first, second]);
        logs.wait_for("event handler stopped").await;
        logs.wait_for("event handler stopped").await;

        let errors = runner.stop().await.unwrap_err();

        let messages: Vec<String> = errors.iter().map(ToString::to_string).collect();
        assert_eq!(
            messages,
            [
                "event handler panicked: boom at 1",
                "event handler panicked: boom at 2"
            ]
        );
    }

    #[tokio::test]
    async fn dropping_the_runner_stops_its_handlers() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        append_notes(&store, &["a"]);
        let (rec, mut handled) = recorder("recorder");
        let runner = Runner::start(Arc::clone(&store), vec![rec]);
        handled.expect(&[1]).await;

        drop(runner);

        // The task ends and drops the handler, closing its channel.
        let ended = timeout(WAIT_LIMIT, handled.0.recv()).await;
        assert!(matches!(ended, Ok(None)), "got {ended:?}");
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_catching_up_handler_lets_other_tasks_run() {
        let dir = TempDir::new().unwrap();
        let store = open(&dir);
        let events: Vec<NewEvent> = (0..3 * BATCH_SIZE)
            .map(|_| NewEvent {
                event_type: "NoteRegistered".into(),
                payload: serde_json::json!({}),
            })
            .collect();
        store.append("many", 0, &events).unwrap();
        let (rec, mut handled) = recorder("recorder");
        let runner = Runner::start(Arc::clone(&store), vec![rec]);

        // On this single thread, the handler only runs while the test waits.
        tokio::task::yield_now().await;

        let mut count = 0;
        while handled.0.try_recv().is_ok() {
            count += 1;
        }
        assert!(
            count < events.len(),
            "the handler caught up on all {count} events without yielding"
        );
        runner.stop().await.unwrap();
    }
}
