//! The investigator's decisions, in memory: given the events already on the
//! issue's stream, when an alert arrives and git, Claude and GitHub behave a
//! certain way, then the alert adds exactly these events to the stream, and
//! none to any other stream.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use chrono::{DateTime, TimeDelta, TimeZone, Utc};
use serde::Serialize;
use serde_json::{Value, json};
use tempfile::TempDir;
use tokio_util::sync::CancellationToken;

use crate::eventhandler::Handler;
use crate::events::{
    self, INVESTIGATION_FAILED_CLAUDE, INVESTIGATION_FAILED_CLONE, INVESTIGATION_FAILED_PR,
    INVESTIGATION_FAILED_PUSH, INVESTIGATION_FAILED_SHUTDOWN, INVESTIGATION_FAILED_TIMEOUT,
    INVESTIGATION_SKIPPED_STALE, INVESTIGATION_SKIPPED_UNMAPPED, SentryIssueAlertReceived,
    SentryStackFrame,
};
use crate::eventstore::{Event, NewEvent, Store};

use super::config::Repo;
use super::flow::{ClaudeFailure, Cloner, Investigator, PrOpener, PullRequest, Run};
use super::names::stream_id;
use super::pr::{pr_body, pr_title};
use super::prompt::investigation_prompt;
use super::test_logs::{Logs, capture_logs};

const ISSUE_ID: &str = "1117540176";
const BRANCH: &str = "heinzel/sentry-1117540176";
const PR_URL: &str = "https://github.com/acme/web/pull/7";
const SUMMARY: &str = "Root cause: blooopy was never declared. Fix: declare it.";
const MAPPED_PROJECT: i64 = 1;
const UNMAPPED_PROJECT: i64 = 99;

/// When the handler started; alerts stored more than [`max_age`] before it
/// are stale.
fn now() -> DateTime<Utc> {
    Utc.with_ymd_and_hms(2026, 10, 25, 12, 0, 0).unwrap()
}

fn max_age() -> TimeDelta {
    TimeDelta::hours(1)
}

// The scenarios.

#[tokio::test]
async fn a_fix_that_can_be_pushed_opens_a_draft_pr() {
    let s = given(&[])
        .when(an_alert(), [])
        .await
        .then(&[started(), pr_opened(PR_URL)]);

    let alert = decode(an_alert());
    let infra = s.infra();
    assert_eq!(infra.prompt.as_deref(), Some(investigation_prompt(&alert, BRANCH).as_str()));
    assert_eq!(
        infra.pr,
        Some(PullRequest {
            title: pr_title(&alert),
            head: BRANCH.into(),
            base: "main".into(),
            body: pr_body(&alert, SUMMARY),
        })
    );
    drop(infra);
    s.and_the_workspace_is_removed()
        .and_logged(json!({
            "level": "INFO", "message": "sentry issue investigation started",
            "handler": "sentry-issue-investigator", "position": 5, "issueId": ISSUE_ID,
            "repo": "acme/web", "branch": BRANCH,
        }))
        .and_logged(json!({
            "level": "INFO", "message": "sentry issue investigation finished",
            "handler": "sentry-issue-investigator", "position": 5, "issueId": ISSUE_ID,
            "outcome": "pr_opened", "prUrl": PR_URL,
        }));
}

#[tokio::test]
async fn no_commits_mean_no_changes() {
    given(&[])
        .when(an_alert(), [claude_commits_nothing()])
        .await
        .then(&[started(), no_changes()])
        .and_nothing_is_pushed()
        .and_the_workspace_is_removed()
        .and_logged(json!({
            "level": "INFO", "message": "sentry issue investigation finished",
            "handler": "sentry-issue-investigator", "position": 5, "issueId": ISSUE_ID,
            "outcome": "no_changes",
        }));
}

#[tokio::test]
async fn a_stale_alert_is_skipped() {
    given(&[])
        .when(a_stale_alert(), [])
        .await
        .then(&[skipped(INVESTIGATION_SKIPPED_STALE)])
        .and_nothing_is_cloned();
}

#[tokio::test]
async fn an_alert_for_an_unmapped_project_is_skipped() {
    given(&[])
        .when(an_unmapped_alert(), [])
        .await
        .then(&[skipped(INVESTIGATION_SKIPPED_UNMAPPED)])
        .and_nothing_is_cloned()
        .and_logged(json!({
            "level": "INFO", "message": "sentry issue investigation finished",
            "handler": "sentry-issue-investigator", "position": 5, "issueId": ISSUE_ID,
            "outcome": "skipped", "reason": "unmapped",
        }));
}

