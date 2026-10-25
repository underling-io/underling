//! The event contracts shared between slices: type names and payload shapes.
//! It holds no behaviour. The JSON shapes are persisted, so changing them is
//! a breaking change.

use serde::{Deserialize, Serialize};

/// The stored type of a [`SentryIssueAlertReceived`] event.
pub const SENTRY_ISSUE_ALERT_RECEIVED: &str = "SentryIssueAlertReceived";

/// Records that a Sentry issue alert rule fired for one Sentry event. The
/// exception details are optional: alerts recorded before they existed, and
/// events without an exception, leave them empty, and empty ones are left out
/// of the JSON.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SentryIssueAlertReceived {
    /// Sentry's event id.
    pub event_id: String,
    /// Sentry's issue id.
    pub issue_id: String,
    /// Sentry's project id.
    pub project: i64,
    /// The issue title.
    pub title: String,
    /// The event level, such as `error`.
    pub level: String,
    /// The event's page in Sentry.
    pub web_url: String,
    /// The name of the alert rule that fired.
    pub triggered_rule: String,
    /// Where Sentry thinks the error comes from.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub culprit: String,
    /// The type of the outermost (last) exception Sentry sent.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub exception_type: String,
    /// The value (message) of the outermost exception.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub exception_value: String,
    /// The outermost exception's frames, in Sentry's order, oldest call
    /// first, keeping at most the last 50.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub frames: Vec<SentryStackFrame>,
}

/// One stack frame of a Sentry exception.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct SentryStackFrame {
    /// The frame's file name.
    pub file: String,
    /// The frame's function.
    pub function: String,
    /// The frame's line number.
    pub line: i64,
}

// The Sentry issue investigation events all go on the stream of one Sentry
// issue: its first event (Started or Skipped) at version 1, and the outcome
// after Started at version 2.

/// The stored type of a [`SentryIssueInvestigationStarted`] event.
pub const SENTRY_ISSUE_INVESTIGATION_STARTED: &str = "SentryIssueInvestigationStarted";
/// The stored type of a [`SentryIssueInvestigationPullRequestOpened`] event.
pub const SENTRY_ISSUE_INVESTIGATION_PULL_REQUEST_OPENED: &str =
    "SentryIssueInvestigationPullRequestOpened";
/// The stored type of a [`SentryIssueInvestigationNoChanges`] event.
pub const SENTRY_ISSUE_INVESTIGATION_NO_CHANGES: &str = "SentryIssueInvestigationNoChanges";
/// The stored type of a [`SentryIssueInvestigationSkipped`] event.
pub const SENTRY_ISSUE_INVESTIGATION_SKIPPED: &str = "SentryIssueInvestigationSkipped";
/// The stored type of a [`SentryIssueInvestigationFailed`] event.
pub const SENTRY_ISSUE_INVESTIGATION_FAILED: &str = "SentryIssueInvestigationFailed";

/// Records that an investigation of a Sentry issue began: `repo`
/// (`owner/repo`) is cloned and Claude works on `branch`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SentryIssueInvestigationStarted {
    /// Sentry's issue id.
    pub issue_id: String,
    /// Sentry's project id.
    pub project: i64,
    /// The GitHub repo, `owner/repo`.
    pub repo: String,
    /// The branch Claude works on.
    pub branch: String,
}

/// Records the draft PR an investigation opened.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SentryIssueInvestigationPullRequestOpened {
    /// Sentry's issue id.
    pub issue_id: String,
    /// The pull request's URL.
    pub pr_url: String,
}

/// Records that an investigation ended without new commits, so no PR was
/// opened.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SentryIssueInvestigationNoChanges {
    /// Sentry's issue id.
    pub issue_id: String,
}

/// A [`SentryIssueInvestigationSkipped`] reason: no repo is mapped to the
/// alert's project.
pub const INVESTIGATION_SKIPPED_UNMAPPED: &str = "unmapped";
/// A [`SentryIssueInvestigationSkipped`] reason: the alert is older than the
/// max age.
pub const INVESTIGATION_SKIPPED_STALE: &str = "stale";

/// Records that a Sentry issue was not investigated, for `reason`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SentryIssueInvestigationSkipped {
    /// Sentry's issue id.
    pub issue_id: String,
    /// Why, one of the `INVESTIGATION_SKIPPED_*` reasons.
    pub reason: String,
}

