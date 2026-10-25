//! A state-change slice:
//!
//! ```text
//! POST /sentry/callback → "Record Sentry issue alert" → SentryIssueAlertReceived
//! ```
//!
//! It verifies a Sentry internal-integration webhook and records each issue
//! alert. It loads the integration's client secret from
//! `SENTRY_CLIENT_SECRET`.

mod callback;
mod exception;
mod go_json;

#[cfg(test)]
mod tests;

use axum::extract::Request;
use axum::routing::MethodFilter;

use crate::app::{ConfigError, Deps, Factory, StateChange};

use callback::Callback;

/// Registers the slice.
pub const FACTORY: Factory = Factory::StateChange(new);

/// The environment variable holding the client secret.
const SECRET_ENV_VAR: &str = "SENTRY_CLIENT_SECRET";

/// The client secret is missing or empty.
#[derive(Debug, thiserror::Error)]
#[error("SENTRY_CLIENT_SECRET must be set")]
struct MissingSecret;

/// Returns the slice, serving `POST /sentry/callback`. It reads the client
/// secret, used to verify webhook signatures, from `SENTRY_CLIENT_SECRET`
/// and fails if it is missing or empty.
fn new(deps: &Deps<'_>) -> Result<StateChange, ConfigError> {
    let secret = (deps.getenv)(SECRET_ENV_VAR)
        .filter(|secret| !secret.is_empty())
        .ok_or(MissingSecret)?;
    Ok(StateChange::new(
        MethodFilter::POST,
        "/sentry/callback",
        move |store| {
            let callback = Callback::new(secret.into_bytes(), store);
            move |request: Request| callback.clone().record(request)
        },
    ))
}