#[tokio::test]
async fn an_alert_both_stale_and_unmapped_is_skipped_as_stale() {
    given(&[])
        .when(a_stale_unmapped_alert(), [])
        .await
        .then(&[skipped(INVESTIGATION_SKIPPED_STALE)])
        .and_nothing_is_cloned();
}

#[tokio::test]
async fn an_alert_for_an_issue_already_investigated_adds_nothing() {
    given(&[started(), no_changes()])
        .when(an_alert(), [])
        .await
        .then(&[])
        .and_nothing_is_cloned()
        .and_logged(json!({"message": "sentry issue already handled", "outcome": "duplicate", "issueId": ISSUE_ID}));
}

#[tokio::test]
async fn an_alert_for_an_issue_already_skipped_adds_nothing() {
    given(&[skipped(INVESTIGATION_SKIPPED_UNMAPPED)])
        .when(an_unmapped_alert(), [])
        .await
        .then(&[]);
}

#[tokio::test]
async fn a_failed_clone_fails_the_investigation() {
    let reason = "git clone: exit status 128: repository not found";
    given(&[])
        .when(an_alert(), [clone_fails(reason)])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_CLONE, reason)]);
}

#[tokio::test]
async fn a_failed_claude_run_fails_the_investigation() {
    let reason = "claude: exit status 1: model refused";
    given(&[])
        .when(an_alert(), [claude_fails(reason)])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_CLAUDE, reason)])
        .and_nothing_is_pushed()
        .and_the_workspace_is_removed();
}

#[tokio::test]
async fn commits_on_other_branches_fail_the_investigation() {
    given(&[])
        .when(an_alert(), [claude_commits_on(&["claude-own", "claude-renamed"])])
        .await
        .then(&[
            started(),
            failed(
                INVESTIGATION_FAILED_CLAUDE,
                &format!("claude committed on branches other than {BRANCH}, which heinzel doesn't push: claude-own, claude-renamed"),
            ),
        ])
        .and_nothing_is_pushed();
}

#[tokio::test]
async fn claude_running_out_of_time_fails_the_investigation() {
    given(&[])
        .when(an_alert(), [claude_times_out()])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_TIMEOUT, "claude did not finish within 30m0s")])
        .and_nothing_is_pushed()
        .and_the_workspace_is_removed();
}

#[tokio::test]
async fn a_changed_mirror_is_neither_fetched_into_nor_pushed() {
    let reason = "the mirror changed while claude ran (config), so heinzel won't push from it";
    let s = given(&[])
        .when(an_alert(), [claude_changes_the_mirror(reason)])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_PUSH, reason)])
        .and_nothing_is_pushed()
        .and_the_workspace_is_removed();

    assert!(!s.infra().listed_branches, "fetched claude's branches into the mirror");
}

#[tokio::test]
async fn failing_to_list_branches_fails_the_investigation() {
    given(&[])
        .when(an_alert(), [listing_branches_fails("git for-each-ref: exit status 128")])
        .await
        .then(&[
            started(),
            failed(INVESTIGATION_FAILED_CLAUDE, "collect claude's commits: git for-each-ref: exit status 128"),
        ])
        .and_nothing_is_pushed();
}

#[tokio::test]
async fn failing_to_count_commits_fails_the_investigation() {
    given(&[])
        .when(an_alert(), [counting_commits_fails("git rev-list: exit status 128")])
        .await
        .then(&[
            started(),
            failed(INVESTIGATION_FAILED_CLAUDE, "collect claude's commits: git rev-list: exit status 128"),
        ])
        .and_nothing_is_pushed();
}

#[tokio::test]
async fn failing_to_list_changed_paths_fails_the_push() {
    given(&[])
        .when(an_alert(), [listing_changed_paths_fails("git diff: exit status 128")])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_PUSH, "git diff: exit status 128")])
        .and_nothing_is_pushed();
}

#[tokio::test]
async fn a_failed_push_fails_the_investigation() {
    let reason = "git push: exit status 1: rejected";
    given(&[])
        .when(an_alert(), [push_fails(reason)])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_PUSH, reason)])
        .and_no_pr_is_opened();
}

