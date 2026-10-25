//! Opens draft pull requests through the GitHub REST API.

#[cfg(test)]
mod tests;

use std::time::Duration;

use reqwest::StatusCode;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE};
use serde::{Deserialize, Serialize};

/// How much of an error response a failure keeps.
const ERROR_BODY_MAX: usize = 512;
/// How much of a response is read at all.
const RESPONSE_MAX: usize = 1 << 20;

/// Opening a pull request failed; the text says why.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{0}")]
pub(crate) struct Error(String);

/// What a new pull request holds. It is always opened as a draft.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub(crate) struct PullRequest {
    pub(crate) title: String,
    pub(crate) head: String,
    pub(crate) base: String,
    pub(crate) body: String,
}

/// The request body: the pull request, as a draft.
#[derive(Serialize)]
struct NewPullRequest<'a> {
    #[serde(flatten)]
    pr: &'a PullRequest,
    draft: bool,
}

/// Opens pull requests through the GitHub REST API.
#[derive(Clone)]
pub(crate) struct Client {
    /// The REST API base, e.g. `https://api.github.com`.
    api_url: String,
    token: String,
    http: reqwest::Client,
}

impl std::fmt::Debug for Client {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Client")
            .field("api_url", &self.api_url)
            .field("token", &"[redacted]")
            .finish_non_exhaustive()
    }
}

impl Client {
    /// Returns a client for the API at `api_url`, authenticating with
    /// `token`, whose requests time out after `timeout`.
    pub(crate) fn new(api_url: &str, token: &str, timeout: Duration) -> Result<Client, Error> {
        let http = reqwest::Client::builder()
            .timeout(timeout)
            .build()
            .map_err(|err| Error(format!("build http client: {err}")))?;
        Ok(Client {
            api_url: api_url.trim_end_matches('/').to_owned(),
            token: token.to_owned(),
            http,
        })
    }

    /// Opens `pr` as a draft in `owner/name` and returns its URL. Any status
    /// but 201 is an error with the status and the start of the response
    /// body.
    pub(crate) async fn open_draft_pr(
        &self,
        owner: &str,
        name: &str,
        pr: &PullRequest,
    ) -> Result<String, Error> {
        let url = format!("{}/repos/{owner}/{name}/pulls", self.api_url);
        let body = serde_json::to_vec(&NewPullRequest { pr, draft: true })
            .map_err(|err| Error(err.to_string()))?;
        let mut response = self
            .http
            .post(url)
            .header(AUTHORIZATION, format!("Bearer {}", self.token))
            .header(ACCEPT, "application/vnd.github+json")
            .header("X-GitHub-Api-Version", "2022-11-28")
            .header(CONTENT_TYPE, "application/json")
            .body(body)
            .send()
            .await
            .map_err(|err| Error(err.to_string()))?;
        let status = response.status();
        let mut body = Vec::new();
        while body.len() < RESPONSE_MAX {
            match response.chunk().await {
                Ok(Some(chunk)) => body.extend_from_slice(&chunk),
                Ok(None) => break,
                Err(err) => return Err(Error(format!("read response: {err}"))),
            }
        }
        body.truncate(RESPONSE_MAX);
        if status != StatusCode::CREATED {
            let text = String::from_utf8_lossy(&body);
            let excerpt = text.trim().as_bytes();
            let excerpt = String::from_utf8_lossy(&excerpt[..excerpt.len().min(ERROR_BODY_MAX)]);
            return Err(Error(format!(
                "create pull request: status {}: {excerpt}",
                status.as_u16()
            )));
        }
        #[derive(Deserialize)]
        struct Created {
            #[serde(default)]
            html_url: Option<String>,
        }
        serde_json::from_slice::<Created>(&body)
            .ok()
            .and_then(|created| created.html_url)
            .filter(|url| !url.is_empty())
            .ok_or_else(|| Error("create pull request: no html_url in response".to_owned()))
    }
}
