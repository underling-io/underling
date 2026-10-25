//! Serves HTTP the way the Go server did: a deadline for request headers, a
//! graceful drain on shutdown, and a hard close when the drain takes too long.

use std::time::Duration;

use axum::Router;
use hyper::server::conn::http1;
use hyper_util::rt::{TokioIo, TokioTimer};
use hyper_util::server::graceful::GracefulShutdown;
use hyper_util::service::TowerToHyperService;
use tokio::net::TcpListener;
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;
use tracing::instrument::WithSubscriber;

/// How long a client may take to send a request's headers before its
/// connection is closed, like Go's `ReadHeaderTimeout`. Without it, slow or
/// idle clients could hold connections open forever.
pub(super) const HEADER_READ_TIMEOUT: Duration = Duration::from_secs(10);

/// How long to wait before accepting again after `accept` failed.
const ACCEPT_RETRY: Duration = Duration::from_millis(100);

/// Connections were still open when the drain ran out of time, so they were
/// closed.
#[derive(Debug)]
pub(super) struct DrainTimedOut;

/// Serves `router` on `listener` until `shutdown` is cancelled. Then it stops
/// accepting, gives open connections up to `drain_timeout` to finish their
/// requests and closes those still open after that, like Go's `Shutdown`
/// followed by `Close`.
///
/// It speaks HTTP/1 only, as the Go server did without TLS.
pub(super) async fn serve(
    listener: TcpListener,
    router: Router,
    shutdown: CancellationToken,
    header_read_timeout: Duration,
    drain_timeout: Duration,
) -> Result<(), DrainTimedOut> {
    let mut http = http1::Builder::new();
    http.timer(TokioTimer::new())
        .header_read_timeout(header_read_timeout);
    let graceful = GracefulShutdown::new();
    let mut connections = JoinSet::new();
    loop {
        tokio::select! {
            () = shutdown.cancelled() => break,
            // Reap finished connections so the set doesn't grow.
            Some(_) = connections.join_next(), if !connections.is_empty() => {}
            accepted = listener.accept() => match accepted {
                Ok((stream, _)) => {
                    // As Go does. Failing only costs latency, so it's ignored.
                    let _ = stream.set_nodelay(true);
                    let service = TowerToHyperService::new(router.clone());
                    let connection = http.serve_connection(TokioIo::new(stream), service);
                    connections.spawn(graceful.watch(connection).with_current_subscriber());
                }
                Err(err) => {
                    // A client that gave up, or out of file descriptors:
                    // retry shortly, as Go does.
                    tracing::warn!(error = %err, "accept connection");
                    tokio::time::sleep(ACCEPT_RETRY).await;
                }
            },
        }
    }
    drop(listener);
    if tokio::time::timeout(drain_timeout, graceful.shutdown())
        .await
        .is_ok()
    {
        return Ok(());
    }
    // Aborting a connection's task closes its socket and drops the request
    // handler at its next await. Like Go's `Close`, this doesn't wait: a
    // handler stuck in sync code would otherwise hold up the shutdown.
    connections.abort_all();
    Err(DrainTimedOut)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testlog::WAIT_LIMIT;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpStream;
    use tokio::time::timeout;

    #[tokio::test]
    async fn a_client_sending_headers_too_slowly_is_disconnected() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let shutdown = CancellationToken::new();
        let header_read_timeout = Duration::from_millis(100);
        let server = tokio::spawn(serve(
            listener,
            Router::new(),
            shutdown.clone(),
            header_read_timeout,
            WAIT_LIMIT,
        ));
        let mut client = TcpStream::connect(addr).await.unwrap();

        client
            .write_all(b"GET / HTTP/1.1\r\nHost: localhost\r\n")
            .await
            .unwrap();

        let mut answer = Vec::new();
        let closed = timeout(WAIT_LIMIT, client.read_to_end(&mut answer)).await;
        assert!(
            closed.is_ok(),
            "still connected {WAIT_LIMIT:?} after a header read timeout of {header_read_timeout:?}"
        );
        shutdown.cancel();
        timeout(WAIT_LIMIT, server).await.unwrap().unwrap().unwrap();
    }
}
