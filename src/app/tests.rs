//! Runs the app in-process with test slices.

use std::net::SocketAddr;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use axum::extract::Path as UrlPath;
use axum::http::StatusCode;
use axum::routing::MethodFilter;
use tempfile::TempDir;
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{Semaphore, mpsc, oneshot};
use tokio::task::JoinHandle;
use tokio::time::timeout;
use tokio_util::sync::CancellationToken;
use tracing::instrument::WithSubscriber;

use super::*;
use crate::eventhandler::Handler;
use crate::eventstore::{Event, NewEvent};
use crate::testlog::{self, WAIT_LIMIT};

/// One in-process run of the app.
struct Instance {
    addr: SocketAddr,
    shutdown: CancellationToken,
    done: JoinHandle<Result<(), Error>>,
}

async fn start(config: Config, slices: Slices) -> Instance {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let shutdown = CancellationToken::new();
    let run = {
        let shutdown = shutdown.clone();
        async move { run(&config, listener, slices, shutdown).await }
    };
    let done = tokio::spawn(run.with_current_subscriber());
    Instance {
        addr,
        shutdown,
        done,
    }
}

impl Instance {
    /// Cancels the run and returns its result.
    async fn stop(self) -> Result<(), Error> {
        self.shutdown.cancel();
        self.finished().await
    }

    /// Waits for the run to return.
    async fn finished(self) -> Result<(), Error> {
        timeout(WAIT_LIMIT, self.done)
            .await
            .expect("run did not return")
            .unwrap()
    }

    /// Sends an empty POST to `path` and returns the status and body.
    async fn post(&self, path: &str) -> (StatusCode, String) {
        post(self.addr, path).await
    }

    /// Waits until new connections are refused.
    async fn wait_until_refusing(&self) {
        let refused = timeout(WAIT_LIMIT, async {
            while TcpStream::connect(self.addr).await.is_ok() {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        });
        refused.await.expect("still accepting connections");
    }
}

/// Sends an empty POST to `path` on `addr` and returns the status and body.
async fn post(addr: SocketAddr, path: &str) -> (StatusCode, String) {
    let response = reqwest::Client::new()
        .post(format!("http://{addr}{path}"))
        .send()
        .await
        .unwrap();
    let status = StatusCode::from_u16(response.status().as_u16()).unwrap();
    (status, response.text().await.unwrap())
}

fn config(dir: &Path, shutdown_timeout: Duration) -> Config {
    Config {
        addr: "127.0.0.1:0".into(),
        data_dir: dir.to_owned(),
        shutdown_timeout,
    }
}

/// Appends one TestEvent to a new stream and answers with its position.
fn append_event(store: &Store, stream: &str) -> (StatusCode, String) {
    let event = NewEvent {
        event_type: "TestEvent".into(),
        payload: serde_json::json!({}),
    };
    match store.append(stream, 0, &[event]) {
        Ok(appended) => (StatusCode::OK, appended.position.to_string()),
        Err(err) => (StatusCode::INTERNAL_SERVER_ERROR, err.to_string()),
    }
}

/// A test state change serving POST /append/{stream}.
fn appender() -> StateChange {
    StateChange::new(MethodFilter::POST, "/append/{stream}", |store| {
        move |UrlPath(stream): UrlPath<String>| {
            let store = Arc::clone(&store);
            async move { append_event(&store, &stream) }
        }
    })
}

/// The test's side of [`blocking`].
struct BlockingControl {
    entered: mpsc::UnboundedReceiver<()>,
    release: Arc<Semaphore>,
}

/// A test state change serving POST /block, which waits for a release before
/// appending to stream "blocked".
fn blocking() -> (StateChange, BlockingControl) {
    let (entered_tx, entered) = mpsc::unbounded_channel();
    let release = Arc::new(Semaphore::new(0));
    let gate = Arc::clone(&release);
    let state_change = StateChange::new(MethodFilter::POST, "/block", move |store| {
        move || {
            let (store, gate, entered) =
                (Arc::clone(&store), Arc::clone(&gate), entered_tx.clone());
            async move {
                let _ = entered.send(());
                let _permit = gate.acquire().await;
                append_event(&store, "blocked")
            }
        }
    });
    (state_change, BlockingControl { entered, release })
}

/// Forwards every handled event's position to a channel. If given a gate, it
/// blocks on its first event until released.
struct Projector {
    gate: Option<(oneshot::Sender<()>, oneshot::Receiver<()>)>,
    handled: mpsc::UnboundedSender<u64>,
}

#[async_trait]
impl Handler for Projector {
    fn name(&self) -> &str {
        "projector"
    }

