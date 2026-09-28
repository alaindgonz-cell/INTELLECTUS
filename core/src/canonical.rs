//! Canonical JSON encoding and domain-separated SHA-256 digests (PROTOCOL.md §1).

use serde::Serialize;
use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::error::{CoreError, Result};

const DOMAIN: &str = "intellectus/v1/";

/// Canonical JSON: sorted keys (serde_json's default `Map` is a `BTreeMap`),
/// no insignificant whitespace, and no floating point numbers.
pub fn canonical_json(value: &Value) -> Result<String> {
    reject_floats(value)?;
    serde_json::to_string(value).map_err(|e| CoreError::internal(format!("encode: {e}")))
}

pub fn to_canonical<T: Serialize>(value: &T) -> Result<String> {
    let v = serde_json::to_value(value).map_err(|e| CoreError::internal(format!("encode: {e}")))?;
    canonical_json(&v)
}

fn reject_floats(value: &Value) -> Result<()> {
    match value {
        Value::Number(n) if !(n.is_i64() || n.is_u64()) => Err(CoreError::new(
            "NON_CANONICAL",
            "floating point numbers are not permitted in digest-covered data",
        )),
        Value::Array(items) => items.iter().try_for_each(reject_floats),
        Value::Object(map) => map.values().try_for_each(reject_floats),
        _ => Ok(()),
    }
}

fn sha256_hex(parts: &[&[u8]]) -> String {
    let mut h = Sha256::new();
    for p in parts {
        h.update(p);
    }
    format!("sha256:{}", hex::encode(h.finalize()))
}

/// `digest(kind, value)` over the canonical encoding.
pub fn digest<T: Serialize>(kind: &str, value: &T) -> Result<String> {
    let body = to_canonical(value)?;
    Ok(sha256_hex(&[
        DOMAIN.as_bytes(),
        kind.as_bytes(),
        b"\n",
        body.as_bytes(),
    ]))
}

/// Digest of an already-canonical encoding (avoids re-encoding large states).
pub fn digest_canonical(kind: &str, canonical: &str) -> String {
    sha256_hex(&[
        DOMAIN.as_bytes(),
        kind.as_bytes(),
        b"\n",
        canonical.as_bytes(),
    ])
}

pub fn blob_digest(bytes: &[u8]) -> String {
    sha256_hex(&[DOMAIN.as_bytes(), b"blob\n", bytes])
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn keys_are_sorted_and_compact() {
        let v = json!({"b": 1, "a": {"d": [1, 2], "c": "x"}});
        assert_eq!(
            canonical_json(&v).unwrap(),
            r#"{"a":{"c":"x","d":[1,2]},"b":1}"#
        );
    }

    #[test]
    fn floats_are_rejected() {
        assert!(canonical_json(&json!({"p": 0.5})).is_err());
    }

    #[test]
    fn digests_are_domain_separated() {
        let v = json!({"a": 1});
        assert_ne!(digest("tree", &v).unwrap(), digest("action", &v).unwrap());
        assert!(blob_digest(b"x").starts_with("sha256:"));
    }
}
