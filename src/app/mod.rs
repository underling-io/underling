//! The app wires the event store, the state changes' HTTP routes and the
//! automations' event handlers together and owns their lifecycle.
//!
//! A slice is either a state change (an HTTP route that appends events) or an
//! automation (an event handler that reacts to them). Each slice exposes a
//! [`Factory`]; [`build`] calls them once at startup and [`run`] runs the
//! result until shutdown.

mod config;
mod serve;
mod slices;

#[cfg(test)]
mod tests;

use std::sync::Arc;
use std::time::Duration;

use axum::Router;
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;
use tokio_util::task::AbortOnDropHandle;
use tracing::instrument::WithSubscriber;

pub use config::Config;
pub use slices::{Automation, ConfigError, Deps, Factory, Slices, StateChange, build};

use crate::eventhandler::{self, Runner};
use crate::eventstore::{self, Store};

/// Why [`run`] failed.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The event store could not be opened.
    #[error("{0}")]
    OpenStore(eventstore::Error),
    /// Serving HTTP failed.
    #[error("serve http: {0}")]
    Serve(std::io::Error),
    /// In-flight requests outlived the shutdown timeout and were cut off.
    #[error("shut down http server: requests still running after {0:?}")]
    ShutdownTimeout(Duration),
    /// Event handlers had stopped with errors, one per failed handler.
    #[error("stop event handlers: {}", join(.0))]
    StopHandlers(Vec<eventhandler::Error>),
}

fn join(errors: &[eventhandler::Error]) -> String {
    let messages: Vec<String> = errors.iter().map(ToString::to_string).collect();
    messages.join("; ")
}

/// Serves the state changes' routes on `listener` and runs the automations
/// until `shutdown` is cancelled or an event handler panics, then shuts down
/// in order:
///
///  1. stop accepting and drain HTTP requests (cut off after the shutdown
///     timeout);
///  2. cancel the automations' token;
///  3. stop the event handlers;
///  4. close the store.
///
/// Every step runs even if an earlier one failed; the first error is
/// returned. A client gets 10s to send a request's headers, as in Go.
/// Dropping the future instead stops the server and signals the automations
/// and handlers to stop, without waiting for them.
pub async fn run(
    config: &Config,
    listener: TcpListener,
    slices: Slices,
    shutdown: CancellationToken,
) -> Result<(), Error> {
    let addr = listener.local_addr().map_err(Error::Serve)?;
    let store = Arc::new(Store::open(&config.data_dir).map_err(Error::OpenStore)?);

    let mut router = Router::new();
    for state_change in slices.state_changes {
        let (path, route) = state_change.into_route(Arc::clone(&store));
        router = router.route(path, route);
    }
    let automations = CancellationToken::new();
    let handlers = slices
        .automations
        .into_iter()
        .map(|a| a.into_handler(automations.clone(), Arc::clone(&store)))
        .collect();
    // If run's future is dropped instead of shut down, the automations still
    // see the cancellation, the runner stops its tasks and the server stops.
    let _cancel_automations = automations.clone().drop_guard();
    let runner = Runner::start(Arc::clone(&store), handlers);
    let stop_serving = CancellationToken::new();
    let serve = serve::serve(
        listener,
        router,
        stop_serving.clone(),
        serve::HEADER_READ_TIMEOUT,
        config.shutdown_timeout,
    );
    let mut serve = AbortOnDropHandle::new(tokio::spawn(serve.with_current_subscriber()));
    tracing::info!(addr = %addr, dataDir = %config.data_dir.display(), "app started");

    // Shut down when asked, when a handler panicked (its error comes from
    // stopping the handlers), or when serving ended early, which only a
    // panic in the server can cause.
    let served_early = tokio::select! {
        () = shutdown.cancelled() => None,
        () = runner.panicked() => None,
        served = &mut serve => Some(served),
    };
    tracing::info!("app shutting down");
    stop_serving.cancel();
    let served = match served_early {
        Some(served) => served,
        None => serve.await,
    };
    let mut errors = Vec::new();
    match served {
        Ok(Ok(())) => {}
        Ok(Err(serve::DrainTimedOut)) => {
            errors.push(Error::ShutdownTimeout(config.shutdown_timeout));
        }
        Err(join) => errors.push(Error::Serve(std::io::Error::other(join))),
    }
    automations.cancel();
    if let Err(stopped) = runner.stop().await {
        errors.push(Error::StopHandlers(stopped));
    }
    store.close();
    errors.into_iter().next().map_or(Ok(()), Err)
}
