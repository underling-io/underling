//! heinzel is an event-sourced app organised as vertical slices.
//!
//! The event store keeps every event; event handlers react to them; the app
//! wires the slices' HTTP routes and event handlers together and owns their
//! lifecycle. Slices live under `src/slices/`, one module directory each.
#![forbid(unsafe_code)]
#![warn(missing_docs)]

pub mod app;
pub mod eventhandler;
pub mod eventstore;
pub mod logging;
pub mod slices;

#[cfg(test)]
mod testlog;
