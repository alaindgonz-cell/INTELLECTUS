#![allow(dead_code)]

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use intellectus_core::coordinator::Coordinator;
use intellectus_core::deduce::Deductor;
use intellectus_core::error::CoreError;
use intellectus_core::policy::{public_key_hex, sign_operator, Policy};
use serde_json::{json, Value};

pub const SEED: [u8; 32] = [7u8; 32];
pub const T0: u64 = 1_800_000_000;

pub const A0: &str = "def parse_page_size(raw):\n    return int(raw)\n";
pub const A1: &str = r#"import re

def parse_page_size(raw: str) -> int:
    if not isinstance(raw, str):
        raise ValueError("page size must be a string")

    if re.fullmatch(r"[0-9]{1,3}", raw) is None:
        raise ValueError("invalid page size syntax")

    value = int(raw)
    if not 1 <= value <= 100:
        raise ValueError("page size out of range")

    return value
"#;

pub fn policy() -> Value {
    json!({
        "version": "P1",
        "tools": {
            "promote_local": {"effect": "internal", "requires_approval": true, "required_checks": ["acceptance_tests", "formal_context"], "idempotent": true},
            "export_view": {"effect": "external", "requires_approval": true, "required_checks": [], "idempotent": false}
        },
        "trusted_issuers": {"acceptance_tests": [{"principal": "runner:fake", "implementation_digest": "fake-worker/v1"}]},
        "protected_paths": ["tests/acceptance/"],
        "max_repairs": 2,
        "role_clearance": {"planner": "internal", "coder": "internal", "tester": "internal", "runner": "internal", "gateway": "internal", "advisor": "public"},
        "jev": {"mode": "SHADOW", "min_confidence_bp": 8000}
    })
}

pub struct H {
    pub c: Option<Coordinator>,
    pub db: PathBuf,
    pub dir: tempfile::TempDir,
    pub clock: Arc<AtomicU64>,
    pub deductor: Deductor,
    nonce: u64,
    pub coder: String,
    pub planner: String,
    pub tester: String,
    pub runner: String,
    pub gateway: String,
    pub advisor: String,
    pub task: String,
    pub req: String,
    pub ev: String,
    pub c_lz: String,
    pub c_rng: String,
    pub c_001: String,
    pub rule: String,
    pub deriv: String,
}

fn clock_fn(c: &Arc<AtomicU64>) -> Box<dyn Fn() -> u64 + Send> {
    let c = c.clone();
    Box::new(move || c.load(Ordering::SeqCst))
}

impl H {
    pub fn new() -> H {
        Self::with(Deductor::Rust, policy())
    }

    pub fn with(deductor: Deductor, policy_json: Value) -> H {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("intellectus.db");
        let clock = Arc::new(AtomicU64::new(T0));
        let policy: Policy = serde_json::from_value(policy_json).unwrap();
        let mut files = BTreeMap::new();
        files.insert(
            "src/page_size.py".to_string(),
            b"def parse_page_size(raw):\n    raise NotImplementedError\n".to_vec(),
        );
        files.insert("src/util.py".to_string(), b"# helpers\n".to_vec());
        files.insert(
            "tests/acceptance/test_page_size.py".to_string(),
            b"# protected acceptance tests T1\n".to_vec(),
        );
        let c = Coordinator::init(
            &db,
            "proj-page",
            vec![public_key_hex(&SEED)],
            policy,
            files,
            deductor.clone(),
            clock_fn(&clock),
        )
        .unwrap();
        let mut h = H {
            c: Some(c),
            db,
            dir,
            clock,
            deductor,
            nonce: 0,
            coder: String::new(),
            planner: String::new(),
            tester: String::new(),
            runner: String::new(),
            gateway: String::new(),
            advisor: String::new(),
            task: "task:page-size".into(),
            req: String::new(),
            ev: String::new(),
            c_lz: String::new(),
            c_rng: String::new(),
            c_001: String::new(),
            rule: String::new(),
            deriv: String::new(),
        };
        h.setup();
        h
    }

    pub fn c(&mut self) -> &mut Coordinator {
        self.c.as_mut().unwrap()
    }

    /// Simulate a crash: drop the coordinator and reopen from the database.
    pub fn crash_and_reopen(&mut self) {
        self.c = None;
        self.c = Some(
            Coordinator::open(&self.db, self.deductor.clone(), clock_fn(&self.clock)).unwrap(),
        );
    }

    pub fn advance(&self, secs: u64) {
        self.clock.fetch_add(secs, Ordering::SeqCst);
    }