    async fn handle(&mut self, event: Event) {
        if let Some((entered, release)) = self.gate.take() {
            let _ = entered.send(());
            let _ = release.await;
        }
        let _ = self.handled.send(event.position);
    }
}

async fn expect_handled(handled: &mut mpsc::UnboundedReceiver<u64>, want: &[u64]) {
    for &w in want {
        match timeout(WAIT_LIMIT, handled.recv()).await {
            Ok(Some(got)) => assert_eq!(got, w, "handled position {got}, want {w}"),
            _ => panic!("timed out waiting for position {w}"),
        }
    }
}

/// An automation running a projector that never blocks.
fn projector() -> (Automation, mpsc::UnboundedReceiver<u64>) {
    let (handled, rx) = mpsc::unbounded_channel();
    let automation = Automation::new(move |_, _| Projector {
        gate: None,
        handled,
    });
    (automation, rx)
}

#[tokio::test]
async fn events_are_handled_once_across_restarts() {
    let dir = TempDir::new().unwrap();

    for (run, stream) in ["first", "second"].into_iter().enumerate() {
        let (automation, mut handled) = projector();
        let slices = Slices {
            state_changes: vec![appender()],
            automations: vec![automation],
        };
        let app = start(config(dir.path(), Duration::from_secs(1)), slices).await;

        let want = run as u64 + 1;
        let (status, body) = app.post(&format!("/append/{stream}")).await;
        assert_eq!((status, body), (StatusCode::OK, want.to_string()));
        expect_handled(&mut handled, &[want]).await;
        app.stop().await.unwrap();
        assert!(
            handled.try_recv().is_err(),
            "run {run}: handled more than once"
        );
    }
}

#[tokio::test]
async fn logs_start_and_shutdown() {
    let dir = TempDir::new().unwrap();
    let mut logs = testlog::capture();
    let app = start(
        config(dir.path(), Duration::from_secs(1)),
        Slices::default(),
    )
    .await;

    let started = logs.wait_for("app started").await;
    assert_eq!(started["addr"], app.addr.to_string());
    assert_eq!(started["dataDir"], dir.path().display().to_string());
    app.shutdown.cancel();
    logs.wait_for("app shutting down").await;
    app.finished().await.unwrap();
}

#[tokio::test]
async fn shutdown_drains_requests_then_stops_handlers_then_closes_store() {
    let dir = TempDir::new().unwrap();
    let mut logs = testlog::capture();
    let (block, mut request) = blocking();
    let (entered_tx, entered) = oneshot::channel();
    let (release_handler, release_rx) = oneshot::channel();
    let (handled_tx, mut handled) = mpsc::unbounded_channel();
    let automation = Automation::new(move |_, _| Projector {
        gate: Some((entered_tx, release_rx)),
        handled: handled_tx,
    });
    let slices = Slices {
        state_changes: vec![appender(), block],
        automations: vec![automation],
    };
    let app = start(config(dir.path(), WAIT_LIMIT), slices).await;
    for stream in ["a", "b", "c"] {
        assert_eq!(
            app.post(&format!("/append/{stream}")).await.0,
            StatusCode::OK
        );
    }
    entered.await.unwrap(); // the handler is now mid-event at position 1

    let in_flight = tokio::spawn(post(app.addr, "/block"));
    request.entered.recv().await.unwrap();
    app.shutdown.cancel();
    logs.wait_for("app shutting down").await;
    app.wait_until_refusing().await;

    assert!(
        !app.done.is_finished(),
        "run returned before the in-flight request finished"
    );
    request.release.add_permits(1);
    assert_eq!(in_flight.await.unwrap().0, StatusCode::OK);
    logs.wait_for("stopping event handlers").await;
    assert!(
        !app.done.is_finished(),
        "run returned while a handler was mid-event"
    );
    release_handler.send(()).unwrap();
    app.finished().await.unwrap();

    expect_handled(&mut handled, &[1]).await;
    assert!(
        handled.try_recv().is_err(),
        "handled more than the current event"
    );
    let store = Store::open(dir.path()).expect("the store was not closed");
    assert_eq!(store.checkpoint("projector").unwrap(), 1);
    store.close();

    let (automation, mut resumed) = projector();
    let slices = Slices {
        automations: vec![automation],
        ..Slices::default()
    };
    let app = start(config(dir.path(), WAIT_LIMIT), slices).await;
    expect_handled(&mut resumed, &[2, 3, 4]).await;
    app.stop().await.unwrap();
}

#[tokio::test]
async fn shutdown_completes_when_a_request_outlives_the_timeout() {
    let dir = TempDir::new().unwrap();
    let (block, mut request) = blocking();
    let slices = Slices {
        state_changes: vec![block],
        ..Slices::default()
    };
    let app = start(config(dir.path(), Duration::from_millis(10)), slices).await;
    let addr = app.addr;
    let in_flight = tokio::spawn(async move {
        reqwest::Client::new()
            .post(format!("http://{addr}/block"))
            .send()
            .await
    });
    request.entered.recv().await.unwrap();

    let err = app.stop().await.unwrap_err();

    assert!(matches!(err, Error::ShutdownTimeout(_)), "got {err:?}");
    let cut_off = timeout(WAIT_LIMIT, in_flight)
        .await
        .expect("the request's connection was left open")
        .unwrap();
    assert!(cut_off.is_err(), "got {cut_off:?}");
    let store = Store::open(dir.path()).expect("the store was not closed");
    store.close();
}

#[tokio::test]
async fn dropping_the_run_stops_serving() {
    let dir = TempDir::new().unwrap();
    let mut logs = testlog::capture();
    let app = start(
        config(dir.path(), Duration::from_secs(1)),
        Slices::default(),
    )
    .await;
    logs.wait_for("app started").await;

    app.done.abort();

    app.wait_until_refusing().await;
}

#[tokio::test]
async fn run_fails_when_the_store_cannot_open() {
    let dir = TempDir::new().unwrap();
    let _held = Store::open(dir.path()).unwrap();

    let app = start(
        config(dir.path(), Duration::from_secs(1)),
        Slices::default(),
    )
    .await;

    let err = app.finished().await.unwrap_err();
    assert!(matches!(err, Error::OpenStore(_)), "got {err:?}");
}

/// Waits in `handle` until its token is cancelled, then reports it.
struct TokenWaiter {
    token: CancellationToken,
    entered: Option<oneshot::Sender<()>>,
    saw_cancel: Option<oneshot::Sender<()>>,
}

#[async_trait]
impl Handler for TokenWaiter {
    fn name(&self) -> &str {
        "token-waiter"
    }