#[tokio::test]
async fn ci_config_changes_are_not_pushed() {
    given(&[])
        .when(
            an_alert(),
            [claude_changes(&[
                "src/app.js",
                ".github/workflows/a.yml",
                ".gitlab-ci.yml",
                "Jenkinsfile",
                ".circleci/config.yml",
                ".travis.yml",
                ".buildkite/pipeline.yml",
                "azure-pipelines.yml",
            ])],
        )
        .await
        .then(&[
            started(),
            failed(
                INVESTIGATION_FAILED_PUSH,
                "changes CI configuration, so heinzel won't push it: .github/workflows/a.yml, .gitlab-ci.yml, Jenkinsfile, .circleci/config.yml, .travis.yml, and 2 more",
            ),
        ])
        .and_nothing_is_pushed();
}

#[tokio::test]
async fn a_failed_pr_fails_the_investigation() {
    let reason = "create pull request: status 422: Validation Failed";
    given(&[])
        .when(an_alert(), [pr_fails(reason)])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_PR, reason)])
        .and_logged(json!({
            "level": "WARN", "message": "sentry issue investigation finished", "issueId": ISSUE_ID,
            "outcome": "failed", "stage": "pr", "reason": reason,
        }));
}

#[tokio::test]
async fn shutting_down_while_claude_runs_fails_the_investigation() {
    given(&[])
        .when(an_alert(), [heinzel_shuts_down_while_claude_runs()])
        .await
        .then(&[
            started(),
            failed(INVESTIGATION_FAILED_SHUTDOWN, "heinzel shut down while claude was running"),
        ])
        .and_nothing_is_pushed();
}

#[tokio::test]
async fn any_failure_during_shutdown_is_a_shutdown_failure() {
    given(&[])
        .when(an_alert(), [heinzel_shuts_down_during_the_push()])
        .await
        .then(&[started(), failed(INVESTIGATION_FAILED_SHUTDOWN, "git push: context canceled")]);
}

#[tokio::test]
async fn an_alert_picked_up_during_shutdown_adds_nothing() {
    given(&[])
        .heinzel_is_shutting_down()
        .when(an_alert(), [])
        .await
        .then(&[])
        .and_nothing_is_cloned()
        .and_logged(json!({"message": "heinzel is shutting down; sentry issue alert left unhandled", "issueId": ISSUE_ID}));
}

#[tokio::test]
async fn other_events_are_ignored() {
    let other = Event {
        event_type: "NoteRegistered".into(),
        payload: json!({}),
        ..an_alert()
    };

    let s = given(&[]).when(other, []).await.then(&[]).and_nothing_is_cloned();

    assert!(s.logs.lines().is_empty(), "logged {:?}", s.logs.lines());
}

#[tokio::test]
async fn an_undecodable_alert_is_logged_and_skipped() {
    let undecodable = Event {
        position: 3,
        payload: json!({"issueId": 1}),
        ..an_alert()
    };

    given(&[])
        .when(undecodable, [])
        .await
        .then(&[])
        .and_nothing_is_cloned()
        .and_logged(json!({
            "level": "ERROR", "message": "decode sentry issue alert",
            "handler": "sentry-issue-investigator", "position": 3,
        }));
}

#[tokio::test]
async fn a_store_failure_is_logged() {
    let s = given(&[]);
    s.store.close();

    s.when(an_alert(), [])
        .await
        .and_nothing_is_cloned()
        .and_logged(json!({
            "level": "ERROR", "message": "record sentry issue investigation",
            "handler": "sentry-issue-investigator", "position": 5, "issueId": ISSUE_ID,
            "type": "SentryIssueInvestigationStarted", "error": "event store is closed",
        }));
}

#[tokio::test]
async fn the_name_is_fixed() {
    let s = given(&[]);

    assert_eq!(s.investigator().name(), "sentry-issue-investigator");
}

// Given, when, then.

/// One given/when/then scenario.
struct Spec {
    store: Arc<Store>,
    cancel: CancellationToken,
    infra: Arc<Mutex<FakeInfra>>,
    logs: Logs,
    /// How many events the store held before the alert.
    before: usize,
    _dir: TempDir,
}

/// An event the alert should add, or one already on the stream.
struct Ev {
    event_type: &'static str,
    payload: Value,
}

fn ev(event_type: &'static str, payload: impl Serialize) -> Ev {
    Ev {
        event_type,
        payload: serde_json::to_value(payload).unwrap(),
    }
}