    pub fn try_cmd(&mut self, name: &str, args: Value) -> Result<Value, CoreError> {
        self.c().handle(name, args)
    }

    pub fn cmd(&mut self, name: &str, args: Value) -> Value {
        self.try_cmd(name, args.clone())
            .unwrap_or_else(|e| panic!("{name} {args}: {e}"))
    }

    pub fn op(&mut self, mut body: Value) -> Value {
        self.nonce += 1;
        body["project_id"] = json!("proj-page");
        body["nonce"] = json!(format!("n-{}", self.nonce));
        body["issued_at"] = json!(self.clock.load(Ordering::SeqCst));
        let env = sign_operator(&SEED, &body.to_string());
        self.cmd("operator", json!({"envelope": env}))
    }

    pub fn op_ok(&mut self, body: Value) -> Value {
        let r = self.op(body.clone());
        assert_eq!(r["status"], "APPLIED", "{body}: {r}");
        r["result"].clone()
    }

    pub fn session(&mut self, role: &str, label: &str) -> String {
        self.cmd("open_session", json!({"role": role, "label": label}))["session_id"]
            .as_str()
            .unwrap()
            .to_string()
    }

    fn setup(&mut self) {
        self.coder = self.session("coder", "m1");
        self.planner = self.session("planner", "m1");
        self.tester = self.session("tester", "m1");
        self.runner = self.session("runner", "fake");
        self.gateway = self.session("gateway", "g1");
        self.advisor = self.session("advisor", "jev");
        self.req = self.op_ok(json!({"op": "add_requirement", "requirement_id": "R1",
            "text": "Accept only strings of one to three ASCII digits whose value is 1..100 inclusive; leading zeros allowed; else ValueError."}))["ref"]
            .as_str().unwrap().into();
        self.op_ok(
            json!({"op": "register_test_manifest", "manifest_id": "T1", "cases": [
                {"name": "ok_1", "input": "1", "expect": 1},
                {"name": "ok_001", "input": "001", "expect": 1},
                {"name": "plus", "input": "+1", "expect": "ValueError"},
                {"name": "space", "input": " 5", "expect": "ValueError"}
            ]}),
        );
        self.op_ok(json!({"op": "register_environment", "environment_id": "ENV1", "description": "fake worker", "worker_kind": "fake"}));
        self.ev = self.op_ok(
            json!({"op": "add_evidence", "evidence_id": "E1", "content": "R1 as captured",
            "source_locator": "ticket#1", "collection_method": "operator-capture/v1"}),
        )["ref"]
            .as_str()
            .unwrap()
            .into();
        self.c_lz = self.op_ok(json!({"op": "add_claim", "claim_id": "c_lz", "proposition": "leading zeros are allowed",
            "context": "ctx:page-size", "literal": {"atom": "leading_zeros_ok", "positive": true}, "basis": "observed", "support_refs": [self.ev]}))["ref"]
            .as_str().unwrap().into();
        self.c_rng = self.op_ok(json!({"op": "add_claim", "claim_id": "c_rng", "proposition": "range is 1..100",
            "context": "ctx:page-size", "literal": {"atom": "range_1_100", "positive": true}, "basis": "observed", "support_refs": [self.ev]}))["ref"]
            .as_str().unwrap().into();
        self.rule = self.op_ok(json!({"op": "add_rule", "rule_id": "r1",
            "body": [{"atom": "leading_zeros_ok", "positive": true}, {"atom": "range_1_100", "positive": true}],
            "head": {"atom": "accepts_001", "positive": true}}))["ref"].as_str().unwrap().into();
        self.c_001 = self.op_ok(json!({"op": "add_claim", "claim_id": "c_001", "proposition": "\"001\" is accepted",
            "context": "ctx:page-size", "literal": {"atom": "accepts_001", "positive": true}, "basis": "derived"}))["ref"].as_str().unwrap().into();
        self.deriv = self.op_ok(
            json!({"op": "add_derivation", "derivation_id": "d1", "conclusion": self.c_001,
            "premises": [self.c_lz, self.c_rng], "rule_id": self.rule, "context": "ctx:page-size"}),
        )["ref"]
            .as_str()
            .unwrap()
            .into();
        self.op_ok(
            json!({"op": "open_task", "task_id": self.task, "requirement_refs": [self.req],
            "test_manifest": "T1", "environment": "ENV1", "contexts": ["ctx:page-size"]}),
        );
    }