    async fn handle(&mut self, _: Event) {
        if let Some(entered) = self.entered.take() {
            let _ = entered.send(());
        }
        self.token.cancelled().await;
        if let Some(saw_cancel) = self.saw_cancel.take() {
            let _ = saw_cancel.send(());
        }
    }
}

#[tokio::test]
async fn the_automation_token_is_cancelled_on_shutdown_before_handlers_stop() {
    let dir = TempDir::new().unwrap();
    let (built_tx, built) = oneshot::channel();
    let (entered_tx, entered) = oneshot::channel();
    let (saw_cancel_tx, saw_cancel) = oneshot::channel();
    let automation = Automation::new(move |token: CancellationToken, store| {
        let _ = built_tx.send((token.clone(), store));
        TokenWaiter {
            token,
            entered: Some(entered_tx),
            saw_cancel: Some(saw_cancel_tx),
        }
    });
    let slices = Slices {
        automations: vec![automation],
        ..Slices::default()
    };
    let app = start(config(dir.path(), Duration::from_secs(1)), slices).await;
    let (token, store) = timeout(WAIT_LIMIT, built).await.unwrap().unwrap();

    append_event(&store, "s");
    timeout(WAIT_LIMIT, entered).await.unwrap().unwrap();
    assert!(
        !token.is_cancelled(),
        "automation token cancelled before shutdown"
    );
    drop(store);

    app.stop()
        .await
        .expect("run did not return: the token was not cancelled before stopping the handlers");
    saw_cancel
        .await
        .expect("the handler did not see the cancellation");
}

#[tokio::test]
async fn dropping_the_run_cancels_the_automation_token() {
    let dir = TempDir::new().unwrap();
    let (built_tx, built) = oneshot::channel();
    let (entered_tx, entered) = oneshot::channel();
    let (saw_cancel_tx, saw_cancel) = oneshot::channel();
    let automation = Automation::new(move |token: CancellationToken, store| {
        let _ = built_tx.send(store);
        TokenWaiter {
            token,
            entered: Some(entered_tx),
            saw_cancel: Some(saw_cancel_tx),
        }
    });
    let slices = Slices {
        automations: vec![automation],
        ..Slices::default()
    };
    let app = start(config(dir.path(), Duration::from_secs(1)), slices).await;
    let store = timeout(WAIT_LIMIT, built).await.unwrap().unwrap();
    append_event(&store, "s");
    timeout(WAIT_LIMIT, entered).await.unwrap().unwrap();

    app.done.abort();

    timeout(WAIT_LIMIT, saw_cancel)
        .await
        .expect("the automation token was not cancelled")
        .unwrap();
}

/// Panics on every event.
struct Panicker;

#[async_trait]
impl Handler for Panicker {
    fn name(&self) -> &str {
        "panicker"
    }

