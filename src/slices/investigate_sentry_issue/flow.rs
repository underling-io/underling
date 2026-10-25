//! The investigator's whole decision path, from alert to recorded outcome.
//! It reaches git, Claude and GitHub only through the small traits below.

use std::collections::HashMap;
use std::sync::Arc;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use tokio_util::sync::CancellationToken;
use tracing::{error, info};

use crate::eventhandler::Handler;
use crate::events::{
    INVESTIGATION_FAILED_CLAUDE, INVESTIGATION_FAILED_CLONE, INVESTIGATION_FAILED_PR,
    INVESTIGATION_FAILED_PUSH, INVESTIGATION_FAILED_SHUTDOWN, INVESTIGATION_FAILED_TIMEOUT,
    INVESTIGATION_SKIPPED_STALE, INVESTIGATION_SKIPPED_UNMAPPED, SENTRY_ISSUE_ALERT_RECEIVED,
    SentryIssueAlertReceived,
};
use crate::eventstore::{Event, Store};

use super::HANDLER_NAME;
use super::ci_config::{MAX_LISTED_PATHS, ci_config_paths, first_n};
use super::config::Repo;
use super::names::{branch_name, stream_id};
use super::outcome::{Outcome, Recorder};
use super::pr::{pr_body, pr_title};
use super::prompt::investigation_prompt;

/// Investigates each new Sentry issue once.
pub(super) struct Investigator {
    /// Cancelled when heinzel shuts down.
    pub(super) cancel: CancellationToken,
    pub(super) store: Arc<Store>,
    /// The handler's start time minus the max age. Alerts stored before it
    /// are past alerts, never investigated; alerts stored later are live,
    /// however long the backlog delays them.
    pub(super) stale_before: DateTime<Utc>,
    /// By Sentry project id.
    pub(super) repos: HashMap<i64, Repo>,
    pub(super) cloner: Box<dyn Cloner>,
    pub(super) prs: Box<dyn PrOpener>,
}

#[async_trait]
impl Handler for Investigator {
    fn name(&self) -> &str {
        HANDLER_NAME
    }

    /// Ignores other event types and, since it cannot fail, logs and skips
    /// payloads it cannot decode. Every investigation ends in a recorded
    /// outcome; failures never escape.
    async fn handle(&mut self, event: Event) {
        if event.event_type != SENTRY_ISSUE_ALERT_RECEIVED {
            return;
        }
        let position = event.position;
        let alert: SentryIssueAlertReceived = match serde_json::from_value(event.payload) {
            Ok(alert) => alert,
            Err(err) => {
                error!(handler = HANDLER_NAME, position, error = %err, "decode sentry issue alert");
                return;
            }
        };
        let recorder = Recorder {
            store: &self.store,
            stream: stream_id(&alert.issue_id),
            position,
            issue_id: &alert.issue_id,
        };

        if event.timestamp < self.stale_before {
            recorder.record(0, &Outcome::Skipped { reason: INVESTIGATION_SKIPPED_STALE });
            return;
        }
        let Some(target) = self.repos.get(&alert.project) else {
            recorder.record(0, &Outcome::Skipped { reason: INVESTIGATION_SKIPPED_UNMAPPED });
            return;
        };

        let branch = branch_name(&alert.issue_id);
        let Some(started) = recorder.started_event(&alert, target, &branch) else {
            return;
        };
        if self.cancel.is_cancelled() {
            // Record nothing: a Started now would only end in Failed
            // shutdown, which is final. Note that the runner still
            // checkpoints past the alert, so only a later alert for the
            // issue starts it.
            info!(
                handler = HANDLER_NAME, position, issueId = %alert.issue_id,
                "heinzel is shutting down; sentry issue alert left unhandled"
            );
            return;
        }
        // A conflict here means another alert for the issue got there first.
        if !recorder.append(0, started) {
            return;
        }
        info!(
            handler = HANDLER_NAME, position, issueId = %alert.issue_id, repo = %target, branch,
            "sentry issue investigation started"
        );
        let outcome = self.investigate(&alert, target, &branch).await;
        recorder.record(1, &outcome);
    }
}

/// Where and why an investigation failed.
struct Failure {
    stage: &'static str,
    reason: String,
}

impl Failure {
    fn new(stage: &'static str, reason: impl Into<String>) -> Failure {
        Failure {
            stage,
            reason: reason.into(),
        }
    }
}

impl Investigator {
    /// Clones `target`, lets Claude work on `branch`, and pushes and opens a
    /// draft PR if it committed anything. Once heinzel shuts down, any
    /// failure is a shutdown failure.
    async fn investigate(
        &self,
        alert: &SentryIssueAlertReceived,
        target: &Repo,
        branch: &str,
    ) -> Outcome {
        let result = match self
            .cloner
            .clone_repo(&alert.issue_id, target, branch, &self.cancel)
            .await
        {
            Ok(mut run) => {
                let result = self.work_in(run.as_mut(), alert, target, branch).await;
                run.remove().await;
                result
            }
            Err(reason) => Err(Failure::new(INVESTIGATION_FAILED_CLONE, reason)),
        };
        match result {
            Ok(outcome) => outcome,
            Err(failure) => Outcome::Failed {
                stage: if self.cancel.is_cancelled() {
                    INVESTIGATION_FAILED_SHUTDOWN
                } else {
                    failure.stage
                },
                reason: failure.reason,
            },
        }
    }