    pub fn root(&mut self) -> String {
        self.c().state().approved_root.clone()
    }

    pub fn submit(&mut self, session: &str, envelope: Value) -> Value {
        let raw = format!("Here is my proposal.\n```json\n{envelope}\n```\n");
        let s = session.to_string();
        self.cmd("submit", json!({"session_id": s, "raw": raw}))
    }

    pub fn candidate_on(&mut self, base: &str, path: &str, content: &str) -> (String, String) {
        let task = self.task.clone();
        let coder = self.coder.clone();
        let r = self.submit(&coder, json!({"kind": "candidate", "task_id": task, "base_root": base, "files": {path: content}}));
        assert_eq!(r["status"], "RECORDED", "{r}");
        (
            r["proposal_id"].as_str().unwrap().into(),
            r["candidate_root"].as_str().unwrap().into(),
        )
    }

    pub fn candidate(&mut self, content: &str) -> (String, String) {
        let base = self.root();
        self.candidate_on(&base, "src/page_size.py", content)
    }

    /// Run the protected runner's checks, reporting every case with `status`.
    pub fn run_checks(&mut self, proposal: &str, result: &str, failing: &[&str]) -> Value {
        let plan = self.cmd("check_plan", json!({"proposal_id": proposal}));
        let check = plan["checks"][0].clone();
        let cases: Vec<Value> = check["test_manifest"]["cases"]
            .as_array()
            .unwrap()
            .iter()
            .map(|c| {
                let n = c["name"].as_str().unwrap();
                json!({"name": n, "status": if failing.contains(&n) {"FAIL"} else {"PASS"}})
            })
            .collect();
        let runner = self.runner.clone();
        self.cmd("report_check", json!({"session_id": runner, "check_id": check["check_id"], "result": result,
            "implementation_digest": "fake-worker/v1", "cases": cases, "collected": cases.len(), "completed": true, "summary": "fake run"}))
    }

    pub fn evaluate(&mut self) -> Value {
        self.cmd("evaluate_context", json!({"context_id": "ctx:page-size"}))
    }

    pub fn action(&mut self, tool: &str, args: Value, key: &str, post: Vec<&str>) -> Value {
        let task = self.task.clone();
        let coder = self.coder.clone();
        let req = self.req.clone();
        let r = self.submit(
            &coder,
            json!({"kind": "action", "task_id": task, "tool_id": tool, "arguments": args,
            "premise_refs": [req], "idempotency_key": key, "expected_postconditions": post}),
        );
        assert_eq!(r["status"], "RECORDED", "{r}");
        r
    }

    pub fn promote_proposal(&mut self, root: &str, key: &str) -> String {
        self.action(
            "promote_local",
            json!({"candidate_root": root}),
            key,
            vec![],
        )["proposal_id"]
            .as_str()
            .unwrap()
            .into()
    }

    pub fn admit(&mut self, proposal: &str) -> Value {
        self.cmd("admit", json!({"proposal_id": proposal}))
    }

    pub fn approve(&mut self, digest: &str, tool: &str) -> String {
        let exp = self.clock.load(Ordering::SeqCst) + 3600;
        self.op_ok(
            json!({"op": "approve", "action_digest": digest, "tool_id": tool, "expires_at": exp}),
        )["approval_id"]
            .as_str()
            .unwrap()
            .into()
    }

    /// Candidate -> passing checks -> formal evaluation -> approved + authorized promotion.
    pub fn authorized_promotion(&mut self, content: &str, key: &str) -> (String, String) {
        let (p, root) = self.candidate(content);
        assert_eq!(self.run_checks(&p, "PASS", &[])["result"], "PASS");
        assert_eq!(self.evaluate()["result"], "PASS");
        let prop = self.promote_proposal(&root, key);
        let r = self.admit(&prop);
        assert_eq!(r["status"], "REJECTED");
        assert_eq!(r["reasons"][0]["code"], "MISSING_APPROVAL", "{r}");
        let digest = r["action_digest"].as_str().unwrap().to_string();
        self.approve(&digest, "promote_local");
        let r = self.admit(&prop);
        assert_eq!(r["status"], "AUTHORIZED", "{r}");
        (r["action_id"].as_str().unwrap().into(), root)
    }

    pub fn codes(v: &Value) -> Vec<String> {
        v["reasons"]
            .as_array()
            .map(|a| {
                a.iter()
                    .map(|r| r["code"].as_str().unwrap().to_string())
                    .collect()
            })
            .unwrap_or_default()
    }
}
