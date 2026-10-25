//! The vertical slices, one module directory each: `src/slices/<slice>/mod.rs`.
//!
//! `build.rs` wires every slice directory in, so adding one is enough. Each
//! slice exposes its factory as `pub const FACTORY: app::Factory`; a slice
//! without one fails to compile. A slice may use only std, external crates,
//! `crate::events`, `crate::eventstore`, `crate::eventhandler`, `crate::app`
//! and its own sub-modules, and slices are coupled only through events. A
//! test enforces the import rule.

include!(concat!(env!("OUT_DIR"), "/slices.rs"));
