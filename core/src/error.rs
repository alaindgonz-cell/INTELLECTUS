//! Transport-level errors and structured decision reasons.

use serde::{Deserialize, Serialize};
use std::fmt;

/// A refusal before any decision was made (malformed command, unknown
/// session, integrity failure). Decisions themselves are `Reason`s.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CoreError {
    pub code: String,
    pub message: String,
}

impl CoreError {
    pub fn new(code: &str, message: impl Into<String>) -> Self {
        CoreError {
            code: code.to_string(),
            message: message.into(),
        }
    }
    pub fn internal(message: impl Into<String>) -> Self {
        Self::new("INTERNAL", message)
    }
    pub fn bad_args(message: impl Into<String>) -> Self {
        Self::new("BAD_ARGS", message)
    }
}

impl fmt::Display for CoreError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for CoreError {}

impl From<rusqlite::Error> for CoreError {
    fn from(e: rusqlite::Error) -> Self {
        CoreError::new("STORAGE", e.to_string())
    }
}

pub type Result<T> = std::result::Result<T, CoreError>;

/// Structured failure reason (contract §2.4 "Failure behavior").
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Reason {
    pub code: String,
    pub failed_check: Option<String>,
    pub subject_reference: Option<String>,
    pub missing_dependency: Option<String>,
    pub counterexample_reference: Option<String>,
    pub permitted_next_steps: Vec<String>,
    pub remaining_budget: Option<Budget>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Budget {
    pub repairs: u64,
}

impl Reason {
    pub fn new(code: &str) -> Self {
        Reason {
            code: code.to_string(),
            failed_check: None,
            subject_reference: None,
            missing_dependency: None,
            counterexample_reference: None,
            permitted_next_steps: vec![],
            remaining_budget: None,
        }
    }
    pub fn check(mut self, c: &str) -> Self {
        self.failed_check = Some(c.to_string());
        self
    }
    pub fn subject(mut self, s: &str) -> Self {
        self.subject_reference = Some(s.to_string());
        self
    }
    pub fn missing(mut self, m: &str) -> Self {
        self.missing_dependency = Some(m.to_string());
        self
    }
    pub fn counterexample(mut self, c: &str) -> Self {
        self.counterexample_reference = Some(c.to_string());
        self
    }
    pub fn next(mut self, steps: &[&str]) -> Self {
        self.permitted_next_steps = steps.iter().map(|s| s.to_string()).collect();
        self
    }
    pub fn budget(mut self, repairs: u64) -> Self {
        self.remaining_budget = Some(Budget { repairs });
        self
    }
}
