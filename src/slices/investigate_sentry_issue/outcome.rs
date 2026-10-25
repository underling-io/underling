//! How an investigation (or a skip) ends: the event recorded on the issue's
//! stream and the log line describing it.

use serde::Serialize;
use tracing::{error, info, warn};

use crate::events::{
    SENTRY_ISSUE_INVESTIGATION_FAILED, SENTRY_ISSUE_INVESTIGATION_NO_CHANGES,
    SENTRY_ISSUE_INVESTIGATION_PULL_REQUEST_OPENED, SENTRY_ISSUE_INVESTIGATION_SKIPPED,
    SENTRY_ISSUE_INVESTIGATION_STARTED, SentryIssueAlertReceived, SentryIssueInvestigationFailed,
    SentryIssueInvestigationNoChanges, SentryIssueInvestigationPullRequestOpened,
    SentryIssueInvestigationSkipped, SentryIssueInvestigationStarted,
};
use crate::eventstore::{self, NewEvent, Store};

use super::config::Repo;
use super::HANDLER_NAME;

/// How an investigation, or a skip, ended.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum Outcome {
    /// A draft PR was opened at `url`.
    PrOpened { url: String },
    /// Claude committed nothing.
    NoChanges,
    /// The issue was not investigated, for `reason`.
    Skipped { reason: &'static str },
    /// The investigation failed at `stage`.
    Failed { stage: &'static str, reason: String },
}

/// One alert's place in the store and in the logs: the issue's stream, and
/// the fields every log line about the alert carries.
pub(super) struct Recorder<'a> {
    pub(super) store: &'a Store,
    pub(super) stream: String,
    pub(super) position: u64,
    pub(super) issue_id: &'a str,
}

impl Recorder<'_> {
    /// Appends `outcome`'s event at `expected_version` and logs the outcome
    /// line. A version conflict means the issue was already handled.
    pub(super) fn record(&self, expected_version: u64, outcome: &Outcome) {
        let Some(event) = self.outcome_event(outcome) else {
            return;
        };
        if !self.append(expected_version, event) {
            return;
        }
        let (handler, position, issue_id) = (HANDLER_NAME, self.position, self.issue_id);
        match outcome {
            Outcome::PrOpened { url } => info!(
                handler, position, issueId = issue_id, outcome = "pr_opened", prUrl = %url,
                "sentry issue investigation finished"
            ),
            Outcome::NoChanges => info!(
                handler, position, issueId = issue_id, outcome = "no_changes",
                "sentry issue investigation finished"
            ),
            Outcome::Skipped { reason } => info!(
                handler, position, issueId = issue_id, outcome = "skipped", reason,
                "sentry issue investigation finished"
            ),
            Outcome::Failed { stage, reason } => warn!(
                handler, position, issueId = issue_id, outcome = "failed", stage, reason = %reason,
                "sentry issue investigation finished"
            ),
        }
    }

    /// Appends `event` and reports whether it was stored. A version conflict
    /// means another alert for the issue got there first.
    pub(super) fn append(&self, expected_version: u64, event: NewEvent) -> bool {
        let (handler, position, issue_id) = (HANDLER_NAME, self.position, self.issue_id);
        match self.store.append(&self.stream, expected_version, std::slice::from_ref(&event)) {
            Ok(_) => true,
            Err(eventstore::Error::Conflict { .. }) => {
                info!(handler, position, issueId = issue_id, outcome = "duplicate", "sentry issue already handled");
                false
            }
            Err(err) => {
                error!(
                    handler, position, issueId = issue_id, r#type = %event.event_type, error = %err,
                    "record sentry issue investigation"
                );
                false
            }
        }
    }

    /// The event that starts an investigation of `alert`'s issue in
    /// `target`, on `branch`.
    pub(super) fn started_event(
        &self,
        alert: &SentryIssueAlertReceived,
        target: &Repo,
        branch: &str,
    ) -> Option<NewEvent> {
        let payload = SentryIssueInvestigationStarted {
            issue_id: alert.issue_id.clone(),
            project: alert.project,
            repo: target.to_string(),
            branch: branch.to_owned(),
        };
        self.encode(SENTRY_ISSUE_INVESTIGATION_STARTED, &payload, "encode investigation started")
    }

    fn outcome_event(&self, outcome: &Outcome) -> Option<NewEvent> {
        let issue_id = self.issue_id.to_owned();
        let message = "encode investigation outcome";
        match outcome {
            Outcome::PrOpened { url } => self.encode(
                SENTRY_ISSUE_INVESTIGATION_PULL_REQUEST_OPENED,
                &SentryIssueInvestigationPullRequestOpened { issue_id, pr_url: url.clone() },
                message,
            ),
            Outcome::NoChanges => self.encode(
                SENTRY_ISSUE_INVESTIGATION_NO_CHANGES,
                &SentryIssueInvestigationNoChanges { issue_id },
                message,
            ),
            Outcome::Skipped { reason } => self.encode(
                SENTRY_ISSUE_INVESTIGATION_SKIPPED,
                &SentryIssueInvestigationSkipped { issue_id, reason: (*reason).to_owned() },
                message,
            ),
            Outcome::Failed { stage, reason } => self.encode(
                SENTRY_ISSUE_INVESTIGATION_FAILED,
                &SentryIssueInvestigationFailed {
                    issue_id,
                    stage: (*stage).to_owned(),
                    reason: reason.clone(),
                },
                message,
            ),
        }
    }

    /// Encodes `payload` as an event of `event_type`, logging `message` if
    /// that fails.
    fn encode(&self, event_type: &str, payload: &impl Serialize, message: &str) -> Option<NewEvent> {
        match serde_json::to_value(payload) {
            Ok(payload) => Some(NewEvent {
                event_type: event_type.to_owned(),
                payload,
            }),
            Err(err) => {
                let (handler, position, issue_id) = (HANDLER_NAME, self.position, self.issue_id);
                error!(handler, position, issueId = issue_id, error = %err, "{message}");
                None
            }
        }
    }
}
