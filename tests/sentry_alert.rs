//! End to end: Sentry webhook deliveries posted to the running app, through
//! its real HTTP server, and what ends up in the event store.

use std::net::SocketAddr;
use std::time::Duration;

use heinzel::app::{self, Config, Deps};
use heinzel::eventstore::{Event, Store};
use hmac::{Hmac, KeyInit, Mac};
use sha2::{Digest, Sha256};
use tempfile::TempDir;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

const SECRET: &str = "e2e-client-secret";
const WAIT_LIMIT: Duration = Duration::from_secs(5);
const MIB: usize = 1 << 20;

fn sign(body: &str) -> String {
    let mut mac = Hmac::<Sha256>::new_from_slice(SECRET.as_bytes()).unwrap();
    mac.update(body.as_bytes());
    hex::encode(mac.finalize().into_bytes())
}

/// An alert for event `event_id`, with `title`.
fn alert(event_id: &str, title: &str) -> String {
    serde_json::json!({
        "action": "triggered",
        "data": {
            "event": {"event_id": event_id, "issue_id": "1117540176", "project": 1, "title": title, "level": "error"},
            "triggered_rule": "Very Important Alert Rule!",
        },
    })
    .to_string()
}

/// The app, built from every slice as `main` does, serving on a free port
/// with a fresh data dir.
struct RunningApp {
    addr: SocketAddr,
    dir: TempDir,
    shutdown: CancellationToken,
    run: JoinHandle<Result<(), app::Error>>,
}

async fn start_app() -> RunningApp {
    let dir = TempDir::new().unwrap();
    let config = Config {
        addr: "127.0.0.1:0".into(),
        data_dir: dir.path().into(),
        shutdown_timeout: WAIT_LIMIT,
    };
    let work_dir = dir.path().join("investigations");
    let getenv = |key: &str| {
        let value = match key {
            "SENTRY_CLIENT_SECRET" => SECRET,
            // The investigator's required config. The alerts posted here are
            // for project 1, which it skips as unmapped.
            "SENTRY_GITHUB_REPOS" => "2=acme/web",
            "GITHUB_TOKEN" => "e2e-github-token",
            "ANTHROPIC_API_KEY" => "e2e-anthropic-key",
            "OPENAI_API_KEY" => "e2e-openai-key",
            "CLAUDE_PLUGIN_DIRS" => "/plugins/codex,/plugins/auto-task",
            "INVESTIGATION_WORK_DIR" => work_dir.to_str().unwrap(),
            _ => return None,
        };
        Some(value.to_owned())
    };
    let slices = app::build(heinzel::slices::FACTORIES, &Deps { getenv: &getenv }).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let shutdown = CancellationToken::new();
    let run = tokio::spawn({
        let shutdown = shutdown.clone();
        async move { app::run(&config, listener, slices, shutdown).await }
    });
    RunningApp {
        addr,
        dir,
        shutdown,
        run,
    }
}

impl RunningApp {
    /// Posts a signed event_alert delivery of `body`; returns the status and
    /// the reply body.
    async fn post_signed(&self, body: String) -> (u16, String) {
        let response = reqwest::Client::new()
            .post(format!("http://{}/sentry/callback", self.addr))
            .header("Content-Type", "application/json")
            .header("Sentry-Hook-Resource", "event_alert")
            .header("Sentry-Hook-Signature", sign(&body))
            .body(body)
            .timeout(WAIT_LIMIT)
            .send()
            .await
            .unwrap();
        (response.status().as_u16(), response.text().await.unwrap())
    }

    /// Shuts the app down cleanly and returns every stored alert, leaving
    /// out what the investigator recorded about them.
    async fn stop(self) -> Vec<Event> {
        self.shutdown.cancel();
        tokio::time::timeout(WAIT_LIMIT, self.run)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        Store::open(self.dir.path())
            .unwrap()
            .read_after(0, 10)
            .unwrap()
            .into_iter()
            .filter(|e| e.event_type == "SentryIssueAlertReceived")
            .collect()
    }
}

#[tokio::test]
async fn a_signed_alert_posted_to_the_app_is_stored() {
    let app = start_app().await;

    let reply = app
        .post_signed(alert(
            "aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4",
            "ReferenceError: blooopy is not defined",
        ))
        .await;

    assert_eq!(reply, (200, "{\"status\":\"recorded\"}\n".to_owned()));
    let stored = app.stop().await;
    assert_eq!(stored.len(), 1, "stored {stored:?}");
    assert_eq!(stored[0].event_type, "SentryIssueAlertReceived");
    let stream = Sha256::digest("aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4\0Very Important Alert Rule!");
    assert_eq!(
        stored[0].stream_id,
        format!("sentry-issue-alert-{}", hex::encode(stream))
    );
    assert_eq!(stored[0].payload["issueId"], "1117540176");
}

/// axum's extractors cap bodies at 2 MB by default; the callback's limit is
/// Go's 10 MiB.
#[tokio::test]
async fn an_alert_over_2_mb_is_stored() {
    let app = start_app().await;

    let reply = app.post_signed(alert("e1", &"x".repeat(3 * MIB))).await;

    assert_eq!(reply, (200, "{\"status\":\"recorded\"}\n".to_owned()));
    assert_eq!(app.stop().await.len(), 1);
}

#[tokio::test]
async fn an_alert_over_10_mib_is_too_large() {
    let app = start_app().await;

    let reply = app.post_signed(alert("e1", &"x".repeat(10 * MIB))).await;

    assert_eq!(reply, (413, "{\"error\":\"body too large\"}\n".to_owned()));
    assert_eq!(app.stop().await, []);
}

#[tokio::test]
async fn a_body_cut_short_cannot_be_read() {
    let app = start_app().await;
    let mut conn = TcpStream::connect(app.addr).await.unwrap();

    conn.write_all(
        b"POST /sentry/callback HTTP/1.1\r\nHost: heinzel.test\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"data\":",
    )
    .await
    .unwrap();
    conn.shutdown().await.unwrap();
    let mut response = String::new();
    tokio::time::timeout(WAIT_LIMIT, conn.read_to_string(&mut response))
        .await
        .unwrap()
        .unwrap();

    assert!(response.starts_with("HTTP/1.1 400 "), "got {response:?}");
    assert!(
        response.ends_with("\r\n\r\n{\"error\":\"cannot read body\"}\n"),
        "got {response:?}"
    );
    assert_eq!(app.stop().await, []);
}
