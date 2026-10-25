//! The claude command and settings, and runs of fake claudes: small shell
//! scripts that act out one behaviour each.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::time::{Duration, SystemTime};

use serde_json::{Value, json};
use tempfile::TempDir;
use tokio_util::sync::CancellationToken;

use super::run::{DRAIN_TIMEOUT, check_init, go_duration};
use super::stream::InitEvent;
use super::*;

/// Only guards against hangs; a heavily loaded machine runs even the fake
/// shell scripts slowly.
const WAIT_LIMIT: Duration = Duration::from_secs(30);

fn test_config() -> Config {
    Config {
        bin: "/opt/claude/bin/claude".into(),
        plugin_dirs: vec!["/p/codex".into(), "/p/auto-task".into()],
        path: "/usr/bin:/bin".into(),
        anthropic_key: "anthropic-key".into(),
        openai_key: "openai-key".into(),
        parent_home: "/home/heinzel".into(),
        deny_read: vec!["/srv/heinzel-data".into()],
        readable: vec!["/p/codex".into(), "/p/auto-task".into(), "/opt/claude/bin".into()],
    }
}

fn test_workspace() -> Workspace {
    let mut ws = Workspace::default();
    ws.work_dir = "/w".into();
    ws.siblings = vec!["/w/old-run".into()];
    ws.run_dir = "/w/run".into();
    ws.mirror = "/w/run/mirror.git".into();
    ws.clone = "/w/run/repo".into();
    ws.home = "/w/run/home".into();
    ws.claude_config = "/w/run/claude-config".into();
    ws.git_home = "/w/run/git-home".into();
    ws.branch = "heinzel/sentry-42".into();
    ws
}

#[test]
fn the_command_line_is_go_s() {
    let cmd = command(&test_config(), &test_workspace(), "the prompt");

    let settings = sandbox_settings(&test_config(), &test_workspace());
    let want = [
        "--bare",
        "-p",
        "--verbose",
        "--output-format",
        "stream-json",
        "--no-session-persistence",
        "--setting-sources",
        "",
        "--settings",
        &settings,
        "--permission-mode",
        "acceptEdits",
        "--permission-prompts",
        "none",
        "--plugin-dir",
        "/p/codex",
        "--plugin-dir",
        "/p/auto-task",
    ];
    assert_eq!(cmd.bin, "/opt/claude/bin/claude");
    assert_eq!(cmd.dir, "/w/run/repo");
    assert_eq!(cmd.args, want);
    assert_eq!(cmd.stdin, "the prompt");
    assert_eq!(cmd.plugin_dirs, ["/p/codex", "/p/auto-task"]);
}

#[test]
fn the_env_holds_only_what_the_child_needs() {
    let cmd = command(&test_config(), &test_workspace(), "p");

    let env: Vec<String> = cmd.env.iter().map(|(k, v)| format!("{k}={v}")).collect();
    assert_eq!(
        env,
        [
            "PATH=/usr/bin:/bin",
            "HOME=/w/run/home",
            "CLAUDE_CONFIG_DIR=/w/run/claude-config",
            "ANTHROPIC_API_KEY=anthropic-key",
            "OPENAI_API_KEY=openai-key",
        ]
    );
}

#[test]
fn debug_output_hides_the_keys() {
    let cmd = command(&test_config(), &test_workspace(), "p");

    for debug in [format!("{:?}", test_config()), format!("{cmd:?}")] {
        assert!(!debug.contains("anthropic-key") && !debug.contains("openai-key"), "{debug}");
    }
}

