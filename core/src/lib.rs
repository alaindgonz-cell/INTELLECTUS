//! THE INTELLECTUS — The Substance.
//!
//! The immutable core: the only writer of authoritative project state. It
//! records untrusted proposals, tracks justification in the Axiomatic
//! Knowledge Graph, issues and checks verification receipts, authorizes exact
//! actions, and owns the outbox launch boundary. It never calls a model and
//! never executes candidate code.

#![allow(clippy::result_large_err)] // structured rejection reasons are cold-path values

pub mod akg;
pub mod canonical;
pub mod coordinator;
pub mod deduce;
pub mod error;
pub mod policy;
pub mod server;
pub mod state;
pub mod store;