/// Starts a scenario whose issue stream already holds `prior`.
fn given(prior: &[Ev]) -> Spec {
    let dir = TempDir::new().unwrap();
    let store = Arc::new(Store::open(dir.path()).unwrap());
    if !prior.is_empty() {
        let events: Vec<NewEvent> = prior
            .iter()
            .map(|e| NewEvent {
                event_type: e.event_type.into(),
                payload: e.payload.clone(),
            })
            .collect();
        store.append(&stream_id(ISSUE_ID), 0, &events).unwrap();
    }
    let cancel = CancellationToken::new();
    Spec {
        store,
        infra: Arc::new(Mutex::new(FakeInfra::new(cancel.clone()))),
        cancel,
        logs: capture_logs(),
        before: prior.len(),
        _dir: dir,
    }
}

/// Changes how git, Claude or GitHub act in a scenario.
type Behaviour = Box<dyn FnOnce(&mut FakeInfra)>;

impl Spec {
    /// Cancels the handler's token before the alert.
    fn heinzel_is_shutting_down(self) -> Spec {
        self.cancel.cancel();
        self
    }

    fn investigator(&self) -> Investigator {
        Investigator {
            cancel: self.cancel.clone(),
            store: Arc::clone(&self.store),
            stale_before: now() - max_age(),
            repos: HashMap::from([(
                MAPPED_PROJECT,
                Repo {
                    owner: "acme".into(),
                    name: "web".into(),
                },
            )]),
            cloner: Box::new(FakeCloner(Arc::clone(&self.infra))),
            prs: Box::new(FakePrs(Arc::clone(&self.infra))),
        }
    }

    /// Hands `alert` to an investigator whose git, Claude and GitHub act as
    /// the behaviours say, and as for a fix that can be pushed otherwise.
    async fn when<const N: usize>(self, alert: Event, behaviours: [Behaviour; N]) -> Spec {
        for behaviour in behaviours {
            behaviour(&mut self.infra.lock().unwrap());
        }
        self.investigator().handle(alert).await;
        self
    }

    /// Checks that the alert added exactly `want` to the issue's stream, and
    /// nothing to any other stream.
    fn then(self, want: &[Ev]) -> Spec {
        let added: Vec<(String, String, Value)> = self
            .store
            .read_after(0, 1000)
            .unwrap()
            .into_iter()
            .skip(self.before)
            .map(|e| (e.stream_id, e.event_type, e.payload))
            .collect();
        let want: Vec<(String, String, Value)> = want
            .iter()
            .map(|e| (stream_id(ISSUE_ID), e.event_type.to_owned(), e.payload.clone()))
            .collect();
        assert_eq!(added, want, "logs: {:#?}", self.logs.lines());
        self
    }

    fn infra(&self) -> std::sync::MutexGuard<'_, FakeInfra> {
        self.infra.lock().unwrap()
    }

    fn and_nothing_is_cloned(self) -> Spec {
        assert!(!self.infra().cloned, "cloned the repo, want no clone");
        self
    }

    fn and_nothing_is_pushed(self) -> Spec {
        assert!(!self.infra().pushed, "pushed the branch, want no push");
        self.and_no_pr_is_opened()
    }

    fn and_no_pr_is_opened(self) -> Spec {
        assert_eq!(self.infra().pr, None, "opened a PR, want none");
        self
    }

    fn and_the_workspace_is_removed(self) -> Spec {
        assert!(self.infra().removed, "left the workspace behind");
        self
    }

    /// Checks that a log line holds all of `fields`.
    fn and_logged(self, fields: Value) -> Spec {
        let fields = fields.as_object().unwrap();
        let lines = self.logs.lines();
        let found = lines
            .iter()
            .any(|line| fields.iter().all(|(k, v)| line.get(k) == Some(v)));
        assert!(found, "no log line holds {fields:?}; logs: {lines:#?}");
        self
    }
}

// The events.

fn started() -> Ev {
    ev(
        events::SENTRY_ISSUE_INVESTIGATION_STARTED,
        events::SentryIssueInvestigationStarted {
            issue_id: ISSUE_ID.into(),
            project: MAPPED_PROJECT,
            repo: "acme/web".into(),
            branch: BRANCH.into(),
        },
    )
}

