//! The flow's traits, implemented with the sub-modules: `workspace` (the
//! clone and git commands), `claude` (the sandboxed child) and `github`
//! (the draft PR).

use std::time::Duration;

use async_trait::async_trait;
use tokio_util::sync::CancellationToken;
use tracing::error;

use super::HANDLER_NAME;
use super::claude::{self, RunError};
use super::config::{Config, Repo};
use super::flow::{ClaudeFailure, Cloner, PrOpener, PullRequest, Run};
use super::github;
use super::workspace::{Workspace, remove_dir};

/// Clones each investigation's repo into a workspace of its own, where
/// Claude runs in a sandboxed child process.
#[derive(Clone)]
pub(super) struct WorkspaceCloner {
    /// The work dir, already defaulted.
    pub(super) work_dir: String,
    /// The clone URL base: a repo is cloned from
    /// `<clone_base><owner>/<repo>.git`.
    pub(super) clone_base: String,
    pub(super) path: String,
    pub(super) github_token: String,
    pub(super) claude: claude::Config,
    pub(super) timeout: Duration,
}

impl WorkspaceCloner {
    pub(super) fn new(cfg: &Config, clone_base: &str) -> WorkspaceCloner {
        WorkspaceCloner {
            work_dir: cfg.work_dir.clone(),
            clone_base: clone_base.to_owned(),
            path: cfg.path.clone(),
            github_token: cfg.github_token.clone(),
            claude: claude::Config {
                bin: cfg.claude_bin.clone(),
                plugin_dirs: cfg.plugin_dirs.clone(),
                path: cfg.path.clone(),
                anthropic_key: cfg.anthropic_key.clone(),
                openai_key: cfg.openai_key.clone(),
                parent_home: cfg.parent_home.clone(),
                deny_read: cfg.deny_read.clone(),
                readable: cfg.readable.clone(),
            },
            timeout: cfg.timeout,
        }
    }
}

#[async_trait]
impl Cloner for WorkspaceCloner {
    async fn clone_repo(
        &self,
        issue_id: &str,
        target: &Repo,
        branch: &str,
        cancel: &CancellationToken,
    ) -> Result<Box<dyn Run>, String> {
        let url = format!("{}{}/{}.git", self.clone_base, target.owner, target.name);
        let ws = Workspace::new(&self.work_dir, &url, branch, &self.path, &self.github_token, cancel)
            .await
            .map_err(|err| err.to_string())?;
        Ok(Box::new(WorkspaceRun {
            ws,
            claude: self.claude.clone(),
            timeout: self.timeout,
            issue_id: issue_id.to_owned(),
            removed: false,
        }))
    }
}

/// An investigation in a workspace. Dropped without [`Run::remove`], say
/// when the flow panics, it still removes the workspace, in the background.
struct WorkspaceRun {
    ws: Workspace,
    claude: claude::Config,
    timeout: Duration,
    issue_id: String,
    removed: bool,
}

impl Drop for WorkspaceRun {
    fn drop(&mut self) {
        if self.removed {
            return;
        }
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            let (dir, issue_id) = (self.ws.run_dir.clone(), self.issue_id.clone());
            runtime.spawn(async move {
                if let Err(err) = remove_dir(dir.clone()).await {
                    log_remove_failure(&issue_id, &dir, &err);
                }
            });
        }
    }
}

fn log_remove_failure(issue_id: &str, dir: &str, err: &std::io::Error) {
    error!(
        handler = HANDLER_NAME, issueId = %issue_id, dir = %dir, error = %err,
        "remove investigation workspace"
    );
}

#[async_trait]
impl Run for WorkspaceRun {
    async fn claude(
        &mut self,
        prompt: &str,
        cancel: &CancellationToken,
    ) -> Result<String, ClaudeFailure> {
        let cmd = claude::command(&self.claude, &self.ws, prompt);
        // The mirror stays read-only while claude runs. If the lock fails,
        // claude doesn't run.
        self.ws.lock_mirror().map_err(|err| ClaudeFailure {
            reason: err.to_string(),
            timed_out: false,
        })?;
        let result = claude::run(&cmd, self.timeout, cancel).await;
        self.ws.unlock_mirror();
        result.map_err(claude_failure)
    }

    fn check_mirror(&self) -> Result<(), String> {
        self.ws.check_mirror().map_err(|err| err.to_string())
    }

    async fn stray_branches(&mut self, cancel: &CancellationToken) -> Result<Vec<String>, String> {
        self.ws.stray_branches(cancel).await.map_err(|err| err.to_string())
    }

    async fn new_commits(&mut self, cancel: &CancellationToken) -> Result<u64, String> {
        self.ws.new_commits(cancel).await.map_err(|err| err.to_string())
    }

    async fn changed_paths(&mut self, cancel: &CancellationToken) -> Result<Vec<String>, String> {
        self.ws.changed_paths(cancel).await.map_err(|err| err.to_string())
    }