    /// Lets Claude work in `run`'s clone, then checks what it committed and
    /// pushes it and opens the draft PR.
    async fn work_in(
        &self,
        run: &mut dyn Run,
        alert: &SentryIssueAlertReceived,
        target: &Repo,
        branch: &str,
    ) -> Result<Outcome, Failure> {
        let cancel = &self.cancel;
        let summary = run
            .claude(&investigation_prompt(alert, branch), cancel)
            .await
            .map_err(|failure| {
                let stage = if failure.timed_out {
                    INVESTIGATION_FAILED_TIMEOUT
                } else {
                    INVESTIGATION_FAILED_CLAUDE
                };
                Failure::new(stage, failure.reason)
            })?;
        // Claude can't reach heinzel's mirror through its sandbox; if it did
        // anyway, nothing is fetched into the mirror or pushed from it.
        run.check_mirror()
            .map_err(|reason| Failure::new(INVESTIGATION_FAILED_PUSH, reason))?;

        let collect = |err| Failure::new(INVESTIGATION_FAILED_CLAUDE, format!("collect claude's commits: {err}"));
        let stray = run.stray_branches(cancel).await.map_err(collect)?;
        if !stray.is_empty() {
            return Err(Failure::new(
                INVESTIGATION_FAILED_CLAUDE,
                format!(
                    "claude committed on branches other than {branch}, which heinzel doesn't push: {}",
                    stray.join(", ")
                ),
            ));
        }
        let commits = run.new_commits(cancel).await.map_err(collect)?;
        if commits == 0 {
            return Ok(Outcome::NoChanges);
        }

        let changed = run
            .changed_paths(cancel)
            .await
            .map_err(|reason| Failure::new(INVESTIGATION_FAILED_PUSH, reason))?;
        let ci = ci_config_paths(&changed);
        if !ci.is_empty() {
            return Err(Failure::new(
                INVESTIGATION_FAILED_PUSH,
                format!(
                    "changes CI configuration, so heinzel won't push it: {}",
                    first_n(&ci, MAX_LISTED_PATHS).join(", ")
                ),
            ));
        }
        run.push(cancel)
            .await
            .map_err(|reason| Failure::new(INVESTIGATION_FAILED_PUSH, reason))?;

        let pr = PullRequest {
            title: pr_title(alert),
            head: branch.to_owned(),
            base: run.default_branch(),
            body: pr_body(alert, &summary),
        };
        let url = self
            .prs
            .open_draft(target, &pr, cancel)
            .await
            .map_err(|reason| Failure::new(INVESTIGATION_FAILED_PR, reason))?;
        Ok(Outcome::PrOpened { url })
    }
}

/// Gets each investigation a fresh clone to work in.
#[async_trait]
pub(super) trait Cloner: Send + Sync {
    /// Clones `target` and checks out `branch` for Claude to work on.
    async fn clone_repo(
        &self,
        issue_id: &str,
        target: &Repo,
        branch: &str,
        cancel: &CancellationToken,
    ) -> Result<Box<dyn Run>, String>;
}

/// One investigation's clone, and Claude working in it.
#[async_trait]
pub(super) trait Run: Send {
    /// Runs Claude with `prompt` and returns its summary.
    async fn claude(
        &mut self,
        prompt: &str,
        cancel: &CancellationToken,
    ) -> Result<String, ClaudeFailure>;
    /// Fails if heinzel's mirror of the repo was changed while Claude ran.
    fn check_mirror(&self) -> Result<(), String>;
    /// Lists the branches other than the investigation branch that hold
    /// commits heinzel won't push.
    async fn stray_branches(&mut self, cancel: &CancellationToken) -> Result<Vec<String>, String>;
    /// Counts the investigation branch's new commits. It runs after
    /// `stray_branches`.
    async fn new_commits(&mut self, cancel: &CancellationToken) -> Result<u64, String>;
    /// Lists the paths those commits change.
    async fn changed_paths(&mut self, cancel: &CancellationToken) -> Result<Vec<String>, String>;
    /// Pushes the investigation branch to GitHub.
    async fn push(&mut self, cancel: &CancellationToken) -> Result<(), String>;
    /// The repo's default branch, which the PR targets.
    fn default_branch(&self) -> String;
    /// Deletes the clone, logging any failure.
    async fn remove(&mut self);
}

/// A failed Claude run: why, and whether it ran past its time limit.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct ClaudeFailure {
    pub(super) reason: String,
    pub(super) timed_out: bool,
}

/// Opens draft pull requests on GitHub.
#[async_trait]
pub(super) trait PrOpener: Send + Sync {
    /// Opens `pr` as a draft in `target` and returns its URL.
    async fn open_draft(
        &self,
        target: &Repo,
        pr: &PullRequest,
        cancel: &CancellationToken,
    ) -> Result<String, String>;
}

/// A pull request to open.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct PullRequest {
    pub(super) title: String,
    pub(super) head: String,
    pub(super) base: String,
    pub(super) body: String,
}