fn pr_opened(url: &str) -> Ev {
    ev(
        events::SENTRY_ISSUE_INVESTIGATION_PULL_REQUEST_OPENED,
        events::SentryIssueInvestigationPullRequestOpened {
            issue_id: ISSUE_ID.into(),
            pr_url: url.into(),
        },
    )
}

fn no_changes() -> Ev {
    ev(
        events::SENTRY_ISSUE_INVESTIGATION_NO_CHANGES,
        events::SentryIssueInvestigationNoChanges {
            issue_id: ISSUE_ID.into(),
        },
    )
}

fn skipped(reason: &str) -> Ev {
    ev(
        events::SENTRY_ISSUE_INVESTIGATION_SKIPPED,
        events::SentryIssueInvestigationSkipped {
            issue_id: ISSUE_ID.into(),
            reason: reason.into(),
        },
    )
}

fn failed(stage: &str, reason: &str) -> Ev {
    ev(
        events::SENTRY_ISSUE_INVESTIGATION_FAILED,
        events::SentryIssueInvestigationFailed {
            issue_id: ISSUE_ID.into(),
            stage: stage.into(),
            reason: reason.into(),
        },
    )
}

// The alerts.

/// A live alert for the mapped project.
fn an_alert() -> Event {
    alert_event(MAPPED_PROJECT, now())
}

/// An alert stored more than the max age before the handler started.
fn a_stale_alert() -> Event {
    alert_event(MAPPED_PROJECT, now() - max_age() - TimeDelta::minutes(1))
}

/// A live alert for a project mapped to no repo.
fn an_unmapped_alert() -> Event {
    alert_event(UNMAPPED_PROJECT, now())
}

/// A stale alert for a project mapped to no repo.
fn a_stale_unmapped_alert() -> Event {
    alert_event(UNMAPPED_PROJECT, now() - max_age() - TimeDelta::minutes(1))
}

fn alert_event(project: i64, stored: DateTime<Utc>) -> Event {
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
        position: 5,
        event_type: events::SENTRY_ISSUE_ALERT_RECEIVED.into(),
        timestamp: stored,
        payload: serde_json::to_value(alert).unwrap(),
    }
}

fn decode(event: Event) -> SentryIssueAlertReceived {
    serde_json::from_value(event.payload).unwrap()
}

// The behaviours.

fn clone_fails(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| f.clone_err = Some(reason))
}

fn claude_fails(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| {
        f.claude_err = Some(ClaudeFailure {
            reason,
            timed_out: false,
        });
    })
}

fn claude_times_out() -> Behaviour {
    Box::new(|f| {
        f.claude_err = Some(ClaudeFailure {
            reason: "claude did not finish within 30m0s".into(),
            timed_out: true,
        });
    })
}

fn claude_changes_the_mirror(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| f.mirror_err = Some(reason))
}

fn claude_commits_nothing() -> Behaviour {
    Box::new(|f| f.commits = 0)
}

fn claude_commits_on(branches: &[&str]) -> Behaviour {
    let branches: Vec<String> = branches.iter().map(|b| (*b).to_owned()).collect();
    Box::new(move |f| f.stray = branches)
}

fn claude_changes(paths: &[&str]) -> Behaviour {
    let paths: Vec<String> = paths.iter().map(|p| (*p).to_owned()).collect();
    Box::new(move |f| f.changed = paths)
}

fn listing_branches_fails(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| f.stray_err = Some(reason))
}

fn counting_commits_fails(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| f.commits_err = Some(reason))
}

fn listing_changed_paths_fails(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| f.changed_err = Some(reason))
}

fn push_fails(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| f.push_err = Some(reason))
}

fn pr_fails(reason: &str) -> Behaviour {
    let reason = reason.to_owned();
    Box::new(move |f| f.pr_err = Some(reason))
}

fn heinzel_shuts_down_while_claude_runs() -> Behaviour {
    Box::new(|f| {
        f.shut_down_during_claude = true;
        f.claude_err = Some(ClaudeFailure {
            reason: "heinzel shut down while claude was running".into(),
            timed_out: false,
        });
    })
}

fn heinzel_shuts_down_during_the_push() -> Behaviour {
    Box::new(|f| {
        f.shut_down_during_push = true;
        f.push_err = Some("git push: context canceled".into());
    })
}

// The fakes.

