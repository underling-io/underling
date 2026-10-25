//! The two slice kinds and how the app builds them at startup.

use std::sync::Arc;

use axum::handler::Handler as HttpHandler;
use axum::routing::{MethodFilter, MethodRouter};
use tokio_util::sync::CancellationToken;

use crate::eventhandler::Handler;
use crate::eventstore::Store;

/// A state-change slice: one HTTP route that may append to the store.
pub struct StateChange {
    method: MethodFilter,
    path: &'static str,
    build: Box<dyn FnOnce(Arc<Store>) -> MethodRouter + Send>,
}

impl StateChange {
    /// Returns a state change serving `method` on `path`. The app calls
    /// `build` once at startup, after the store is open, to get the route's
    /// axum handler.
    pub fn new<H, T>(
        method: MethodFilter,
        path: &'static str,
        build: impl FnOnce(Arc<Store>) -> H + Send + 'static,
    ) -> StateChange
    where
        H: HttpHandler<T, ()>,
        T: 'static,
    {
        StateChange {
            method,
            path,
            build: Box::new(move |store| axum::routing::on(method, build(store))),
        }
    }

    /// The HTTP method the route answers.
    pub fn method(&self) -> MethodFilter {
        self.method
    }

    /// The route's path, in axum syntax, e.g. `/append/{stream}`.
    pub fn path(&self) -> &'static str {
        self.path
    }

    pub(super) fn into_route(self, store: Arc<Store>) -> (&'static str, MethodRouter) {
        (self.path, (self.build)(store))
    }
}

impl std::fmt::Debug for StateChange {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("StateChange")
            .field("method", &self.method)
            .field("path", &self.path)
            .finish_non_exhaustive()
    }
}

/// An automation slice: an event handler that reacts to stored events.
pub struct Automation {
    build: Box<dyn FnOnce(CancellationToken, Arc<Store>) -> Box<dyn Handler> + Send>,
}

impl Automation {
    /// Returns an automation. The app calls `build` once at startup, after the
    /// store is open, to get the event handler. The token is cancelled when
    /// shutdown begins, before the event handlers are stopped, so long work
    /// inside `handle` can give up.
    pub fn new<H: Handler + 'static>(
        build: impl FnOnce(CancellationToken, Arc<Store>) -> H + Send + 'static,
    ) -> Automation {
        Automation {
            build: Box::new(move |token, store| Box::new(build(token, store))),
        }
    }

    pub(super) fn into_handler(
        self,
        token: CancellationToken,
        store: Arc<Store>,
    ) -> Box<dyn Handler> {
        (self.build)(token, store)
    }
}

impl std::fmt::Debug for Automation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Automation").finish_non_exhaustive()
    }
}

/// Every slice the app runs, grouped by kind.
#[derive(Debug, Default)]
pub struct Slices {
    /// The state changes, in build order.
    pub state_changes: Vec<StateChange>,
    /// The automations, in build order.
    pub automations: Vec<Automation>,
}

/// What the app hands every slice factory at startup. Slices log through
/// `tracing` directly.
pub struct Deps<'a> {
    /// Looks up an environment variable; `None` if it is unset.
    pub getenv: &'a dyn Fn(&str) -> Option<String>,
}

impl std::fmt::Debug for Deps<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Deps").finish_non_exhaustive()
    }
}

/// A slice factory's error, typically invalid config. A slice keeps its own
/// error type; `?` turns it into this.
pub type ConfigError = Box<dyn std::error::Error + Send + Sync>;

