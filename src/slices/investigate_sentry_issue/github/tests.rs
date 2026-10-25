//! The client against a local fake GitHub API.

use std::sync::{Arc, Mutex};

use axum::Router;
use axum::extract::{Request, State};
use axum::http::StatusCode;
use axum::response::IntoResponse;
use serde_json::{Value, json};
use tokio::net::TcpListener;

use super::*;

/// What the fake API received.
#[derive(Debug, Clone, Default)]
struct Received {
    method: String,
    path: String,
    headers: Vec<(String, String)>,
    body: Vec<u8>,
}

impl Received {
    fn header(&self, name: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|(k, _)| k.eq_ignore_ascii_case(name))
            .map(|(_, v)| v.as_str())
    }
}

/// A fake GitHub API that answers every request with `status` and `body`
/// and keeps the last request. Returns its base URL.
async fn fake_api(status: u16, body: String) -> (String, Arc<Mutex<Received>>) {
    let received = Arc::new(Mutex::new(Received::default()));
    let app = Router::new()
        .fallback(
            |State((received, status, body)): State<(Arc<Mutex<Received>>, u16, String)>,
             request: Request| async move {
                let (parts, request_body) = request.into_parts();
                let bytes = axum::body::to_bytes(request_body, usize::MAX).await.unwrap();
                *received.lock().unwrap() = Received {
                    method: parts.method.to_string(),
                    path: parts.uri.path().to_owned(),
                    headers: parts
                        .headers
                        .iter()
                        .map(|(k, v)| (k.to_string(), v.to_str().unwrap().to_owned()))
                        .collect(),
                    body: bytes.to_vec(),
                };
                (StatusCode::from_u16(status).unwrap(), body).into_response()
            },
        )
        .with_state((Arc::clone(&received), status, body));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (format!("http://{addr}/"), received)
}

fn client(api_url: &str) -> Client {
    Client::new(api_url, "gh-token", Duration::from_secs(5)).unwrap()
}

fn pr() -> PullRequest {
    PullRequest {
        title: "Fix".into(),
        head: "heinzel/sentry-1".into(),
        base: "main".into(),
        body: "body".into(),
    }
}

#[tokio::test]
async fn a_draft_is_posted_and_its_url_returned() {
    let (api, received) = fake_api(201, r#"{"number":7,"html_url":"https://github.com/acme/web/pull/7"}"#.into()).await;

    let url = client(&api).open_draft_pr("acme", "web", &pr()).await;

    assert_eq!(url, Ok("https://github.com/acme/web/pull/7".into()));
    let got = received.lock().unwrap().clone();
    assert_eq!((got.method.as_str(), got.path.as_str()), ("POST", "/repos/acme/web/pulls"));
    for (header, want) in [
        ("Authorization", "Bearer gh-token"),
        ("Accept", "application/vnd.github+json"),
        ("X-GitHub-Api-Version", "2022-11-28"),
        ("Content-Type", "application/json"),
    ] {
        assert_eq!(got.header(header), Some(want), "{header}");
    }
    assert_eq!(
        String::from_utf8(got.body).unwrap(),
        r#"{"title":"Fix","head":"heinzel/sentry-1","base":"main","body":"body","draft":true}"#
    );
}

#[tokio::test]
async fn any_other_status_fails_with_the_start_of_the_body() {
    let body = format!("  {{\"message\":\"Validation Failed\"}}{}", "x".repeat(1000));
    let (api, _) = fake_api(422, body).await;

    let err = client(&api).open_draft_pr("acme", "web", &pr()).await.unwrap_err().to_string();

    let prefix = "create pull request: status 422: {\"message\":\"Validation Failed\"}";
    assert!(err.starts_with(prefix), "{err}");
    assert_eq!(err.len(), "create pull request: status 422: ".len() + ERROR_BODY_MAX);
}

#[tokio::test]
async fn a_created_answer_without_a_url_fails() {
    for body in [json!({"number": 7}), json!({"html_url": ""}), Value::Null] {
        let (api, _) = fake_api(201, body.to_string()).await;

        let err = client(&api).open_draft_pr("acme", "web", &pr()).await.unwrap_err();

        assert_eq!(err.to_string(), "create pull request: no html_url in response", "{body}");
    }
}

#[test]
fn debug_output_hides_the_token() {
    assert!(!format!("{:?}", client("https://api.github.test")).contains("gh-token"));
}

#[tokio::test]
async fn an_unreachable_api_fails() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    drop(listener);

    let got = client(&format!("http://{addr}")).open_draft_pr("acme", "web", &pr()).await;

    assert!(got.is_err());
}