/// Stands in for git, Claude and GitHub, and notes what the investigator
/// asked of them. By default Claude commits a fix that touches no CI config
/// and everything succeeds.
struct FakeInfra {
    cancel: CancellationToken,
    clone_err: Option<String>,
    claude_err: Option<ClaudeFailure>,
    shut_down_during_claude: bool,
    mirror_err: Option<String>,
    stray: Vec<String>,
    stray_err: Option<String>,
    commits: u64,
    commits_err: Option<String>,
    changed: Vec<String>,
    changed_err: Option<String>,
    push_err: Option<String>,
    shut_down_during_push: bool,
    pr_err: Option<String>,

    cloned: bool,
    prompt: Option<String>,
    listed_branches: bool,
    pushed: bool,
    pr: Option<PullRequest>,
    removed: bool,
}

impl FakeInfra {
    fn new(cancel: CancellationToken) -> FakeInfra {
        FakeInfra {
            cancel,
            clone_err: None,
            claude_err: None,
            shut_down_during_claude: false,
            mirror_err: None,
            stray: Vec::new(),
            stray_err: None,
            commits: 1,
            commits_err: None,
            changed: vec!["src/app.js".into()],
            changed_err: None,
            push_err: None,
            shut_down_during_push: false,
            pr_err: None,
            cloned: false,
            prompt: None,
            listed_branches: false,
            pushed: false,
            pr: None,
            removed: false,
        }
    }
}

fn acme_web() -> Repo {
    Repo {
        owner: "acme".into(),
        name: "web".into(),
    }
}

struct FakeCloner(Arc<Mutex<FakeInfra>>);

#[async_trait]
impl Cloner for FakeCloner {
    async fn clone_repo(
        &self,
        issue_id: &str,
        target: &Repo,
        branch: &str,
        _: &CancellationToken,
    ) -> Result<Box<dyn Run>, String> {
        assert_eq!((issue_id, target, branch), (ISSUE_ID, &acme_web(), BRANCH));
        let mut f = self.0.lock().unwrap();
        if let Some(err) = f.clone_err.clone() {
            return Err(err);
        }
        f.cloned = true;
        Ok(Box::new(FakeRun(Arc::clone(&self.0))))
    }
}

struct FakeRun(Arc<Mutex<FakeInfra>>);

#[async_trait]
impl Run for FakeRun {
    async fn claude(&mut self, prompt: &str, _: &CancellationToken) -> Result<String, ClaudeFailure> {
        let mut f = self.0.lock().unwrap();
        f.prompt = Some(prompt.to_owned());
        if f.shut_down_during_claude {
            f.cancel.cancel();
        }
        f.claude_err.clone().map_or(Ok(SUMMARY.to_owned()), Err)
    }

    fn check_mirror(&self) -> Result<(), String> {
        self.0.lock().unwrap().mirror_err.clone().map_or(Ok(()), Err)
    }

    async fn stray_branches(&mut self, _: &CancellationToken) -> Result<Vec<String>, String> {
        let mut f = self.0.lock().unwrap();
        f.listed_branches = true;
        f.stray_err.clone().map_or(Ok(f.stray.clone()), Err)
    }

    async fn new_commits(&mut self, _: &CancellationToken) -> Result<u64, String> {
        let f = self.0.lock().unwrap();
        f.commits_err.clone().map_or(Ok(f.commits), Err)
    }

    async fn changed_paths(&mut self, _: &CancellationToken) -> Result<Vec<String>, String> {
        let f = self.0.lock().unwrap();
        f.changed_err.clone().map_or(Ok(f.changed.clone()), Err)
    }

    async fn push(&mut self, _: &CancellationToken) -> Result<(), String> {
        let mut f = self.0.lock().unwrap();
        if f.shut_down_during_push {
            f.cancel.cancel();
        }
        if let Some(err) = f.push_err.clone() {
            return Err(err);
        }
        f.pushed = true;
        Ok(())
    }

    fn default_branch(&self) -> String {
        "main".into()
    }

    async fn remove(&mut self) {
        self.0.lock().unwrap().removed = true;
    }
}

struct FakePrs(Arc<Mutex<FakeInfra>>);

#[async_trait]
impl PrOpener for FakePrs {
    async fn open_draft(&self, target: &Repo, pr: &PullRequest, _: &CancellationToken) -> Result<String, String> {
        assert_eq!(target, &acme_web());
        let mut f = self.0.lock().unwrap();
        if let Some(err) = f.pr_err.clone() {
            return Err(err);
        }
        f.pr = Some(pr.clone());
        Ok(PR_URL.into())
    }
}