    async fn handle(&mut self, event: Event) {
        panic!("boom at {}", event.position);
    }
}

#[tokio::test]
async fn a_handler_panic_shuts_the_app_down_with_an_error() {
    let dir = TempDir::new().unwrap();
    let mut logs = testlog::capture();
    let slices = Slices {
        state_changes: vec![appender()],
        automations: vec![Automation::new(|_, _| Panicker)],
    };
    let app = start(config(dir.path(), Duration::from_secs(1)), slices).await;

    assert_eq!(app.post("/append/a").await.0, StatusCode::OK);

    logs.wait_for("app shutting down").await;
    let err = app.finished().await.unwrap_err();
    assert_eq!(
        err.to_string(),
        "stop event handlers: event handler panicked: boom at 1"
    );
    let store = Store::open(dir.path()).expect("the store was not closed");
    assert_eq!(store.checkpoint("panicker").unwrap(), 0);
    store.close();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_request_stuck_in_sync_code_does_not_hold_up_shutdown() {
    let dir = TempDir::new().unwrap();
    let (entered_tx, mut entered) = mpsc::unbounded_channel();
    let stuck = StateChange::new(MethodFilter::POST, "/stuck", move |_| {
        move || {
            let entered = entered_tx.clone();
            async move {
                let _ = entered.send(());
                // Blocks this worker thread; no abort can interrupt it.
                std::thread::sleep(Duration::from_secs(2));
            }
        }
    });
    let slices = Slices {
        state_changes: vec![stuck],
        ..Slices::default()
    };
    let app = start(config(dir.path(), Duration::from_millis(50)), slices).await;
    let addr = app.addr;
    let _in_flight = tokio::spawn(async move {
        reqwest::Client::new()
            .post(format!("http://{addr}/stuck"))
            .send()
            .await
    });
    timeout(WAIT_LIMIT, entered.recv()).await.unwrap().unwrap();

    let started = std::time::Instant::now();
    let err = app.stop().await.unwrap_err();

    assert!(matches!(err, Error::ShutdownTimeout(_)), "got {err:?}");
    let took = started.elapsed();
    assert!(
        took < Duration::from_secs(1),
        "run took {took:?} to return, waiting for the stuck request"
    );
}
