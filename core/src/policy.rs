//! Protected policy and operator authentication.

use std::collections::BTreeMap;

use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use serde::{Deserialize, Serialize};

use crate::error::{CoreError, Result};

pub const OPERATOR_SIGNING_PREFIX: &[u8] = b"intellectus/v1/operator\n";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Policy {
    pub version: String,
    pub tools: BTreeMap<String, ToolPolicy>,
    pub trusted_issuers: BTreeMap<String, Vec<TrustedIssuer>>,
    #[serde(default)]
    pub protected_paths: Vec<String>,
    pub max_repairs: u64,
    pub role_clearance: BTreeMap<String, Sensitivity>,
    pub jev: JevPolicy,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolPolicy {
    pub effect: Effect,
    pub requires_approval: bool,
    #[serde(default)]
    pub required_checks: Vec<String>,
    pub idempotent: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Effect {
    /// Executed atomically by the coordinator itself (e.g. local promotion).
    Internal,
    /// Executed by the tool gateway against an external target.
    External,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TrustedIssuer {
    pub principal: String,
    pub implementation_digest: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct JevPolicy {
    /// LIVE, SHADOW or OFF.
    pub mode: String,
    pub min_confidence_bp: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Sensitivity {
    Public,
    Internal,
    Restricted,
}

/// Checks the core itself performs (not delegated to a runner).
pub const CORE_CHECKS: &[&str] = &["formal_context"];
/// Checks a registered runner performs.
pub const RUNNER_CHECKS: &[&str] = &["acceptance_tests"];

impl Policy {
    pub fn validate(&self) -> Result<()> {
        if self.version.is_empty() {
            return Err(CoreError::bad_args("policy version required"));
        }
        for (name, tool) in &self.tools {
            for c in &tool.required_checks {
                if !CORE_CHECKS.contains(&c.as_str()) && !RUNNER_CHECKS.contains(&c.as_str()) {
                    return Err(CoreError::bad_args(format!(
                        "tool {name}: unsupported check {c}"
                    )));
                }
            }
        }
        if !["LIVE", "SHADOW", "OFF"].contains(&self.jev.mode.as_str()) {
            return Err(CoreError::bad_args("jev.mode must be LIVE, SHADOW or OFF"));
        }
        if self.jev.min_confidence_bp > 10_000 {
            return Err(CoreError::bad_args(
                "jev.min_confidence_bp must be <= 10000",
            ));
        }
        Ok(())
    }

    pub fn clearance(&self, role: &str) -> Sensitivity {
        // Unknown roles get the lowest clearance.
        self.role_clearance
            .get(role)
            .copied()
            .unwrap_or(Sensitivity::Public)
    }

    pub fn is_trusted_issuer(&self, check: &str, principal: &str, implementation: &str) -> bool {
        self.trusted_issuers
            .get(check)
            .map(|v| {
                v.iter()
                    .any(|i| i.principal == principal && i.implementation_digest == implementation)
            })
            .unwrap_or(false)
    }

    pub fn is_protected(&self, path: &str) -> bool {
        self.protected_paths
            .iter()
            .any(|p| path.starts_with(p.as_str()))
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OperatorEnvelope {
    pub body: String,
    pub key_id: String,
    pub signature: String,
}

/// Verify an operator envelope against the registered keys; returns the body.
pub fn verify_operator(
    env: &OperatorEnvelope,
    registered: &std::collections::BTreeSet<String>,
) -> Result<()> {
    if !registered.contains(&env.key_id) {
        return Err(CoreError::new(
            "UNKNOWN_OPERATOR_KEY",
            "key is not a registered operator key",
        ));
    }
    let key_bytes: [u8; 32] = hex::decode(&env.key_id)
        .ok()
        .and_then(|b| b.try_into().ok())
        .ok_or_else(|| CoreError::new("BAD_SIGNATURE", "malformed key id"))?;
    let key = VerifyingKey::from_bytes(&key_bytes)
        .map_err(|_| CoreError::new("BAD_SIGNATURE", "invalid key"))?;
    let sig_bytes: [u8; 64] = hex::decode(&env.signature)
        .ok()
        .and_then(|b| b.try_into().ok())
        .ok_or_else(|| CoreError::new("BAD_SIGNATURE", "malformed signature"))?;
    let sig = Signature::from_bytes(&sig_bytes);
    let mut msg = OPERATOR_SIGNING_PREFIX.to_vec();
    msg.extend_from_slice(env.body.as_bytes());
    key.verify(&msg, &sig)
        .map_err(|_| CoreError::new("BAD_SIGNATURE", "signature does not verify"))
}

/// Sign an operator body (used by the `sign` CLI and tests).
pub fn sign_operator(seed: &[u8; 32], body: &str) -> OperatorEnvelope {
    use ed25519_dalek::{Signer, SigningKey};
    let key = SigningKey::from_bytes(seed);
    let mut msg = OPERATOR_SIGNING_PREFIX.to_vec();
    msg.extend_from_slice(body.as_bytes());
    OperatorEnvelope {
        body: body.to_string(),
        key_id: hex::encode(key.verifying_key().to_bytes()),
        signature: hex::encode(key.sign(&msg).to_bytes()),
    }
}

pub fn public_key_hex(seed: &[u8; 32]) -> String {
    hex::encode(
        ed25519_dalek::SigningKey::from_bytes(seed)
            .verifying_key()
            .to_bytes(),
    )
}