/// A [`SentryIssueInvestigationFailed`] stage: cloning the repo.
pub const INVESTIGATION_FAILED_CLONE: &str = "clone";
/// A [`SentryIssueInvestigationFailed`] stage: running Claude.
pub const INVESTIGATION_FAILED_CLAUDE: &str = "claude";
/// A [`SentryIssueInvestigationFailed`] stage: the investigation timed out.
pub const INVESTIGATION_FAILED_TIMEOUT: &str = "timeout";
/// A [`SentryIssueInvestigationFailed`] stage: pushing the branch.
pub const INVESTIGATION_FAILED_PUSH: &str = "push";
/// A [`SentryIssueInvestigationFailed`] stage: opening the pull request.
pub const INVESTIGATION_FAILED_PR: &str = "pr";
/// A [`SentryIssueInvestigationFailed`] stage: heinzel shut down mid-way.
pub const INVESTIGATION_FAILED_SHUTDOWN: &str = "shutdown";

/// Records that an investigation failed at `stage`, with a human-readable
/// `reason`. It is final: there is no retry.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SentryIssueInvestigationFailed {
    /// Sentry's issue id.
    pub issue_id: String,
    /// Where it failed, one of the `INVESTIGATION_FAILED_*` stages.
    pub stage: String,
    /// What went wrong.
    pub reason: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};

    fn to_json(payload: impl Serialize) -> Value {
        serde_json::to_value(payload).unwrap()
    }

    #[test]
    fn a_full_alert_has_go_s_keys() {
        let alert = SentryIssueAlertReceived {
            event_id: "e1".into(),
            issue_id: "1".into(),
            project: 2,
            title: "t".into(),
            level: "error".into(),
            web_url: "u".into(),
            triggered_rule: "r".into(),
            culprit: "c".into(),
            exception_type: "T".into(),
            exception_value: "v".into(),
            frames: vec![SentryStackFrame {
                file: "f.go".into(),
                function: "fn".into(),
                line: 3,
            }],
        };

        assert_eq!(
            to_json(alert),
            json!({
                "eventId": "e1", "issueId": "1", "project": 2, "title": "t",
                "level": "error", "webUrl": "u", "triggeredRule": "r",
                "culprit": "c", "exceptionType": "T", "exceptionValue": "v",
                "frames": [{"file": "f.go", "function": "fn", "line": 3}],
            })
        );
    }

    #[test]
    fn an_alert_without_details_leaves_them_out() {
        let alert = SentryIssueAlertReceived {
            event_id: "e1".into(),
            ..Default::default()
        };

        assert_eq!(
            to_json(alert),
            json!({
                "eventId": "e1", "issueId": "", "project": 0, "title": "",
                "level": "", "webUrl": "", "triggeredRule": "",
            })
        );
    }

    #[test]
    fn a_stored_alert_without_details_still_decodes() {
        let old = r#"{"eventId":"e1","issueId":"1","project":1,"title":"t","level":"error","webUrl":"u","triggeredRule":"r"}"#;

        let got: SentryIssueAlertReceived = serde_json::from_str(old).unwrap();

        assert_eq!(got.issue_id, "1");
        assert!(got.culprit.is_empty() && got.frames.is_empty());
    }

    #[test]
    fn investigation_events_have_go_s_keys() {
        assert_eq!(
            to_json(SentryIssueInvestigationStarted {
                issue_id: "1".into(),
                project: 2,
                repo: "acme/web".into(),
                branch: "b".into(),
            }),
            json!({"issueId": "1", "project": 2, "repo": "acme/web", "branch": "b"})
        );
        assert_eq!(
            to_json(SentryIssueInvestigationPullRequestOpened {
                issue_id: "1".into(),
                pr_url: "p".into(),
            }),
            json!({"issueId": "1", "prUrl": "p"})
        );
        assert_eq!(
            to_json(SentryIssueInvestigationNoChanges {
                issue_id: "1".into()
            }),
            json!({"issueId": "1"})
        );
        assert_eq!(
            to_json(SentryIssueInvestigationSkipped {
                issue_id: "1".into(),
                reason: INVESTIGATION_SKIPPED_STALE.into(),
            }),
            json!({"issueId": "1", "reason": "stale"})
        );
        assert_eq!(
            to_json(SentryIssueInvestigationFailed {
                issue_id: "1".into(),
                stage: INVESTIGATION_FAILED_PR.into(),
                reason: "r".into(),
            }),
            json!({"issueId": "1", "stage": "pr", "reason": "r"})
        );
    }
}
