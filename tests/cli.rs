//! The heinzel binary: flags, exit codes, JSON logs and graceful shutdown.

use std::process::Stdio;
use std::time::Duration;

use serde_json::Value;
use tempfile::TempDir;
use tokio::io::{AsyncBufReadExt, BufReader, Lines};
use tokio::process::{ChildStderr, Command};
use tokio::time::timeout;

const WAIT_LIMIT: Duration = Duration::from_secs(10);

/// The binary, configured with every required environment variable. The
/// investigator's work dir doesn't exist; it would only be created for an
/// investigation.
fn heinzel() -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_heinzel"));
    command
        .env("SENTRY_CLIENT_SECRET", "cli-test-secret")
        .env("SENTRY_GITHUB_REPOS", "2=acme/web")
        .env("GITHUB_TOKEN", "cli-test-github-token")
        .env("ANTHROPIC_API_KEY", "cli-test-anthropic-key")
        .env("OPENAI_API_KEY", "cli-test-openai-key")
        .env("CLAUDE_PLUGIN_DIRS", "/plugins/codex,/plugins/auto-task")
        .env(
            "INVESTIGATION_WORK_DIR",
            std::env::temp_dir().join(format!("heinzel-cli-test-{}", std::process::id())),
        )
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    command
}

/// Reads JSON log lines until one has `message`, skipping anything else.
async fn wait_for_log(lines: &mut Lines<BufReader<ChildStderr>>, message: &str) -> Value {
    let found = timeout(WAIT_LIMIT, async {
        while let Some(line) = lines.next_line().await.unwrap() {
            if let Ok(entry) = serde_json::from_str::<Value>(&line)
                && entry["message"] == message
            {
                return Some(entry);
            }
        }
        None
    });
    match found.await {
        Ok(Some(entry)) => entry,
        _ => panic!("no log line {message:?}"),
    }
}

#[tokio::test]
async fn help_exits_zero() {
    let output = heinzel().arg("--help").output().await.unwrap();

    assert_eq!(output.status.code(), Some(0));
    assert!(String::from_utf8_lossy(&output.stdout).contains("--shutdown-timeout"));
}

#[tokio::test]
async fn invalid_arguments_exit_two_with_a_json_log_line() {
    let mut child = heinzel().arg("--nope").spawn().unwrap();
    let mut lines = BufReader::new(child.stderr.take().unwrap()).lines();

    let entry = wait_for_log(&mut lines, "invalid arguments").await;

    assert_eq!(entry["level"], "ERROR");
    assert!(
        entry["error"].as_str().unwrap().contains("--nope"),
        "got {entry}"
    );
    let status = timeout(WAIT_LIMIT, child.wait()).await.unwrap().unwrap();
    assert_eq!(status.code(), Some(2));
}

#[tokio::test]
async fn invalid_configuration_exits_two_with_a_json_log_line() {
    let dir = TempDir::new().unwrap();
    let mut child = heinzel()
        .env_remove("SENTRY_CLIENT_SECRET")
        .args(["--addr", "127.0.0.1:0", "--data-dir"])
        .arg(dir.path())
        .spawn()
        .unwrap();
    let mut lines = BufReader::new(child.stderr.take().unwrap()).lines();

    let entry = wait_for_log(&mut lines, "invalid configuration").await;

    assert_eq!(entry["level"], "ERROR");
    assert_eq!(entry["error"], "SENTRY_CLIENT_SECRET must be set");
    let status = timeout(WAIT_LIMIT, child.wait()).await.unwrap().unwrap();
    assert_eq!(status.code(), Some(2));
}

#[tokio::test]
async fn sigterm_shuts_down_gracefully() {
    let dir = TempDir::new().unwrap();
    let mut child = heinzel()
        .args(["--addr", "127.0.0.1:0", "--data-dir"])
        .arg(dir.path())
        .spawn()
        .unwrap();
    let mut lines = BufReader::new(child.stderr.take().unwrap()).lines();
    let started = wait_for_log(&mut lines, "app started").await;
    assert_eq!(started["dataDir"], dir.path().display().to_string());

    let pid = child.id().unwrap().to_string();
    let killed = Command::new("kill")
        .args(["-TERM", &pid])
        .status()
        .await
        .unwrap();
    assert!(killed.success());

    wait_for_log(&mut lines, "app shutting down").await;
    let status = timeout(WAIT_LIMIT, child.wait()).await.unwrap().unwrap();
    assert_eq!(status.code(), Some(0));
}