/// A slice's factory. Every slice module exposes one as `pub const FACTORY`.
#[derive(Debug, Clone, Copy)]
pub enum Factory {
    /// Builds a state-change slice.
    StateChange(fn(&Deps<'_>) -> Result<StateChange, ConfigError>),
    /// Builds an automation slice.
    Automation(fn(&Deps<'_>) -> Result<Automation, ConfigError>),
}

/// Calls every factory with `deps`, in order, and returns the slices. It stops
/// at the first factory error and returns it unchanged.
pub fn build(factories: &[Factory], deps: &Deps<'_>) -> Result<Slices, ConfigError> {
    let mut slices = Slices::default();
    for factory in factories {
        match factory {
            Factory::StateChange(new) => slices.state_changes.push(new(deps)?),
            Factory::Automation(new) => slices.automations.push(new(deps)?),
        }
    }
    Ok(slices)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::eventstore::Event;
    use async_trait::async_trait;
    use std::cell::RefCell;

    fn no_env(_: &str) -> Option<String> {
        None
    }

    struct Noop;

    #[async_trait]
    impl Handler for Noop {
        fn name(&self) -> &str {
            "noop"
        }

        async fn handle(&mut self, _: Event) {}
    }

    thread_local! {
        static BUILT: RefCell<Vec<&'static str>> = const { RefCell::new(Vec::new()) };
    }

    fn route_a(_: &Deps<'_>) -> Result<StateChange, ConfigError> {
        Ok(StateChange::new(MethodFilter::GET, "/a", |_| || async {}))
    }

    fn route_b(_: &Deps<'_>) -> Result<StateChange, ConfigError> {
        Ok(StateChange::new(MethodFilter::GET, "/b", |_| || async {}))
    }

    fn first(_: &Deps<'_>) -> Result<Automation, ConfigError> {
        BUILT.with_borrow_mut(|built| built.push("first"));
        Ok(Automation::new(|_, _| Noop))
    }

    fn second(_: &Deps<'_>) -> Result<Automation, ConfigError> {
        BUILT.with_borrow_mut(|built| built.push("second"));
        Ok(Automation::new(|_, _| Noop))
    }

    #[test]
    fn build_keeps_list_order_within_each_kind() {
        let factories = [
            Factory::Automation(first),
            Factory::StateChange(route_a),
            Factory::Automation(second),
            Factory::StateChange(route_b),
        ];

        let slices = build(&factories, &Deps { getenv: &no_env }).unwrap();

        let paths: Vec<&str> = slices.state_changes.iter().map(StateChange::path).collect();
        assert_eq!(paths, ["/a", "/b"]);
        assert_eq!(slices.automations.len(), 2);
        assert_eq!(BUILT.with_borrow(Clone::clone), ["first", "second"]);
    }

    fn echo_env(deps: &Deps<'_>) -> Result<StateChange, ConfigError> {
        Err(format!("X is {:?}", (deps.getenv)("X")).into())
    }

    #[test]
    fn build_passes_deps_to_factories() {
        let getenv = |key: &str| (key == "X").then(|| "value".to_owned());

        let err = build(&[Factory::StateChange(echo_env)], &Deps { getenv: &getenv }).unwrap_err();

        assert_eq!(err.to_string(), r#"X is Some("value")"#);
    }

    #[derive(Debug, thiserror::Error)]
    #[error("SENTRY_CLIENT_SECRET must be set")]
    struct MissingSecret;

    fn failing(_: &Deps<'_>) -> Result<StateChange, ConfigError> {
        Err(MissingSecret.into())
    }

    fn never(_: &Deps<'_>) -> Result<Automation, ConfigError> {
        panic!("factory called after the failing one");
    }

    #[test]
    fn build_returns_the_first_error_unchanged_and_stops() {
        let factories = [Factory::StateChange(failing), Factory::Automation(never)];

        let err = build(&factories, &Deps { getenv: &no_env }).unwrap_err();

        assert!(err.downcast_ref::<MissingSecret>().is_some(), "got {err:?}");
    }

    #[test]
    fn build_with_no_factories_gives_no_slices() {
        let slices = build(&[], &Deps { getenv: &no_env }).unwrap();

        assert!(slices.state_changes.is_empty());
        assert!(slices.automations.is_empty());
    }
}
