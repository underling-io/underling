//! An automation slice:
//!
//! ```text
//! SentryIssueAlertReceived → sentry-issue-investigator →
//!   SentryIssueInvestigationStarted, then PullRequestOpened, NoChanges or Failed
//!   (or SentryIssueInvestigationSkipped)
//! ```
//!
//! For each new Sentry issue it clones the GitHub repo mapped to the issue's
//! project, runs headless Claude Code in a sandboxed child process to
//! investigate and fix the issue, then pushes the branch and opens a draft PR
//! itself. Claude never sees the GitHub token. It loads its config from the
//! environment; see `config.rs`.
//!
//! `flow.rs` holds the whole decision path, from alert to recorded outcome.
//! It reaches git, Claude and GitHub only through small traits, which
//! `infrastructure.rs` implements with the sub-modules: `workspace` (the
//! clone and git commands), `claude` (the sandboxed child) and `github` (the
//! draft PR). The sub-modules never use the flow.
//!
//! The slice needs Unix: it runs Claude in its own process group and checks
//! the work dir's owner and mode.

#[cfg(not(unix))]
compile_error!("the investigate_sentry_issue slice needs Unix: process groups and work-dir checks");

mod ci_config;
mod claude;
mod config;
mod duration;
mod flow;
mod github;
mod infrastructure;
mod names;
mod outcome;
mod paths;
mod pr;
mod process;
mod prompt;
mod text;
mod workspace;

#[cfg(test)]
mod e2e_tests;
#[cfg(test)]
mod flow_tests;
#[cfg(test)]
mod test_logs;

use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use chrono::{DateTime, TimeDelta, Utc};
use tokio_util::sync::CancellationToken;
use tracing::warn;

use crate::app::{Automation, ConfigError, Deps, Factory};
use crate::eventstore::Store;

use config::{Config, WORK_DIR_VAR};
use flow::Investigator;
use infrastructure::{GitHubPrOpener, WorkspaceCloner};

/// Registers the slice.
pub const FACTORY: Factory = Factory::Automation(from_env);

/// The checkpoint key; never change it.
const HANDLER_NAME: &str = "sentry-issue-investigator";

const GITHUB_API_URL: &str = "https://api.github.com";
const GITHUB_CLONE_URL: &str = "https://github.com/";
const GITHUB_REQUEST_TIMEOUT: Duration = Duration::from_secs(30);

/// What [`new`] needs.
struct Params<'a> {
    /// Looks up the slice's config.
    getenv: &'a dyn Fn(&str) -> Option<String>,
    /// The GitHub REST API base, e.g. `https://api.github.com`.
    github_api_url: &'a str,
    /// The clone URL base: a repo is cloned from
    /// `<github_clone_url><owner>/<repo>.git`, e.g. `https://github.com/`.
    github_clone_url: &'a str,
    /// How long a GitHub API request may take.
    github_timeout: Duration,
    /// The clock, read once when the handler is built: alerts stored more
    /// than the max age before that are stale.
    now: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
}

fn from_env(deps: &Deps<'_>) -> Result<Automation, ConfigError> {
    new(Params {
        getenv: deps.getenv,
        github_api_url: GITHUB_API_URL,
        github_clone_url: GITHUB_CLONE_URL,
        github_timeout: GITHUB_REQUEST_TIMEOUT,
        now: Arc::new(Utc::now),
    })
}

/// Returns the slice. It loads its config through `p.getenv` and fails if
/// any value is invalid.
fn new(p: Params<'_>) -> Result<Automation, ConfigError> {
    Ok(Automation::new(handler_builder(p)?))
}

/// Loads the config as [`new`] does and returns what builds the handler
/// from the automation's token and the store.
fn handler_builder(
    p: Params<'_>,
) -> Result<impl Fn(CancellationToken, Arc<Store>) -> Investigator + Send + 'static, ConfigError> {
    let mut cfg = Config::load(p.getenv)?;
    if cfg.work_dir.is_empty() {
        // A dir of its own, not the temp dir itself: the child is denied
        // reads of the whole work dir, and the sandbox needs the temp dir.
        cfg.work_dir = workspace::default_dir();
    }
    // Each run checks the work dir again, since it may change or appear
    // later; checking an existing one now fails startup instead of every
    // alert.
    let missing = Path::new(&cfg.work_dir)
        .symlink_metadata()
        .is_err_and(|err| err.kind() == std::io::ErrorKind::NotFound);
    if !missing {
        workspace::check_dir(&cfg.work_dir).map_err(|err| format!("{WORK_DIR_VAR}: {err}"))?;
    }
    cfg.resolve_read_paths()?;
    for (dir, denied) in cfg.denied_path_dirs() {
        warn!(
            handler = HANDLER_NAME, dir, denied,
            "PATH entry lies under a path the child may not read; tools there won't run inside the sandbox"
        );
    }
    let client = github::Client::new(p.github_api_url, &cfg.github_token, p.github_timeout)?;
    let cloner = WorkspaceCloner::new(&cfg, p.github_clone_url);
    let max_age = TimeDelta::from_std(cfg.max_age).unwrap_or(TimeDelta::MAX);
    let now = p.now;
    Ok(move |cancel, store| Investigator {
        cancel,
        store,
        stale_before: now()
            .checked_sub_signed(max_age)
            .unwrap_or(DateTime::<Utc>::MIN_UTC),
        repos: cfg.repos.clone(),
        cloner: Box::new(cloner.clone()),
        prs: Box::new(GitHubPrOpener {
            client: client.clone(),
        }),
    })
}