    async fn push(&mut self, cancel: &CancellationToken) -> Result<(), String> {
        self.ws.push(cancel).await.map_err(|err| err.to_string())
    }

    fn default_branch(&self) -> String {
        self.ws.default_branch.clone()
    }

    async fn remove(&mut self) {
        self.removed = true;
        if let Err(err) = self.ws.remove().await {
            log_remove_failure(&self.issue_id, &self.ws.run_dir, &err);
        }
    }
}

/// Tells the flow why claude failed, and whether it ran out of time.
fn claude_failure(err: RunError) -> ClaudeFailure {
    ClaudeFailure {
        timed_out: matches!(err, RunError::Timeout(_)),
        reason: err.to_string(),
    }
}

/// Opens draft pull requests through the GitHub REST API.
pub(super) struct GitHubPrOpener {
    pub(super) client: github::Client,
}

#[async_trait]
impl PrOpener for GitHubPrOpener {
    async fn open_draft(
        &self,
        target: &Repo,
        pr: &PullRequest,
        cancel: &CancellationToken,
    ) -> Result<String, String> {
        let pr = github::PullRequest {
            title: pr.title.clone(),
            head: pr.head.clone(),
            base: pr.base.clone(),
            body: pr.body.clone(),
        };
        tokio::select! {
            opened = self.client.open_draft_pr(&target.owner, &target.name, &pr) => {
                opened.map_err(|err| err.to_string())
            }
            () = cancel.cancelled() => Err("create pull request: heinzel shut down".to_owned()),
        }
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::process::Command;

    use super::super::test_logs::capture_logs;
    use super::*;

    fn git(dir: &Path, args: &[&str]) {
        let status = Command::new("git")
            .args(["-c", "user.name=test", "-c", "user.email=test@example.com"])
            .args(args)
            .current_dir(dir)
            .status()
            .unwrap();
        assert!(status.success(), "git {args:?}");
    }

    /// A run in a fresh workspace on a local bare remote, under `root`.
    async fn test_run(root: &Path) -> WorkspaceRun {
        let remote = root.join("web.git");
        let seed = root.join("seed");
        std::fs::create_dir(&seed).unwrap();
        git(&seed, &["init", "-q", "--bare", "-b", "main", remote.to_str().unwrap()]);
        git(&seed, &["init", "-q", "-b", "main"]);
        git(&seed, &["commit", "-q", "--allow-empty", "-m", "seed"]);
        git(&seed, &["push", "-q", remote.to_str().unwrap(), "main"]);
        let ws = Workspace::new(
            root.join("work").to_str().unwrap(),
            remote.to_str().unwrap(),
            "heinzel/sentry-1",
            &std::env::var("PATH").unwrap(),
            "token",
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        WorkspaceRun {
            ws,
            claude: claude::Config::default(),
            timeout: Duration::from_secs(60),
            issue_id: "1".into(),
            removed: false,
        }
    }

    #[tokio::test]
    async fn a_failed_removal_is_logged() {
        use std::os::unix::fs::PermissionsExt;
        let root = tempfile::tempdir().unwrap();
        let mut run = test_run(root.path()).await;
        let work_dir = root.path().join("work");
        let locked = std::fs::Permissions::from_mode(0o500);
        std::fs::set_permissions(&work_dir, locked).unwrap();
        let logs = capture_logs();

        run.remove().await;

        std::fs::set_permissions(&work_dir, std::fs::Permissions::from_mode(0o700)).unwrap();
        let lines = logs.lines();
        let line = lines
            .iter()
            .find(|l| l["message"] == "remove investigation workspace")
            .unwrap_or_else(|| panic!("no removal error logged: {lines:?}"));
        assert_eq!(line["level"], "ERROR");
        assert_eq!(line["handler"], "sentry-issue-investigator");
        assert_eq!(line["issueId"], "1");
        assert_eq!(line["dir"], run.ws.run_dir.as_str());
        assert!(line["error"].as_str().is_some_and(|e| !e.is_empty()), "{line}");
    }

    #[tokio::test]
    async fn a_run_dropped_without_removing_it_still_removes_its_workspace() {
        let root = tempfile::tempdir().unwrap();
        let run = test_run(root.path()).await;
        let run_dir = run.ws.run_dir.clone();

        drop(run);

        tokio::time::timeout(Duration::from_secs(10), async {
            while Path::new(&run_dir).exists() {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("the run dir outlived the run");
    }

    #[test]
    fn a_real_claude_timeout_counts_as_timed_out() {
        let got = claude_failure(RunError::Timeout(Duration::from_secs(30 * 60)));

        assert_eq!(
            got,
            ClaudeFailure {
                reason: "claude did not finish within 30m0s".into(),
                timed_out: true,
            }
        );
    }

    #[test]
    fn other_claude_failures_do_not_count_as_timed_out() {
        for err in [RunError::Failed("claude: exit status 1".into()), RunError::Shutdown] {
            assert!(!claude_failure(err).timed_out);
        }
    }
}