#[test]
fn the_settings_sandbox_the_clone_byte_for_byte() {
    let got = sandbox_settings(&test_config(), &test_workspace());

    let want = concat!(
        r#"{"permissions":{"defaultMode":"acceptEdits","disableBypassPermissionsMode":"disable","#,
        r#""deny":["Read(//proc/**)","Read(//home/heinzel/**)","Edit(//home/heinzel/**)","#,
        r#""Read(//srv/heinzel-data/**)","Edit(//srv/heinzel-data/**)","#,
        r#""Read(//w/old-run/**)","Edit(//w/old-run/**)","#,
        r#""Read(//w/run/mirror.git/**)","Edit(//w/run/mirror.git/**)","#,
        r#""Read(//w/run/git-home/**)","Edit(//w/run/git-home/**)"]},"#,
        r#""sandbox":{"enabled":true,"failIfUnavailable":true,"autoAllowBashIfSandboxed":true,"#,
        r#""allowUnsandboxedCommands":false,"filesystem":{"allowWrite":["/w/run/repo","/w/run/home"],"#,
        r#""denyRead":["/home/heinzel","/srv/heinzel-data","/w"],"#,
        r#""allowRead":["/w/run/repo","/w/run/home","/w/run/claude-config","/p/codex","/p/auto-task","/opt/claude/bin"]},"#,
        r#""network":{"allowedDomains":["api.openai.com"]}}}"#,
    );
    assert_eq!(got, want);
}

fn settings_json(cfg: &Config) -> Value {
    serde_json::from_str(&sandbox_settings(cfg, &test_workspace())).unwrap()
}

#[test]
fn the_settings_without_a_parent_home() {
    let mut cfg = test_config();
    cfg.parent_home = String::new();
    cfg.deny_read = Vec::new();

    let got = settings_json(&cfg);

    assert_eq!(got["sandbox"]["filesystem"]["denyRead"], json!(["/w"]));
    assert_eq!(
        got["permissions"]["deny"],
        json!([
            "Read(//proc/**)",
            "Read(//w/old-run/**)",
            "Edit(//w/old-run/**)",
            "Read(//w/run/mirror.git/**)",
            "Edit(//w/run/mirror.git/**)",
            "Read(//w/run/git-home/**)",
            "Edit(//w/run/git-home/**)",
        ])
    );
}

#[test]
fn the_settings_deny_work_dir_paths_as_configured_too() {
    let mut cfg = test_config();
    cfg.parent_home = String::new();
    cfg.deny_read = Vec::new();
    let mut ws = test_workspace();
    ws.given_work_dir = "/link/w".into();

    let got: Value = serde_json::from_str(&sandbox_settings(&cfg, &ws)).unwrap();

    assert_eq!(got["sandbox"]["filesystem"]["denyRead"], json!(["/link/w", "/w"]));
    assert_eq!(
        got["permissions"]["deny"],
        json!([
            "Read(//proc/**)",
            "Read(//link/w/old-run/**)",
            "Edit(//link/w/old-run/**)",
            "Read(//w/old-run/**)",
            "Edit(//w/old-run/**)",
            "Read(//link/w/run/mirror.git/**)",
            "Edit(//link/w/run/mirror.git/**)",
            "Read(//w/run/mirror.git/**)",
            "Edit(//w/run/mirror.git/**)",
            "Read(//link/w/run/git-home/**)",
            "Edit(//link/w/run/git-home/**)",
            "Read(//w/run/git-home/**)",
            "Edit(//w/run/git-home/**)",
        ])
    );
}

#[test]
fn the_settings_deny_symlinked_paths_and_their_targets() {
    let root = TempDir::new().unwrap();
    let target = root.path().join("data");
    fs::create_dir(&target).unwrap();
    let resolved = fs::canonicalize(&target).unwrap().to_str().unwrap().to_owned();
    let link = root.path().join("data-link");
    std::os::unix::fs::symlink(&target, &link).unwrap();
    let link = link.to_str().unwrap().to_owned();
    let mut cfg = test_config();
    cfg.parent_home = link.clone();
    cfg.deny_read = vec![link.clone(), "/srv/heinzel-data".into()];

    let got = settings_json(&cfg);

    assert_eq!(
        got["sandbox"]["filesystem"]["denyRead"],
        json!([link, resolved, "/srv/heinzel-data", "/w"])
    );
    let deny = got["permissions"]["deny"].as_array().unwrap();
    assert_eq!(
        deny[..7],
        [
            json!("Read(//proc/**)"),
            json!(format!("Read(/{link}/**)")),
            json!(format!("Edit(/{link}/**)")),
            json!(format!("Read(/{resolved}/**)")),
            json!(format!("Edit(/{resolved}/**)")),
            json!("Read(//srv/heinzel-data/**)"),
            json!("Edit(//srv/heinzel-data/**)"),
        ]
    );
}

