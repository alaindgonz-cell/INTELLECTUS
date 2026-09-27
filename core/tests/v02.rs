//! v0.2 additions: entrypoints, provider provenance, the scheduler role,
//! read-only UI commands and the database schema check.

mod common;

use common::{A1, H};
use intellectus_core::coordinator::Coordinator;
use intellectus_core::deduce::Deductor;
use serde_json::json;

fn codes(v: &serde_json::Value) -> Vec<String> {
    H::codes(v)
}

#[test]
fn manifest_entrypoint_is_digest_covered_and_reaches_the_runner() {
    let mut h = H::new();
    let ep =
        json!({"language": "python", "path": "src/page_size.py", "function": "parse_page_size"});
    let d1 = h.op_ok(json!({"op": "register_test_manifest", "manifest_id": "T2",
        "cases": [{"name": "ok_1", "input": "1", "expect": {"returns": 1}}], "entrypoint": ep}))
        ["digest"]
        .clone();
    let mut other = ep.clone();
    other["function"] = json!("parse_other");
    let d2 = h.op_ok(json!({"op": "register_test_manifest", "manifest_id": "T3",
        "cases": [{"name": "ok_1", "input": "1", "expect": {"returns": 1}}], "entrypoint": other}))
        ["digest"]
        .clone();
    assert_ne!(d1, d2);
    for bad in [
        json!({"language": "ruby", "path": "a.rb", "function": "f"}),
        json!({"language": "python", "path": "../x.py", "function": "f"}),
        json!({"language": "python", "path": "x.py", "function": "f(); import os"}),
    ] {
        let r = h.op(json!({"op": "register_test_manifest", "manifest_id": "T4",
            "cases": [{"name": "a", "input": 1, "expect": {"returns": 1}}], "entrypoint": bad}));
        assert_eq!(r["status"], "REJECTED", "{r}");
    }
    h.op_ok(json!({"op": "open_task", "task_id": "task:ep", "title": "entrypoint task",
        "requirement_refs": [h.req], "test_manifest": "T2", "environment": "ENV1", "contexts": ["ctx:page-size"]}));
    h.task = "task:ep".into();
    let (p, _) = h.candidate(A1);
    let plan = h.cmd("check_plan", json!({"proposal_id": p}));
    assert_eq!(plan["checks"][0]["test_manifest"]["entrypoint"], ep);
    let tasks = h.cmd("list_tasks", json!({}));
    let t = tasks["tasks"]
        .as_array()
        .unwrap()
        .iter()
        .find(|t| t["task_id"] == "task:ep")
        .unwrap()
        .clone();
    assert_eq!(t["title"], "entrypoint task");
    assert_eq!(t["entrypoint"], ep);
    assert!(t["requirement"].as_str().unwrap().contains("ASCII digits"));
}

#[test]
fn provider_provenance_is_recorded_without_trusting_it() {
    let mut h = H::new();
    let intake = h.session("intake", "claude");
    let r = h.cmd("record_input", json!({"session_id": intake, "raw": "Sure - I propose a task.",
        "provider_record": {"provider": "anthropic", "model_requested": "claude-opus-5", "model_returned": "claude-opus-5",
            "response_id": "msg_1", "raw_response": "{\"id\":\"msg_1\"}", "usage": {"input_tokens": 10, "output_tokens": 5},
            "latency_ms": 900, "attempts": 1}}));
    let id = r["input_id"].as_str().unwrap().to_string();
    let rec = h.c().state().inputs[&id].clone();
    assert_eq!(rec.kind, "record");
    let p = rec.provider.unwrap();
    assert_eq!(p.model_returned, "claude-opus-5");
    let blob = h
        .c()
        .store()
        .blob(p.response_blob.as_ref().unwrap())
        .unwrap()
        .unwrap();
    assert_eq!(blob, b"{\"id\":\"msg_1\"}");
    // An intake session cannot submit proposals, and a coder cannot use record_input as a side door.
    assert!(h
        .try_cmd("submit", json!({"session_id": intake, "raw": "{}"}))
        .is_err());
    // Floats in usage are rejected (digest-covered data is integer-only).
    let coder = h.coder.clone();
    assert!(h
        .try_cmd("record_input", json!({"session_id": coder, "raw": "x",
            "provider_record": {"provider": "a", "model_requested": "m", "model_returned": "m", "usage": {"cost": 0.5}}}))
        .is_err());
}

#[test]
fn scheduler_role_may_only_propose_actions() {
    let mut h = H::new();
    let sched = h.session("scheduler", "engine");
    let task = h.task.clone();
    let base = h.root();
    let r = h.submit(&sched, json!({"kind": "candidate", "task_id": task, "base_root": base, "files": {"src/page_size.py": A1}}));
    assert_eq!(codes(&r), vec!["ROLE_NOT_PERMITTED"]);
    let r = h.submit(
        &sched,
        json!({"kind": "action", "task_id": task, "tool_id": "promote_local",
        "arguments": {"candidate_root": base}, "idempotency_key": "k"}),
    );
    assert_eq!(r["status"], "RECORDED", "{r}");
}

#[test]
fn task_without_manifest_fails_closed() {
    let mut h = H::new();
    h.op_ok(json!({"op": "open_task", "task_id": "task:export", "title": "export", "requirement_refs": [h.req]}));
    h.task = "task:export".into();
    let (p, root) = h.candidate(A1);
    assert_eq!(
        h.try_cmd("check_plan", json!({"proposal_id": p}))
            .unwrap_err()
            .code,
        "UNKNOWN_TEST_MANIFEST"
    );
    let prop = h.promote_proposal(&root, "k");
    let r = h.admit(&prop);
    assert!(
        codes(&r).contains(&"MISSING_TEST_MANIFEST".to_string()),
        "{r}"
    );
}

#[test]
fn read_only_ui_commands() {
    let mut h = H::new();
    let st = h.cmd("status", json!({}));
    assert_eq!(st["schema_version"], 2);
    assert_eq!(st["policy_version"], "P1");
    let tree = h.cmd("read_tree", json!({}));
    assert!(tree["files"]["src/page_size.py"]
        .as_str()
        .unwrap()
        .contains("NotImplementedError"));
    let ev = h.cmd("events_since", json!({"after": 0, "limit": 3}));
    let evs = ev["events"].as_array().unwrap();
    assert_eq!(evs.len(), 3);
    assert_eq!(evs[0]["sequence"], 1);
    assert!(evs[0]["summary"]
        .as_str()
        .unwrap()
        .starts_with("project created"));
    let later = h.cmd("events_since", json!({"after": 2, "limit": 1}));
    assert_eq!(later["events"][0]["sequence"], 3);
}

#[test]
fn database_from_another_schema_is_refused() {
    let h = H::new();
    drop(h.c);
    let conn = rusqlite::Connection::open(&h.db).unwrap();
    conn.execute(
        "UPDATE meta SET value = '1' WHERE key = 'schema_version'",
        [],
    )
    .unwrap();
    drop(conn);
    let err = Coordinator::open(&h.db, Deductor::Rust, Box::new(|| 0))
        .err()
        .unwrap();
    assert_eq!(err.code, "UNSUPPORTED_SCHEMA");
}