#[test]
fn go_durations() {
    let cases = [
        (Duration::ZERO, "0s"),
        (Duration::from_secs(30 * 60), "30m0s"),
        (Duration::from_secs(3600), "1h0m0s"),
        (Duration::from_secs(90), "1m30s"),
        (Duration::from_millis(1500), "1.5s"),
        (Duration::from_millis(500), "500ms"),
        (Duration::from_micros(1500), "1.5ms"),
        (Duration::from_nanos(1500), "1.5µs"),
        (Duration::from_nanos(7), "7ns"),
        (Duration::from_millis(2 * 3_600_000 + 3 * 60_000 + 4_005), "2h3m4.005s"),
    ];
    for (d, want) in cases {
        assert_eq!(go_duration(d), want, "{d:?}");
    }
}

fn init(json: &str) -> InitEvent {
    serde_json::from_str(json).unwrap()
}

#[test]
fn the_init_check() {
    let plugins = ["/p/codex".to_owned(), "/p/at".to_owned()];
    let good = r#"{"permissionMode":"acceptEdits","plugins":[{"name":"codex","path":"/p/codex"},{"name":"at","path":"/p/at/plugins/at"}]}"#;
    let cases = [
        ("as configured", Some(good), None),
        ("missing", None, Some("no system/init event")),
        (
            "other permission mode",
            Some(r#"{"permissionMode":"default","plugins":[{"path":"/p/codex"},{"path":"/p/at"}]}"#),
            Some(r#"permission mode "default", not acceptEdits"#),
        ),
        (
            "plugin missing",
            Some(r#"{"permissionMode":"acceptEdits","plugins":[{"path":"/p/codex"}]}"#),
            Some("no plugin loaded from /p/at"),
        ),
        (
            "a sibling dir isn't inside",
            Some(r#"{"permissionMode":"acceptEdits","plugins":[{"path":"/p/codex"},{"path":"/p/atx"}]}"#),
            Some("no plugin loaded from /p/at"),
        ),
        (
            "plugin errors",
            Some(r#"{"permissionMode":"acceptEdits","plugins":[{"path":"/p/codex"},{"path":"/p/at"}],"plugin_errors":[{"plugin":"at","type":"hooks","message":"bad hooks file"},{"plugin":"x"}]}"#),
            Some(r#"plugin errors: {"plugin":"at","type":"hooks","message":"bad hooks file"}; {"plugin":"x"}"#),
        ),
    ];
    for (name, event, want) in cases {
        let event = event.map(init);

        let got = check_init(event.as_ref(), &plugins);

        assert_eq!(got.err().as_deref(), want, "{name}");
    }
}

// Runs of fake claudes.

const PLUGIN_DIRS: [&str; 2] = ["/plugins/codex", "/plugins/auto-task"];
const INIT: &str = r#"{"type":"system","subtype":"init","permissionMode":"acceptEdits","plugins":[{"name":"codex","path":"/plugins/codex"},{"name":"auto-task","path":"/plugins/auto-task"}]}"#;
const NOTHING_TO_FIX: &str =
    r#"{"type":"result","subtype":"success","is_error":false,"result":"Nothing to fix."}"#;

/// A fake claude: a shell script running `body` in a fresh dir.
struct Fake {
    dir: TempDir,
    cmd: Cmd,
}

fn fake(body: &str) -> Fake {
    let dir = TempDir::new().unwrap();
    let bin = dir.path().join("claude");
    fs::write(&bin, format!("#!/bin/sh\n{body}\n")).unwrap();
    fs::set_permissions(&bin, fs::Permissions::from_mode(0o755)).unwrap();
    let work = dir.path().join("work");
    fs::create_dir(&work).unwrap();
    let mut args = vec!["--bare".to_owned(), "-p".to_owned()];
    for d in PLUGIN_DIRS {
        args.extend(["--plugin-dir".to_owned(), d.to_owned()]);
    }
    let cmd = Cmd {
        bin: bin.to_str().unwrap().into(),
        args,
        env: vec![("PATH".into(), std::env::var("PATH").unwrap())],
        dir: work.to_str().unwrap().into(),
        stdin: "the prompt".into(),
        plugin_dirs: PLUGIN_DIRS.map(String::from).into(),
    };
    Fake { dir, cmd }
}

impl Fake {
    fn file(&self, name: &str) -> std::path::PathBuf {
        Path::new(&self.cmd.dir).join(name)
    }

    /// Runs the fake, failing the test if it doesn't return in time.
    async fn run(&self, timeout: Duration, cancel: &CancellationToken) -> Result<String, RunError> {
        tokio::time::timeout(WAIT_LIMIT, run(&self.cmd, timeout, cancel))
            .await
            .expect("run did not return in time")
    }

    async fn run_to_end(&self) -> Result<String, RunError> {
        self.run(Duration::from_secs(60), &CancellationToken::new()).await
    }

    /// Waits until the fake wrote `name` in its dir.
    async fn wait_for(&self, name: &str) {
        let path = self.file(name);
        tokio::time::timeout(WAIT_LIMIT, async {
            while !path.exists() {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("the fake never got there");
    }

    /// The pid the fake wrote in `name`.
    fn pid(&self, name: &str) -> nix::unistd::Pid {
        let text = fs::read_to_string(self.file(name)).unwrap();
        nix::unistd::Pid::from_raw(text.trim().parse().unwrap())
    }
}

/// Waits until `pid` is gone, failing the test if it outlives the wait.
async fn assert_gone(pid: nix::unistd::Pid) {
    let gone = tokio::time::timeout(WAIT_LIMIT, async {
        while nix::sys::signal::kill(pid, None).is_ok() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    assert!(gone.is_ok(), "process {pid} outlived the run");
}

#[tokio::test]
async fn the_prompt_goes_to_claude_and_its_summary_comes_back() {
    let f = fake(&format!(
        "prompt=$(cat)\necho '{INIT}'\nprintf '{{\"type\":\"result\",\"is_error\":false,\"result\":\"Summary of: %s\"}}\\n' \"$prompt\""
    ));

    assert_eq!(f.run_to_end().await, Ok("Summary of: the prompt".into()));
}

#[tokio::test]
async fn a_failing_claude_fails_with_its_result_and_stderr() {
    let f = fake(&format!(
        "cat >/dev/null\necho '{INIT}'\necho '{{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"result\":\"model refused\"}}'\necho 'fake claude failed' >&2\nexit 1"
    ));

    assert_eq!(
        f.run_to_end().await,
        Err(RunError::Failed("claude: exit status 1: model refused: fake claude failed".into()))
    );
}

#[tokio::test]
async fn an_unsuccessful_last_event_fails_the_run() {
    let f = fake(&format!("cat >/dev/null\necho '{INIT}'\necho '{{\"type\":\"assistant\"}}'\necho oops >&2"));

    assert_eq!(
        f.run_to_end().await,
        Err(RunError::Failed("claude did not end with a successful result: oops".into()))
    );
}

#[tokio::test]
async fn a_claude_that_does_not_start_fails_the_run() {
    let mut f = fake("");
    f.cmd.bin = f.dir.path().join("no-claude").to_str().unwrap().into();

    let err = f.run_to_end().await.unwrap_err().to_string();

    assert!(err.starts_with("start claude: "), "{err}");
}

#[tokio::test]
async fn a_session_that_did_not_start_as_configured_fails_the_run() {
    let cases = [
        ("", "no system/init event"),
        (
            r#"{"type":"system","subtype":"init","permissionMode":"default","plugins":[{"path":"/plugins/codex"},{"path":"/plugins/auto-task"}]}"#,
            r#"permission mode "default", not acceptEdits"#,
        ),
        (
            r#"{"type":"system","subtype":"init","permissionMode":"acceptEdits","plugins":[{"path":"/plugins/codex"}]}"#,
            "no plugin loaded from /plugins/auto-task",
        ),
        (
            r#"{"type":"system","subtype":"init","permissionMode":"acceptEdits","plugins":[{"path":"/plugins/codex"},{"path":"/plugins/auto-task"}],"plugin_errors":[{"plugin":"auto-task","type":"hooks","message":"hooks.json is invalid"}]}"#,
            r#"plugin errors: {"plugin":"auto-task","type":"hooks","message":"hooks.json is invalid"}"#,
        ),
    ];
    for (init_line, reason) in cases {
        let f = fake(&format!("cat >/dev/null\necho '{init_line}'\necho '{NOTHING_TO_FIX}'"));

        let got = f.run_to_end().await;

        assert_eq!(
            got,
            Err(RunError::Failed(format!("claude's session did not start as configured: {reason}"))),
        );
    }
}

#[tokio::test]
async fn claude_is_killed_after_the_timeout() {
    let f = fake("touch started\nexec sleep 3600");

    let err = f.run(Duration::from_millis(500), &CancellationToken::new()).await.unwrap_err();

    assert_eq!(err, RunError::Timeout(Duration::from_millis(500)));
    assert_eq!(err.to_string(), "claude did not finish within 500ms");
}

#[tokio::test]
async fn claude_is_killed_on_shutdown() {
    let f = fake("touch started\nexec sleep 3600");
    let cancel = CancellationToken::new();

    let (got, ()) = tokio::join!(f.run(Duration::from_secs(3600), &cancel), async {
        f.wait_for("started").await;
        cancel.cancel();
    });

    assert_eq!(got, Err(RunError::Shutdown));
}

#[tokio::test]
async fn shutdown_kills_claude_s_whole_process_group() {
    let f = fake("sleep 3600 &\necho $! > grandchild\ntouch started\nexec sleep 3600");
    let cancel = CancellationToken::new();

    let (got, ()) = tokio::join!(f.run(Duration::from_secs(3600), &cancel), async {
        f.wait_for("started").await;
        cancel.cancel();
    });

    assert_eq!(got, Err(RunError::Shutdown));
    assert_gone(f.pid("grandchild")).await;
}

#[tokio::test]
async fn claude_does_not_start_once_heinzel_is_shutting_down() {
    let f = fake("touch ran");
    let cancel = CancellationToken::new();
    cancel.cancel();

    let got = f.run(Duration::from_secs(60), &cancel).await;

    assert_eq!(got, Err(RunError::Shutdown));
    assert!(!f.file("ran").exists(), "claude ran");
}

#[tokio::test]
async fn dropping_the_run_kills_claude_s_process_group() {
    let f = fake("sleep 3600 &\necho $! > grandchild\ntouch started\nexec sleep 3600");
    let cancel = CancellationToken::new();

    tokio::select! {
        _ = run(&f.cmd, Duration::from_secs(3600), &cancel) => panic!("the run ended"),
        () = f.wait_for("started") => {}
    }

    assert_gone(f.pid("grandchild")).await;
}

#[tokio::test]
async fn only_the_tail_of_stderr_is_kept() {
    let f = fake("cat >/dev/null\nhead -c 100000 /dev/zero | tr '\\0' x >&2\necho END >&2\nexit 1");

    let err = f.run_to_end().await.unwrap_err().to_string();

    let tail = err.strip_prefix("claude: exit status 1: ").unwrap();
    assert_eq!(tail.len(), 2047, "the 2048-byte tail, trimmed");
    assert!(tail.ends_with("xxxEND"), "{}", &tail[tail.len() - 10..]);
}

#[tokio::test]
async fn a_leftover_grandchild_neither_delays_nor_outlives_the_run() {
    // The grandchild holds stdout and stderr open after claude exits. The
    // fake stamps `exiting` last, so the test can time the run from
    // claude's exit, whatever the fake's own start-up costs under load.
    let f = fake(&format!(
        "cat >/dev/null\n(sleep 60; echo late) &\necho $! > grandchild\necho '{INIT}'\necho '{NOTHING_TO_FIX}'\ntouch exiting"
    ));

    let got = f.run_to_end().await;
    let returned = SystemTime::now();

    assert_eq!(got, Ok("Nothing to fix.".into()));
    let exited = fs::metadata(f.file("exiting")).unwrap().modified().unwrap();
    let after_exit = returned.duration_since(exited).unwrap_or_default();
    // Waiting on the grandchild's pipes would take the whole drain timeout.
    assert!(after_exit < DRAIN_TIMEOUT, "returned {after_exit:?} after claude exited");
    assert_gone(f.pid("grandchild")).await;
}
