//! The coordinator: the single writer of authoritative project state.
//!
//! Every command runs against the current head, decides, and commits its
//! events, snapshots and projections in one SQLite transaction. Decisions
//! (admission, dispatch eligibility, check results) are computed here and
//! recorded as events; the reducer only folds them.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

use serde::de::DeserializeOwned;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::akg::{self, Epistemic};
use crate::canonical::{blob_digest, canonical_json, digest, digest_canonical, to_canonical};
use crate::deduce::Deductor;
use crate::error::{CoreError, Reason, Result};
use crate::policy::{
    verify_operator, Effect, OperatorEnvelope, Policy, Sensitivity, RUNNER_CHECKS,
};
use crate::state::*;
use crate::store::{Batch, Store};

pub const PROMOTE_TOOL: &str = "promote_local";
pub const PROTOCOL_VERSION: u64 = 1;
pub const ROUTE_OPTIONS: [&str; 5] = ["GATHER_CONTEXT", "REPAIR", "REPLAN", "ESCALATE", "STOP"];
const MODE_ROLES: [&str; 3] = ["planner", "coder", "tester"];
const ROLES: [&str; 8] = [
    "planner",
    "coder",
    "tester",
    "runner",
    "gateway",
    "advisor",
    "intake",
    "scheduler",
];
const MAX_SUMMARY: usize = 2000;

pub type Clock = Box<dyn Fn() -> u64 + Send>;

pub fn system_clock() -> Clock {
    Box::new(|| {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0)
    })
}

/// A working transaction: a private copy of the head state plus the events
/// and blobs that will be committed atomically.
pub struct Tx {
    pub state: State,
    digest: String,
    base_sequence: u64,
    now: u64,
    events: Vec<(Event, String, String)>,
    blobs: Vec<(String, Vec<u8>)>,
}

impl Tx {
    /// Sequence number the next emitted event will receive (used for ids).
    pub fn next_seq(&self) -> u64 {
        self.state.sequence + 1
    }

    pub fn emit(&mut self, actor: &str, body: EventBody) -> Result<u64> {
        let seq = self.next_seq();
        let event = Event {
            event_id: format!("evt:{seq}"),
            project_id: self.state.project_id.clone(),
            sequence: seq,
            schema_version: SCHEMA_VERSION,
            reducer_version: REDUCER_VERSION,
            event_type: body.event_type(),
            authenticated_actor: actor.to_string(),
            recorded_at: self.now,
            prior_state_digest: self.digest.clone(),
            body,
        };
        let next = reduce_with_digest(&self.state, &self.digest, &event)?;
        let json = to_canonical(&next)?;
        let d = digest_canonical("state", &json);
        self.state = next;
        self.digest = d.clone();
        self.events.push((event, d, json));
        Ok(seq)
    }

    pub fn put_blob(&mut self, bytes: Vec<u8>) -> String {
        let d = blob_digest(&bytes);
        self.blobs.push((d.clone(), bytes));
        d
    }
}

pub struct Coordinator {
    store: Store,
    state: State,
    digest: String,
    deductor: Deductor,
    clock: Clock,
    /// Named fault-injection points (tests only): the transaction aborts
    /// exactly as a crash before commit would.
    pub faults: BTreeSet<String>,
}

fn args<T: DeserializeOwned>(v: Value) -> Result<T> {
    serde_json::from_value(v).map_err(|e| CoreError::bad_args(e.to_string()))
}

fn valid_ident(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 96
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-:".contains(&b))
}

fn rejected(reasons: Vec<Reason>) -> Value {
    json!({"status": "REJECTED", "reasons": reasons})
}

impl Coordinator {
    // ------------------------------------------------------------------
    // Lifecycle
    // ------------------------------------------------------------------

    pub fn init(
        path: &Path,
        project_id: &str,
        operator_keys: Vec<String>,
        policy: Policy,
        root_files: BTreeMap<String, Vec<u8>>,
        deductor: Deductor,
        clock: Clock,
    ) -> Result<Coordinator> {
        if !valid_ident(project_id) {
            return Err(CoreError::bad_args("invalid project id"));
        }
        policy.validate()?;
        for k in &operator_keys {
            if hex::decode(k).map(|b| b.len()) != Ok(32) {
                return Err(CoreError::bad_args("operator key must be 32-byte hex"));
            }
        }
        let store = Store::create(path, project_id)?;
        let state = State::empty(project_id);
        let empty_digest = state.digest()?;
        let mut c = Coordinator {
            store,
            state,
            digest: empty_digest,
            deductor,
            clock,
            faults: BTreeSet::new(),
        };
        let mut tx = c.begin();
        let mut manifest = BTreeMap::new();
        for (p, bytes) in root_files {
            validate_path(&p).map_err(|r| CoreError::bad_args(format!("{}: {p}", r.code)))?;
            manifest.insert(p, tx.put_blob(bytes));
        }
        let root = digest("tree", &manifest)?;
        let policy_digest = digest("policy", &policy)?;
        tx.emit(
            "operator:genesis",
            EventBody::Genesis {
                operator_keys,
                policy,
                policy_digest,
                approved_root: root,
                manifest,
            },
        )?;
        c.commit(tx)?;
        Ok(c)
    }

    pub fn open(path: &Path, deductor: Deductor, clock: Clock) -> Result<Coordinator> {
        let store = Store::open(path)?;
        let (seq, head_digest) = store
            .head()?
            .ok_or_else(|| CoreError::new("NOT_INITIALIZED", "no head; run init"))?;
        let (state, digest) = store.load_snapshot(seq)?;
        if digest != head_digest {
            return Err(CoreError::new(
                "INTEGRITY",
                "head digest does not match snapshot",
            ));
        }
        let mut c = Coordinator {
            store,
            state,
            digest,
            deductor,
            clock,
            faults: BTreeSet::new(),
        };
        c.recover()?;
        Ok(c)
    }

    /// A crash after DISPATCH_STARTED but before an outcome was recorded is
    /// not evidence of failure: such actions become OUTCOME_UNKNOWN.
    fn recover(&mut self) -> Result<()> {
        let mut tx = self.begin();
        let started: Vec<(String, String)> = tx
            .state
            .actions
            .values()
            .filter(|a| a.state == DispatchState::DispatchStarted)
            .map(|a| {
                (
                    a.action_id.clone(),
                    a.attempts
                        .last()
                        .map(|x| x.attempt_id.clone())
                        .unwrap_or_default(),
                )
            })
            .collect();
        for (action_id, attempt_id) in started {
            tx.emit(
                "core:recovery",
                EventBody::OutcomeUnknown {
                    action_id,
                    attempt_id,
                    reason: "coordinator restarted before an outcome was recorded".into(),
                },
            )?;
        }
        self.commit(tx)
    }

    pub fn state(&self) -> &State {
        &self.state
    }

    pub fn store(&self) -> &Store {
        &self.store
    }

    pub fn now(&self) -> u64 {
        (self.clock)()
    }

    fn begin(&self) -> Tx {
        Tx {
            state: self.state.clone(),
            digest: self.digest.clone(),
            base_sequence: self.state.sequence,
            now: (self.clock)(),
            events: vec![],
            blobs: vec![],
        }
    }

    fn fault(&self, point: &str) -> Result<()> {
        if self.faults.contains(point) {
            return Err(CoreError::new("FAULT_INJECTED", point));
        }
        Ok(())
    }

    fn commit(&mut self, tx: Tx) -> Result<()> {
        if tx.events.is_empty() {
            return Ok(());
        }
        self.fault("before_commit")?;
        let last = tx.state.sequence;
        let actions = tx
            .state
            .actions
            .values()
            .filter(|a| self.state.actions.get(&a.action_id) != Some(a))
            .map(|a| (a.clone(), last))
            .collect();
        let batch = Batch {
            base_sequence: tx.base_sequence,
            events: tx.events,
            blobs: tx.blobs,
            actions,
        };
        self.store.commit(&batch)?;
        self.state = tx.state;
        self.digest = tx.digest;
        Ok(())
    }

    // ------------------------------------------------------------------
    // Command dispatch
    // ------------------------------------------------------------------

    pub fn handle(&mut self, cmd: &str, a: Value) -> Result<Value> {
        let read_only = matches!(
            cmd,
            "hello"
                | "view"
                | "route"
                | "task_report"
                | "replay_verify"
                | "pending_reconciliation"
                | "query"
                | "list_tasks"
                | "read_tree"
                | "events_since"
                | "status"
        );
        let effect_recording = matches!(
            cmd,
            "record_outcome" | "record_outcome_unknown" | "record_reconciliation" | "operator"
        );
        if self.state.shutdown && !read_only && !effect_recording {
            return Err(CoreError::new(
                "SHUTDOWN",
                "operator shutdown: no new work is accepted",
            ));
        }
        match cmd {
            "hello" => self.hello(),
            "open_session" => self.open_session(args(a)?),
            "view" => self.view(args(a)?),
            "query" => self.query(args(a)?),
            "submit" => self.submit(args(a)?),
            "check_plan" => self.check_plan(args(a)?),
            "report_check" => self.report_check(args(a)?),
            "evaluate_context" => self.evaluate_context(args(a)?),
            "admit" => self.admit(args(a)?),
            "operator" => self.operator(args(a)?),
            "dispatch_begin" => self.dispatch_begin(args(a)?),
            "record_outcome" => self.record_outcome(args(a)?),
            "record_outcome_unknown" => self.record_outcome_unknown(args(a)?),
            "pending_reconciliation" => self.pending_reconciliation(),
            "record_reconciliation" => self.record_reconciliation(args(a)?),
            "route" => self.route_cmd(args(a)?),
            "record_assessment" => self.record_assessment(args(a)?),
            "task_transition" => self.task_transition(args(a)?),
            "task_report" => self.task_report(args(a)?),
            "replay_verify" => self.replay_verify(),
            "record_input" => self.record_input(args(a)?),
            "list_tasks" => self.list_tasks(),
            "read_tree" => self.read_tree(args(a)?),
            "events_since" => self.events_since(args(a)?),
            "status" => self.status(),
            other => Err(CoreError::new("UNKNOWN_COMMAND", other.to_string())),
        }
    }

    fn session(&self, id: &str, roles: &[&str]) -> Result<Session> {
        let s = self
            .state
            .sessions
            .get(id)
            .ok_or_else(|| CoreError::new("UNKNOWN_SESSION", id.to_string()))?;
        if !roles.contains(&s.role.as_str()) {
            return Err(CoreError::new(
                "ROLE_NOT_PERMITTED",
                format!("{} may not do this", s.role),
            ));
        }
        Ok(s.clone())
    }

    fn fingerprint(s: &State) -> Result<String> {
        akg::dependency_fingerprint(s)
    }

    fn hello(&self) -> Result<Value> {
        Ok(json!({
            "protocol": PROTOCOL_VERSION,
            "project_id": self.state.project_id,
            "head_sequence": self.state.sequence,
            "reducer_version": REDUCER_VERSION,
            "schema_version": SCHEMA_VERSION,
            "deductor": {"kind": self.deductor.kind(), "implementation_digest": self.deductor.implementation_digest()?},
        }))
    }

    fn open_session(&mut self, a: OpenSession) -> Result<Value> {
        if !ROLES.contains(&a.role.as_str()) {
            return Err(CoreError::bad_args(format!("unknown role {}", a.role)));
        }
        if !valid_ident(&a.label) {
            return Err(CoreError::bad_args("invalid label"));
        }
        let mut tx = self.begin();
        let session_id = format!("sess:{}", tx.next_seq());
        let principal = format!("{}:{}", a.role, a.label);
        let session = Session {
            session_id: session_id.clone(),
            role: a.role,
            principal: principal.clone(),
            opened_sequence: tx.next_seq(),
        };
        tx.emit("engine", EventBody::SessionOpened { session })?;
        self.commit(tx)?;
        Ok(json!({"session_id": session_id, "principal": principal}))
    }

    // ------------------------------------------------------------------
    // Views and retrieval
    // ------------------------------------------------------------------

    fn materialize(&self, root: &str) -> Result<BTreeMap<String, String>> {
        let manifest = self
            .state
            .trees
            .get(root)
            .ok_or_else(|| CoreError::new("UNKNOWN_TREE", root.to_string()))?;
        let mut files = BTreeMap::new();
        for (p, d) in manifest {
            let bytes = self
                .store
                .blob(d)?
                .ok_or_else(|| CoreError::new("UNAVAILABLE", format!("blob {d}")))?;
            files.insert(p.clone(), String::from_utf8_lossy(&bytes).into_owned());
        }
        Ok(files)
    }

    fn view(&self, a: ViewArgs) -> Result<Value> {
        let sess = self.session(&a.session_id, &ROLES)?;
        let s = &self.state;
        let policy = s.policy()?;
        let clearance = policy.clearance(&sess.role);
        let now = self.now();
        let task = s
            .tasks
            .get(&a.task_id)
            .ok_or_else(|| CoreError::new("UNKNOWN_TASK", a.task_id.clone()))?;
        let candidates: Vec<Value> = task
            .candidates
            .iter()
            .filter_map(|pid| s.proposals.get(pid))
            .map(|p| {
                let root = match &p.body {
                    ProposalBody::Candidate { candidate_root, .. } => candidate_root.clone(),
                    _ => String::new(),
                };
                json!({"proposal_id": p.proposal_id, "candidate_root": root, "acceptance": p.acceptance})
            })
            .collect();
        let last_failures: Vec<Value> = s
            .verifications
            .values()
            .filter(|v| v.check_kind == "acceptance_tests" && v.result != CheckResult::Pass)
            .filter(|v| v.subject.proposal_id.as_ref().is_some_and(|p| task.candidates.contains(p)))
            .map(|v| json!({"proposal_id": v.subject.proposal_id, "result": v.result, "failed_cases": v.failed_cases}))
            .collect();
        let mut out = json!({
            "snapshot_sequence": s.sequence,
            "state_digest": self.digest,
            "dependency_fingerprint": Self::fingerprint(s)?,
            "policy_version": policy.version,
            "task": {
                "task_id": task.task_id, "status": task.status,
                "requirement_refs": task.requirement_refs,
                "repairs_used": task.repairs_used, "max_repairs": task.max_repairs,
                "failed_candidates": task.failed_candidates,
                "candidates": candidates, "last_failures": last_failures,
                "contexts": task.contexts,
            },
        });
        if sess.role == "advisor" {
            // Minimum authorized context for routing.
            return Ok(out);
        }
        let requirements: Vec<Value> = task
            .requirement_refs
            .iter()
            .filter_map(|r| s.nodes.get(r))
            .filter(|n| akg::visible(s, n, clearance))
            .map(|n| match &n.body {
                NodeBody::Requirement { text } => {
                    json!({"ref": n.node_ref, "text": text, "lifecycle": n.lifecycle})
                }
                _ => json!({"ref": n.node_ref}),
            })
            .collect();
        let claims: Vec<Value> = s
            .nodes
            .values()
            .filter(|n| akg::visible(s, n, clearance))
            .filter_map(|n| match &n.body {
                NodeBody::Claim { proposition, context, literal, .. } if task.contexts.contains(context) => Some(json!({
                    "ref": n.node_ref, "proposition": proposition, "context": context, "literal": literal,
                    "lifecycle": n.lifecycle, "epistemic": akg::epistemic(s, &n.node_ref, now),
                    "usable": akg::premise_usable(s, &n.node_ref, now).is_ok(),
                })),
                _ => None,
            })
            .collect();
        out["requirements"] = json!(requirements);
        out["claims"] = json!(claims);
        if MODE_ROLES.contains(&sess.role.as_str()) {
            out["approved_root"] =
                json!({"digest": s.approved_root, "files": self.materialize(&s.approved_root)?});
            if let Some(m) = task
                .test_manifest
                .as_ref()
                .and_then(|id| s.test_manifests.get(id))
            {
                out["test_manifest"] = json!({"id": m.manifest_id, "digest": m.digest, "cases": m.cases, "entrypoint": m.entrypoint});
            }
        }
        Ok(out)
    }

    fn query(&self, a: QueryArgs) -> Result<Value> {
        let sess = self.session(&a.session_id, &ROLES)?;
        let clearance = self.state.policy()?.clearance(&sess.role);
        let now = self.now();
        let Some(nodes) = akg::expand(&self.state, &a.node_ref, clearance, a.depth.min(8)) else {
            // Identical to a genuinely missing record: no existence leak.
            return Ok(json!({"status": "NOT_FOUND"}));
        };
        let items: Vec<Value> = nodes
            .iter()
            .map(|n| {
                let mut v = json!({"ref": n.node_ref, "lifecycle": n.lifecycle, "body": n.body});
                if let NodeBody::Claim { .. } = n.body {
                    v["epistemic"] = json!(akg::epistemic(&self.state, &n.node_ref, now));
                }
                if let Some(d) = self.state.derivation_status.get(&n.node_ref) {
                    v["derivation_status"] = json!(d.status);
                }
                v
            })
            .collect();
        Ok(json!({"status": "OK", "snapshot_sequence": self.state.sequence, "nodes": items}))
    }

    // ------------------------------------------------------------------
    // Proposals
    // ------------------------------------------------------------------

    /// Record untrusted input (and its provider provenance) in `tx`.
    fn record_untrusted(
        tx: &mut Tx,
        sess: &Session,
        raw: &str,
        provider: Option<&ProviderArgs>,
        kind: &str,
    ) -> Result<String> {
        let input_id = format!("input:{}", tx.next_seq());
        let blob = tx.put_blob(raw.as_bytes().to_vec());
        let provider = match provider {
            None => None,
            Some(p) => {
                canonical_json(&p.usage)?;
                let response_blob = p
                    .raw_response
                    .as_ref()
                    .map(|r| tx.put_blob(r.clone().into_bytes()));
                Some(ProviderRecord {
                    provider: p.provider.clone(),
                    model_requested: p.model_requested.clone(),
                    model_returned: p.model_returned.clone(),
                    response_id: p.response_id.clone(),
                    response_blob,
                    usage: p.usage.clone(),
                    latency_ms: p.latency_ms,
                    attempts: p.attempts,
                })
            }
        };
        tx.emit(
            &sess.principal,
            EventBody::InputRecorded {
                input: InputRecord {
                    input_id: input_id.clone(),
                    session_id: sess.session_id.clone(),
                    principal: sess.principal.clone(),
                    blob,
                    byte_len: raw.len() as u64,
                    rejected: None,
                    provider,
                    kind: kind.to_string(),
                },
            },
        )?;
        Ok(input_id)
    }

    /// Record model output that is not a proposal (e.g. a chat reply).
    fn record_input(&mut self, a: RecordInputArgs) -> Result<Value> {
        let sess = self.session(&a.session_id, &["intake", "planner", "coder", "tester"])?;
        let mut tx = self.begin();
        let input_id =
            Self::record_untrusted(&mut tx, &sess, &a.raw, a.provider_record.as_ref(), "record")?;
        self.commit(tx)?;
        Ok(json!({"status": "RECORDED", "input_id": input_id}))
    }

    fn submit(&mut self, a: SubmitArgs) -> Result<Value> {
        let sess = self.session(&a.session_id, &["planner", "coder", "tester", "scheduler"])?;
        // Record the untrusted input before interpreting it.
        let record = |c: &Coordinator| -> Result<(Tx, String)> {
            let mut tx = c.begin();
            let input_id = Self::record_untrusted(
                &mut tx,
                &sess,
                &a.raw,
                a.provider_record.as_ref(),
                "proposal",
            )?;
            Ok((tx, input_id))
        };
        let (mut tx, input_id) = record(self)?;
        match self.interpret(&mut tx, &sess, &input_id, &a.raw) {
            Ok(result) => {
                self.commit(tx)?;
                Ok(result)
            }
            Err(reasons) => {
                // Discard any partial interpretation; keep only the input and its rejection.
                let (mut tx, input_id) = record(self)?;
                tx.emit(
                    &sess.principal,
                    EventBody::InputRejected {
                        input_id: input_id.clone(),
                        reasons: reasons.clone(),
                    },
                )?;
                self.commit(tx)?;
                Ok(json!({"status": "REJECTED", "input_id": input_id, "reasons": reasons}))
            }
        }
    }

    fn interpret(
        &self,
        tx: &mut Tx,
        sess: &Session,
        input_id: &str,
        raw: &str,
    ) -> std::result::Result<Value, Vec<Reason>> {
        let one = |code: &str| vec![Reason::new(code).subject(input_id)];
        let text = extract_envelope(raw).map_err(&one)?;
        let env: Envelope = serde_json::from_str(text).map_err(|_| one("SCHEMA_INVALID"))?;
        let task_id = env.task_id().to_string();
        let task = tx
            .state
            .tasks
            .get(&task_id)
            .cloned()
            .ok_or_else(|| one("UNKNOWN_TASK"))?;
        if task.status != TaskStatus::Open {
            return Err(one("TASK_NOT_OPEN"));
        }
        let policy = tx.state.policy.clone().expect("initialized");
        let allowed: &[&str] = match &env {
            Envelope::Plan(_) => &["planner"],
            Envelope::Candidate(_) => &["coder"],
            Envelope::Claim(_) => &["planner", "coder", "tester"],
            // The scheduler proposes deterministic, runtime-built actions
            // (e.g. promoting a candidate that passed its checks).
            Envelope::Action(_) => &["planner", "coder", "scheduler"],
        };
        if !allowed.contains(&sess.role.as_str()) {
            return Err(one("ROLE_NOT_PERMITTED"));
        }
        let proposal_id = format!("prop:{}", tx.next_seq());
        let mut extra = json!({});
        let (body, payload_digest) = match env {
            Envelope::Plan(p) => {
                if let Some(bad) = p
                    .premise_refs
                    .iter()
                    .find(|r| !tx.state.nodes.contains_key(*r))
                {
                    return Err(vec![Reason::new("UNKNOWN_PREMISE").missing(bad)]);
                }
                let d = digest("plan", &json!({"task_id": p.task_id, "summary": p.summary, "steps": p.steps, "premise_refs": p.premise_refs}))
                    .map_err(|_| one("SCHEMA_INVALID"))?;
                (
                    ProposalBody::Plan {
                        summary: p.summary,
                        steps: p.steps,
                        premise_refs: p.premise_refs,
                    },
                    d,
                )
            }
            Envelope::Candidate(c) => {
                let base = tx
                    .state
                    .trees
                    .get(&c.base_root)
                    .cloned()
                    .ok_or_else(|| one("UNKNOWN_BASE_ROOT"))?;
                if c.files.is_empty() && c.deletions.is_empty() {
                    return Err(one("EMPTY_CANDIDATE"));
                }
                for p in c.files.keys().chain(c.deletions.iter()) {
                    validate_path(p).map_err(|r| vec![r])?;
                    if policy.is_protected(p) {
                        return Err(vec![Reason::new("PROTECTED_PATH")
                            .subject(p)
                            .next(&["REPLAN", "ESCALATE"])]);
                    }
                }
                let is_repair = task.failed_candidates > 0;
                if is_repair && task.repairs_used >= task.max_repairs {
                    return Err(vec![Reason::new("REPAIR_BUDGET_EXHAUSTED")
                        .subject(&task_id)
                        .next(&["ESCALATE", "STOP"])
                        .budget(0)]);
                }
                let mut manifest = base;
                for d in &c.deletions {
                    if manifest.remove(d).is_none() {
                        return Err(vec![Reason::new("UNKNOWN_PATH").subject(d)]);
                    }
                }
                for (p, content) in &c.files {
                    manifest.insert(p.clone(), tx.put_blob(content.clone().into_bytes()));
                }
                let root = digest("tree", &manifest).map_err(|_| one("SCHEMA_INVALID"))?;
                if !tx.state.trees.contains_key(&root) {
                    tx.emit(
                        "core",
                        EventBody::TreeRegistered {
                            root: root.clone(),
                            manifest,
                        },
                    )
                    .map_err(|_| one("INTERNAL"))?;
                }
                let d = digest(
                    "candidate",
                    &json!({"task_id": task_id, "base_root": c.base_root, "candidate_root": root}),
                )
                .map_err(|_| one("SCHEMA_INVALID"))?;
                extra = json!({"candidate_root": root});
                (
                    ProposalBody::Candidate {
                        base_root: c.base_root,
                        candidate_root: root,
                        changed_paths: c.files.keys().cloned().collect(),
                        deletions: c.deletions,
                        rationale: c.rationale,
                        is_repair,
                    },
                    d,
                )
            }
            Envelope::Claim(c) => {
                if !valid_ident(&c.claim_id) || !c.context.starts_with("ctx:") {
                    return Err(one("SCHEMA_INVALID"));
                }
                let entity = format!("claim:{}", c.claim_id);
                if tx.state.latest.contains_key(&entity) {
                    return Err(vec![Reason::new("ENTITY_EXISTS").subject(&entity)]);
                }
                if let Some(l) = &c.literal {
                    if !valid_ident(&l.atom) {
                        return Err(one("SCHEMA_INVALID"));
                    }
                }
                let seq = tx.next_seq();
                let claim_ref = format!("{entity}@1");
                let node = Node {
                    node_ref: claim_ref.clone(),
                    entity_id: entity,
                    revision: 1,
                    lifecycle: Lifecycle::Current,
                    meta: meta(seq, &sess.principal, tx.now, Sensitivity::Internal),
                    // A model can only propose; it cannot assert an observation.
                    body: NodeBody::Claim {
                        proposition: c.proposition.clone(),
                        context: c.context.clone(),
                        literal: c.literal.clone(),
                        basis: Basis::Derived,
                        support_refs: vec![],
                    },
                };
                tx.emit(&sess.principal, EventBody::NodeAdded { node })
                    .map_err(|_| one("INTERNAL"))?;
                let mut derivation_ref = None;
                if let Some(d) = c.derivation {
                    for p in &d.premises {
                        if !matches!(
                            tx.state.nodes.get(p).map(|n| &n.body),
                            Some(NodeBody::Claim { .. })
                        ) {
                            return Err(vec![Reason::new("UNKNOWN_PREMISE").missing(p)]);
                        }
                    }
                    if !matches!(
                        tx.state.nodes.get(&d.rule_id).map(|n| &n.body),
                        Some(NodeBody::Rule { .. })
                    ) {
                        return Err(vec![Reason::new("UNKNOWN_RULE").missing(&d.rule_id)]);
                    }
                    let seq = tx.next_seq();
                    let entity = format!("derivation:{}.{}", c.claim_id, seq);
                    let r = format!("{entity}@1");
                    let node = Node {
                        node_ref: r.clone(),
                        entity_id: entity,
                        revision: 1,
                        lifecycle: Lifecycle::Current,
                        meta: meta(seq, &sess.principal, tx.now, Sensitivity::Internal),
                        body: NodeBody::Derivation {
                            conclusion: claim_ref.clone(),
                            premises: d.premises,
                            rule: d.rule_id,
                            context: c.context.clone(),
                        },
                    };
                    tx.emit(&sess.principal, EventBody::NodeAdded { node })
                        .map_err(|_| one("INTERNAL"))?;
                    derivation_ref = Some(r);
                }
                let d = digest(
                    "claim",
                    &json!({"claim": claim_ref, "derivation": derivation_ref}),
                )
                .map_err(|_| one("INTERNAL"))?;
                extra = json!({"claim_ref": claim_ref, "derivation_ref": derivation_ref});
                (
                    ProposalBody::Claim {
                        claim_ref,
                        derivation_ref,
                    },
                    d,
                )
            }
            Envelope::Action(ac) => {
                if ac.idempotency_key.is_empty() || ac.idempotency_key.len() > 128 {
                    return Err(one("SCHEMA_INVALID"));
                }
                canonical_json(&ac.arguments).map_err(|_| one("NON_CANONICAL_ARGUMENTS"))?;
                let d = action_digest(
                    &tx.state,
                    &task_id,
                    &ac.tool_id,
                    &ac.arguments,
                    &ac.premise_refs,
                    &ac.expected_postconditions,
                )
                .map_err(|_| one("NON_CANONICAL_ARGUMENTS"))?;
                extra = json!({"action_digest": d});
                (
                    ProposalBody::Action {
                        tool_id: ac.tool_id,
                        arguments: ac.arguments,
                        premise_refs: ac.premise_refs,
                        expected_postconditions: ac.expected_postconditions,
                        idempotency_key: ac.idempotency_key,
                    },
                    d,
                )
            }
        };
        let kind = body.kind();
        let proposal = Proposal {
            proposal_id: format!("prop:{}", tx.next_seq()),
            input_id: input_id.to_string(),
            session_id: sess.session_id.clone(),
            principal: sess.principal.clone(),
            task_id,
            payload_digest: payload_digest.clone(),
            recorded_sequence: tx.next_seq(),
            body,
            acceptance: None,
        };
        let _ = proposal_id;
        let pid = proposal.proposal_id.clone();
        tx.emit(&sess.principal, EventBody::ProposalRecorded { proposal })
            .map_err(|_| one("INTERNAL"))?;
        let mut out = json!({"status": "RECORDED", "input_id": input_id, "proposal_id": pid, "kind": kind, "payload_digest": payload_digest});
        if let Value::Object(m) = extra {
            for (k, v) in m {
                out[k] = v;
            }
        }
        Ok(out)
    }

    // ------------------------------------------------------------------
    // Verification
    // ------------------------------------------------------------------

    fn check_plan(&mut self, a: ProposalArgs) -> Result<Value> {
        let s = &self.state;
        let p = s
            .proposals
            .get(&a.proposal_id)
            .ok_or_else(|| CoreError::new("UNKNOWN_PROPOSAL", a.proposal_id.clone()))?;
        let ProposalBody::Candidate {
            base_root,
            candidate_root,
            ..
        } = &p.body
        else {
            return Err(CoreError::bad_args(
                "check_plan applies to candidate proposals",
            ));
        };
        let task = s
            .tasks
            .get(&p.task_id)
            .ok_or_else(|| CoreError::internal("task missing"))?;
        let policy = s.policy()?;
        let manifest = task
            .test_manifest
            .as_ref()
            .and_then(|id| s.test_manifests.get(id))
            .ok_or_else(|| {
                CoreError::new("UNKNOWN_TEST_MANIFEST", format!("{:?}", task.test_manifest))
            })?
            .clone();
        let env = task
            .environment
            .as_ref()
            .and_then(|id| s.environments.get(id))
            .ok_or_else(|| {
                CoreError::new("UNKNOWN_ENVIRONMENT", format!("{:?}", task.environment))
            })?
            .clone();
        let kinds: Vec<String> = policy
            .tools
            .get(PROMOTE_TOOL)
            .map(|t| {
                t.required_checks
                    .iter()
                    .filter(|c| RUNNER_CHECKS.contains(&c.as_str()))
                    .cloned()
                    .collect()
            })
            .unwrap_or_default();
        let fp = Self::fingerprint(s)?;
        let subject = Subject {
            proposal_id: Some(p.proposal_id.clone()),
            base_root: Some(base_root.clone()),
            candidate_root: Some(candidate_root.clone()),
            payload_digest: Some(p.payload_digest.clone()),
            context_id: None,
            premise_digest: None,
        };
        let materialized = self.materialize(candidate_root)?;
        let mut tx = self.begin();
        let mut checks = Vec::new();
        for kind in kinds {
            let existing = tx.state.checks.values().find(|c| {
                !c.reported
                    && c.check_kind == kind
                    && c.subject == subject
                    && c.dependency_fingerprint == fp
                    && c.policy_digest == tx.state.policy_digest
            });
            let check = match existing {
                Some(c) => c.clone(),
                None => {
                    let c = CheckRequest {
                        check_id: format!("chk:{}", tx.next_seq()),
                        check_kind: kind.clone(),
                        subject: subject.clone(),
                        test_manifest_id: manifest.manifest_id.clone(),
                        test_manifest_digest: manifest.digest.clone(),
                        environment_id: env.environment_id.clone(),
                        environment_digest: env.digest.clone(),
                        validation_snapshot: tx.state.sequence,
                        dependency_fingerprint: fp.clone(),
                        policy_digest: tx.state.policy_digest.clone(),
                        reported: false,
                    };
                    tx.emit(
                        "core:verifier",
                        EventBody::CheckRequested { check: c.clone() },
                    )?;
                    c
                }
            };
            checks.push(json!({
                "check_id": check.check_id, "check_kind": check.check_kind, "subject": check.subject,
                "test_manifest": {"id": manifest.manifest_id, "digest": manifest.digest, "cases": manifest.cases, "entrypoint": manifest.entrypoint},
                "environment": {"id": env.environment_id, "digest": env.digest, "worker_kind": env.worker_kind},
                "validation_snapshot": check.validation_snapshot, "dependency_fingerprint": check.dependency_fingerprint,
                "policy_version": policy.version,
            }));
        }
        self.commit(tx)?;
        Ok(json!({"checks": checks, "materialized": materialized}))
    }

    fn report_check(&mut self, a: ReportCheck) -> Result<Value> {
        let sess = self.session(&a.session_id, &["runner"])?;
        let check = self
            .state
            .checks
            .get(&a.check_id)
            .cloned()
            .ok_or_else(|| CoreError::new("UNKNOWN_CHECK", a.check_id.clone()))?;
        if check.reported {
            return Err(CoreError::new("ALREADY_REPORTED", a.check_id));
        }
        let policy = self.state.policy()?.clone();
        let mut tx = self.begin();
        if !policy.is_trusted_issuer(&check.check_kind, &sess.principal, &a.implementation_digest) {
            let reasons = vec![Reason::new("UNTRUSTED_ISSUER")
                .check(&check.check_kind)
                .subject(&sess.principal)];
            tx.emit(
                &sess.principal,
                EventBody::CheckReportRejected {
                    check_id: check.check_id.clone(),
                    reasons: reasons.clone(),
                },
            )?;
            self.commit(tx)?;
            return Ok(rejected(reasons));
        }
        let manifest = tx
            .state
            .test_manifests
            .get(&check.test_manifest_id)
            .cloned()
            .ok_or_else(|| CoreError::internal("manifest"))?;
        let expected: BTreeSet<&str> = manifest.cases.iter().map(|c| c.name.as_str()).collect();
        let reported: BTreeMap<&str, &str> = a
            .cases
            .iter()
            .map(|c| (c.name.as_str(), c.status.as_str()))
            .collect();
        let failed: Vec<String> = manifest
            .cases
            .iter()
            .filter(|c| reported.get(c.name.as_str()) != Some(&"PASS"))
            .map(|c| c.name.clone())
            .collect();
        let any_fail = reported.values().any(|s| *s == "FAIL");
        let exact = a.completed
            && a.collected == expected.len() as u64
            && reported.len() == a.cases.len()
            && reported.keys().copied().collect::<BTreeSet<_>>() == expected;
        // Never trust a pass string: PASS requires the complete expected manifest.
        let result = match a.result.as_str() {
            "PASS" if exact && failed.is_empty() => CheckResult::Pass,
            "PASS" | "FAIL" if any_fail => CheckResult::Fail,
            "FAIL" => CheckResult::Fail,
            "TIMEOUT" => CheckResult::Timeout,
            "ERROR" => CheckResult::Error,
            _ => CheckResult::Unknown,
        };
        let env = tx.state.environments.get(&check.environment_id).cloned();
        let mut limits = vec![
            "evidence about this run of this manifest only; not proof of general correctness"
                .to_string(),
        ];
        if env.as_ref().is_none_or(|e| e.worker_kind == "fake") {
            limits.push("SIMULATED: fake worker; candidate code was not executed".into());
        }
        let mut summary = a.summary;
        summary.truncate(MAX_SUMMARY);
        let verification_id = format!("ver:{}", tx.next_seq());
        let v = Verification {
            verification_id: verification_id.clone(),
            check_id: Some(check.check_id.clone()),
            check_kind: check.check_kind.clone(),
            issuer: sess.principal.clone(),
            implementation_digest: a.implementation_digest,
            subject: check.subject.clone(),
            test_manifest_digest: Some(check.test_manifest_digest.clone()),
            environment_digest: Some(check.environment_digest.clone()),
            validation_snapshot: check.validation_snapshot,
            dependency_fingerprint: check.dependency_fingerprint.clone(),
            policy_digest: check.policy_digest.clone(),
            result,
            failed_cases: failed,
            applicability_limits: limits,
            public_summary: summary,
        };
        tx.emit(
            &sess.principal,
            EventBody::VerificationIssued {
                verification: v.clone(),
            },
        )?;
        self.commit(tx)?;
        Ok(
            json!({"status": "ISSUED", "verification_id": verification_id, "result": result, "failed_cases": v.failed_cases}),
        )
    }

    fn evaluate_context(&mut self, a: ContextArgs) -> Result<Value> {
        let now = self.now();
        let compiled = akg::compile_context(&self.state, &a.context_id, now);
        let fp = Self::fingerprint(&self.state)?;
        let premise_digest = digest("premises", &compiled.premise_refs)?;
        let implementation = self
            .deductor
            .implementation_digest()
            .unwrap_or_else(|e| format!("unavailable:{}", e.code));
        let mut derivations: BTreeMap<String, DerivationEval> = BTreeMap::new();
        let seq = self.state.sequence + 1;
        for (d, why) in &compiled.invalid {
            derivations.insert(
                d.clone(),
                DerivationEval {
                    status: DerivationStatus::Invalid,
                    evaluated_sequence: seq,
                    reason: Some(why.clone()),
                },
            );
        }
        let empty = compiled.problem.facts.is_empty() && compiled.problem.rules.is_empty();
        let (result, consistent, conflicts, summary) = if empty {
            (
                CheckResult::Unknown,
                false,
                vec![],
                "empty formal context: nothing to establish".to_string(),
            )
        } else {
            match self.deductor.run(&compiled.problem) {
                Err(e) => (
                    CheckResult::Error,
                    false,
                    vec![],
                    format!("{}: {}", e.code, e.message),
                ),
                Ok(out) => {
                    let conflicts: Vec<String> = out
                        .conflicts
                        .iter()
                        .map(|&i| compiled.atoms[i as usize - 1].clone())
                        .collect();
                    let conflicted: BTreeSet<u64> = out.conflicts.iter().copied().collect();
                    for (i, dref) in compiled.rule_derivations.iter().enumerate() {
                        let body = &compiled.derivation_bodies[dref];
                        let head = compiled.problem.rules[i].head;
                        let touches = std::iter::once(head)
                            .chain(body.iter().copied())
                            .any(|l| conflicted.contains(&l.unsigned_abs()));
                        let (status, reason) = if touches {
                            (
                                DerivationStatus::Indeterminate,
                                Some("CONTEXT_INCONSISTENT".to_string()),
                            )
                        } else if body.iter().all(|l| out.contains(*l)) {
                            (DerivationStatus::Valid, None)
                        } else {
                            (
                                DerivationStatus::Invalid,
                                Some("UNGROUNDED_OR_CIRCULAR".to_string()),
                            )
                        };
                        derivations.insert(
                            dref.clone(),
                            DerivationEval {
                                status,
                                evaluated_sequence: seq,
                                reason,
                            },
                        );
                    }
                    let r = if out.consistent {
                        CheckResult::Pass
                    } else {
                        CheckResult::Fail
                    };
                    let summary = format!(
                        "{} literals derived, {} conflicts",
                        out.derived.len(),
                        out.conflicts.len()
                    );
                    (r, out.consistent, conflicts, summary)
                }
            }
        };
        let mut tx = self.begin();
        let verification_id = format!("ver:{}", tx.next_seq());
        tx.emit(
            "core:deductor",
            EventBody::VerificationIssued {
                verification: Verification {
                    verification_id: verification_id.clone(),
                    check_id: None,
                    check_kind: "formal_context".into(),
                    issuer: format!("core:deductor:{}", self.deductor.kind()),
                    implementation_digest: implementation,
                    subject: Subject {
                        proposal_id: None,
                        base_root: None,
                        candidate_root: None,
                        payload_digest: None,
                        context_id: Some(a.context_id.clone()),
                        premise_digest: Some(premise_digest),
                    },
                    test_manifest_digest: None,
                    environment_digest: None,
                    validation_snapshot: tx.state.sequence,
                    dependency_fingerprint: fp.clone(),
                    policy_digest: tx.state.policy_digest.clone(),
                    result,
                    failed_cases: conflicts.clone(),
                    applicability_limits: vec![
                        "bounded finite ground Horn fragment only; says nothing about natural-language consistency".into(),
                    ],
                    public_summary: summary,
                },
            },
        )?;
        let evaluation = ContextEval {
            context_id: a.context_id.clone(),
            evaluated_sequence: tx.next_seq(),
            dependency_fingerprint: fp,
            premise_refs: compiled.premise_refs.clone(),
            consistent: consistent && result == CheckResult::Pass,
            conflicts: conflicts.clone(),
            verification_id: verification_id.clone(),
        };
        tx.emit(
            "core:deductor",
            EventBody::ContextEvaluated {
                evaluation,
                derivations: derivations.clone(),
            },
        )?;
        self.commit(tx)?;
        let ds: Vec<Value> = derivations
            .iter()
            .map(|(k, v)| json!({"derivation_id": k, "status": v.status, "reason": v.reason}))
            .collect();
        Ok(
            json!({"verification_id": verification_id, "result": result, "consistent": consistent, "conflicts": conflicts, "derivations": ds}),
        )
    }

    // ------------------------------------------------------------------
    // Admission
    // ------------------------------------------------------------------

    fn admit(&mut self, a: ProposalArgs) -> Result<Value> {
        let s = self.state.clone();
        let now = self.now();
        let p = s
            .proposals
            .get(&a.proposal_id)
            .ok_or_else(|| CoreError::new("UNKNOWN_PROPOSAL", a.proposal_id.clone()))?;
        let ProposalBody::Action {
            tool_id,
            arguments,
            premise_refs,
            expected_postconditions,
            idempotency_key,
        } = &p.body
        else {
            return Err(CoreError::bad_args("admit applies to action proposals"));
        };
        let policy = s.policy()?;
        let ad = action_digest(
            &s,
            &p.task_id,
            tool_id,
            arguments,
            premise_refs,
            expected_postconditions,
        )?;
        let mut tx = self.begin();

        // Scoped idempotency: same key + same payload returns the existing action.
        if let Some(existing) = s.idempotency.get(idempotency_key) {
            let act = &s.actions[existing];
            if act.action_digest == ad {
                return Ok(
                    json!({"status": "EXISTING", "action_id": act.action_id, "action_digest": ad, "dispatch_state": act.state}),
                );
            }
            let reasons = vec![Reason::new("IDEMPOTENCY_CONFLICT")
                .subject(idempotency_key)
                .next(&["REPLAN"])];
            tx.emit(
                "core:admission",
                EventBody::AdmissionRejected {
                    proposal_id: p.proposal_id.clone(),
                    action_digest: Some(ad.clone()),
                    reasons: reasons.clone(),
                },
            )?;
            self.commit(tx)?;
            return Ok(json!({"status": "REJECTED", "action_digest": ad, "reasons": reasons}));
        }

        let mut reasons: Vec<Reason> = Vec::new();
        let task = &s.tasks[&p.task_id];
        let remaining = task.max_repairs.saturating_sub(task.repairs_used);
        if task.status != TaskStatus::Open {
            reasons.push(
                Reason::new("TASK_NOT_OPEN")
                    .subject(&task.task_id)
                    .next(&["STOP"]),
            );
        }
        let Some(tool) = policy.tools.get(tool_id) else {
            // Unknown capability: fail closed.
            reasons.push(
                Reason::new("UNKNOWN_TOOL")
                    .subject(tool_id)
                    .next(&["REPLAN", "ESCALATE"]),
            );
            tx.emit(
                "core:admission",
                EventBody::AdmissionRejected {
                    proposal_id: p.proposal_id.clone(),
                    action_digest: Some(ad.clone()),
                    reasons: reasons.clone(),
                },
            )?;
            self.commit(tx)?;
            return Ok(json!({"status": "REJECTED", "action_digest": ad, "reasons": reasons}));
        };
        for r in premise_refs.iter().chain(task.requirement_refs.iter()) {
            if let Err(problem) = akg::premise_usable(&s, r, now) {
                reasons.push(Reason::new(problem.code()).missing(r).next(&[
                    "GATHER_CONTEXT",
                    "REPLAN",
                    "ESCALATE",
                ]));
            }
        }
        let fp = Self::fingerprint(&s)?;
        let mut verification_ids = Vec::new();
        let mut candidate_root = None;
        let mut reservation_scope = format!("tool:{tool_id}");

        if tool_id == PROMOTE_TOOL {
            reservation_scope = "approved_root".into();
            let root = arguments
                .as_object()
                .filter(|o| o.len() == 1)
                .and_then(|o| o.get("candidate_root"))
                .and_then(|v| v.as_str());
            match root {
                None => reasons.push(
                    Reason::new("SCHEMA_INVALID")
                        .subject("arguments")
                        .next(&["REPLAN"]),
                ),
                Some(root) => {
                    candidate_root = Some(root.to_string());
                    let candidates: Vec<&Proposal> = task
                        .candidates
                        .iter()
                        .filter_map(|c| s.proposals.get(c))
                        .filter(|c| matches!(&c.body, ProposalBody::Candidate { candidate_root, .. } if candidate_root == root))
                        .collect();
                    if candidates.is_empty() {
                        reasons.push(
                            Reason::new("UNKNOWN_CANDIDATE")
                                .subject(root)
                                .next(&["REPLAN"]),
                        );
                    }
                    if !candidates.iter().any(|c| matches!(&c.body, ProposalBody::Candidate { base_root, .. } if *base_root == s.approved_root)) {
                        reasons.push(Reason::new("BASE_ROOT_CHANGED").subject(root).missing(&s.approved_root).next(&["REPAIR", "REPLAN"]));
                    }
                }
            }
        }

        for check in &tool.required_checks {
            match check.as_str() {
                "acceptance_tests" => {
                    let Some(root) = candidate_root.as_deref() else {
                        reasons.push(
                            Reason::new("UNSUPPORTED_CHECK")
                                .check(check)
                                .next(&["ESCALATE"]),
                        );
                        continue;
                    };
                    match self.acceptance_applicable(&s, task, root, &fp) {
                        Ok(v) => verification_ids.push(v),
                        Err(r) => reasons.push(r.budget(remaining)),
                    }
                }
                "formal_context" => {
                    if task.contexts.is_empty() {
                        reasons.push(
                            Reason::new("FORMAL_CONTEXT_MISSING")
                                .check(check)
                                .next(&["ESCALATE"]),
                        );
                    }
                    for ctx in &task.contexts {
                        match s.contexts.get(ctx) {
                            None => reasons.push(
                                Reason::new("FORMAL_CONTEXT_MISSING")
                                    .check(check)
                                    .subject(ctx)
                                    .next(&["GATHER_CONTEXT"]),
                            ),
                            Some(e) if e.dependency_fingerprint != fp => reasons.push(
                                Reason::new("FORMAL_CONTEXT_STALE")
                                    .check(check)
                                    .subject(ctx)
                                    .next(&["GATHER_CONTEXT"]),
                            ),
                            Some(e) if !e.consistent => reasons.push(
                                Reason::new("FORMAL_CONTEXT_INCONSISTENT")
                                    .check(check)
                                    .subject(ctx)
                                    .counterexample(&e.verification_id)
                                    .next(&["REPLAN", "ESCALATE"]),
                            ),
                            Some(e) => verification_ids.push(e.verification_id.clone()),
                        }
                    }
                }
                other => reasons.push(
                    Reason::new("UNSUPPORTED_CHECK")
                        .check(other)
                        .next(&["ESCALATE"]),
                ),
            }
        }

        if s.actions
            .values()
            .any(|x| x.holds_reservation() && x.reservation_scope == reservation_scope)
        {
            reasons.push(
                Reason::new("RESOURCE_CONFLICT")
                    .subject(&reservation_scope)
                    .next(&["REPAIR", "REPLAN"]),
            );
        }

        let mut approval_ids = Vec::new();
        if tool.requires_approval {
            let approval = s.approvals.values().find(|ap| {
                ap.action_digest == ad
                    && &ap.tool_id == tool_id
                    && !ap.revoked
                    && ap.consumed_by.is_none()
                    && now < ap.expires_at
                    && ap.policy_digest == s.policy_digest
            });
            match approval {
                Some(ap) => approval_ids.push(ap.approval_id.clone()),
                None => reasons.push(
                    Reason::new("MISSING_APPROVAL")
                        .subject(&ad)
                        .next(&["REQUEST_APPROVAL", "ESCALATE"]),
                ),
            }
        }

        if !reasons.is_empty() {
            tx.emit(
                "core:admission",
                EventBody::AdmissionRejected {
                    proposal_id: p.proposal_id.clone(),
                    action_digest: Some(ad.clone()),
                    reasons: reasons.clone(),
                },
            )?;
            self.commit(tx)?;
            return Ok(json!({"status": "REJECTED", "action_digest": ad, "reasons": reasons}));
        }

        for v in &verification_ids {
            tx.emit(
                "core:admission",
                EventBody::ApplicabilityRecorded {
                    record: ApplicabilityRecord {
                        verification_id: v.clone(),
                        current_snapshot: s.sequence,
                        current_dependency_fingerprint: fp.clone(),
                        current_policy_digest: s.policy_digest.clone(),
                        checker: format!("core:admission/v{PROTOCOL_VERSION}"),
                        applicable: true,
                    },
                },
            )?;
        }
        let action_id = format!("act:{}", tx.next_seq());
        let action = Action {
            action_id: action_id.clone(),
            proposal_id: p.proposal_id.clone(),
            task_id: p.task_id.clone(),
            requesting_principal: p.principal.clone(),
            tool_id: tool_id.clone(),
            effect: tool.effect,
            arguments: arguments.clone(),
            premise_refs: premise_refs.clone(),
            expected_postconditions: expected_postconditions.clone(),
            idempotency_key: idempotency_key.clone(),
            action_digest: ad.clone(),
            base_snapshot_sequence: s.sequence,
            base_root: s.approved_root.clone(),
            candidate_root,
            dependency_fingerprint: fp,
            policy_digest: s.policy_digest.clone(),
            approval_ids,
            verification_ids,
            reservation_scope,
            state: DispatchState::AuthorizedIntent,
            attempts: vec![],
            outcome: None,
            postconditions: None,
            requires_human_review: false,
            reasons: vec![],
        };
        // Intent, outbox entry and reservation are one event in one transaction.
        tx.emit("core:admission", EventBody::IntentAuthorized { action })?;
        self.fault("during_authorization")?;
        self.commit(tx)?;
        Ok(json!({"status": "AUTHORIZED", "action_id": action_id, "action_digest": ad}))
    }

    fn acceptance_applicable(
        &self,
        s: &State,
        task: &Task,
        root: &str,
        fp: &str,
    ) -> std::result::Result<String, Reason> {
        let policy = s.policy().map_err(|_| Reason::new("INTERNAL"))?;
        // A task without a protected manifest/environment can never satisfy
        // acceptance_tests: None never equals a receipt's Some(..).
        let manifest = task
            .test_manifest
            .as_ref()
            .and_then(|id| s.test_manifests.get(id))
            .map(|m| m.digest.clone());
        let env = task
            .environment
            .as_ref()
            .and_then(|id| s.environments.get(id))
            .map(|e| e.digest.clone());
        if manifest.is_none() || env.is_none() {
            return Err(Reason::new("MISSING_TEST_MANIFEST")
                .check("acceptance_tests")
                .subject(root)
                .next(&["ESCALATE"]));
        }
        let for_root: Vec<&Verification> = s
            .verifications
            .values()
            .filter(|v| {
                v.check_kind == "acceptance_tests"
                    && v.subject.candidate_root.as_deref() == Some(root)
            })
            .collect();
        let applicable = for_root.iter().filter(|v| {
            v.result == CheckResult::Pass
                && v.subject.base_root.as_deref() == Some(s.approved_root.as_str())
                && policy.is_trusted_issuer(&v.check_kind, &v.issuer, &v.implementation_digest)
                && v.dependency_fingerprint == fp
                && v.policy_digest == s.policy_digest
                && v.test_manifest_digest == manifest
                && v.environment_digest == env
        });
        if let Some(v) = applicable.max_by_key(|v| v.validation_snapshot) {
            return Ok(v.verification_id.clone());
        }
        let r = Reason::new("").check("acceptance_tests").subject(root);
        if let Some(v) = for_root
            .iter()
            .filter(|v| v.result == CheckResult::Pass)
            .max_by_key(|v| v.validation_snapshot)
        {
            return Err(Reason {
                code: "STALE_VERIFICATION".into(),
                ..r.counterexample(&v.verification_id)
                    .next(&["GATHER_CONTEXT", "REPAIR"])
            });
        }
        if let Some(v) = for_root.iter().max_by_key(|v| v.validation_snapshot) {
            let code = if v.result == CheckResult::Fail {
                "CHECK_FAILED"
            } else {
                "CHECK_NOT_PASSED"
            };
            return Err(Reason {
                code: code.into(),
                ..r.counterexample(&v.verification_id).next(&[
                    "REPAIR",
                    "GATHER_CONTEXT",
                    "ESCALATE",
                ])
            });
        }
        let task_has_receipts = s.verifications.values().any(|v| {
            v.check_kind == "acceptance_tests"
                && v.subject
                    .proposal_id
                    .as_ref()
                    .is_some_and(|p| task.candidates.contains(p))
        });
        let code = if task_has_receipts {
            "ARTIFACT_BINDING_MISMATCH"
        } else {
            "MISSING_VERIFICATION"
        };
        Err(Reason {
            code: code.into(),
            ..r.next(&["REPAIR", "GATHER_CONTEXT"])
        })
    }

    // ------------------------------------------------------------------
    // Dispatch and effects
    // ------------------------------------------------------------------

    fn dispatch_begin(&mut self, a: ActionArgs) -> Result<Value> {
        let s = self.state.clone();
        let now = self.now();
        let act = s
            .actions
            .get(&a.action_id)
            .ok_or_else(|| CoreError::new("UNKNOWN_ACTION", a.action_id.clone()))?;
        if act.state != DispatchState::AuthorizedIntent {
            return Ok(
                json!({"status": "NOT_ELIGIBLE", "dispatch_state": act.state, "reasons": [Reason::new("NOT_ELIGIBLE").subject(&act.action_id)]}),
            );
        }
        // Launch boundary: re-check everything against the current head.
        let mut stop: Option<(DispatchState, Reason)> = None;
        let task = &s.tasks[&act.task_id];
        let recomputed = action_digest(
            &s,
            &act.task_id,
            &act.tool_id,
            &act.arguments,
            &act.premise_refs,
            &act.expected_postconditions,
        );
        if task.status != TaskStatus::Open {
            stop = Some((
                DispatchState::Cancelled,
                Reason::new("TASK_NOT_OPEN").subject(&task.task_id),
            ));
        } else if s.policy_digest != act.policy_digest {
            stop = Some((
                DispatchState::Expired,
                Reason::new("POLICY_CHANGED").next(&["REPLAN"]),
            ));
        } else if recomputed.as_deref().ok() != Some(act.action_digest.as_str()) {
            stop = Some((
                DispatchState::Expired,
                Reason::new("PAYLOAD_IDENTITY_MISMATCH").next(&["REPLAN"]),
            ));
        } else if Self::fingerprint(&s)? != act.dependency_fingerprint {
            stop = Some((
                DispatchState::Expired,
                Reason::new("STALE_DEPENDENCY").next(&["GATHER_CONTEXT", "REPLAN"]),
            ));
        } else if s.approved_root != act.base_root {
            stop = Some((
                DispatchState::Expired,
                Reason::new("BASE_ROOT_CHANGED").next(&["REPAIR"]),
            ));
        }
        for ap in &act.approval_ids {
            if stop.is_some() {
                break;
            }
            let approval = &s.approvals[ap];
            if approval.revoked {
                stop = Some((
                    DispatchState::Cancelled,
                    Reason::new("APPROVAL_REVOKED").subject(ap),
                ));
            } else if now >= approval.expires_at {
                stop = Some((
                    DispatchState::Expired,
                    Reason::new("APPROVAL_EXPIRED")
                        .subject(ap)
                        .next(&["REQUEST_APPROVAL"]),
                ));
            }
        }
        for r in act.premise_refs.iter().chain(task.requirement_refs.iter()) {
            if stop.is_some() {
                break;
            }
            if let Err(problem) = akg::premise_usable(&s, r, now) {
                stop = Some((
                    DispatchState::Expired,
                    Reason::new(problem.code()).missing(r),
                ));
            }
        }
        let mut tx = self.begin();
        if let Some((state, reason)) = stop {
            let status = if state == DispatchState::Cancelled {
                "CANCELLED"
            } else {
                "EXPIRED"
            };
            tx.emit(
                "core:launch",
                EventBody::DispatchStopped {
                    action_id: act.action_id.clone(),
                    state,
                    reasons: vec![reason.clone()],
                },
            )?;
            self.commit(tx)?;
            return Ok(json!({"status": status, "reasons": [reason]}));
        }
        let attempt_id = format!("att:{}", tx.next_seq());
        tx.emit(
            "core:launch",
            EventBody::DispatchStarted {
                action_id: act.action_id.clone(),
                attempt_id: attempt_id.clone(),
            },
        )?;
        if act.effect == Effect::External {
            // Durable STARTED record precedes handoff to the gateway.
            self.commit(tx)?;
            return Ok(json!({
                "status": "STARTED", "attempt_id": attempt_id, "tool_id": act.tool_id,
                "arguments": act.arguments, "idempotency_key": act.idempotency_key,
            }));
        }
        // Internal effect: local promotion is one atomic transition.
        if act.tool_id != PROMOTE_TOOL {
            return Err(CoreError::new("UNKNOWN_INTERNAL_TOOL", act.tool_id.clone()));
        }
        let to = act
            .candidate_root
            .clone()
            .ok_or_else(|| CoreError::internal("promotion without candidate"))?;
        let from = tx.state.approved_root.clone();
        tx.emit(
            "core:launch",
            EventBody::ApprovedRootAdvanced {
                action_id: act.action_id.clone(),
                from: from.clone(),
                to: to.clone(),
            },
        )?;
        let obs = tx.put_blob(canonical_json(&json!({"approved_root": to}))?.into_bytes());
        tx.emit(
            "core:launch",
            EventBody::OutcomeObserved {
                action_id: act.action_id.clone(),
                attempt_id: attempt_id.clone(),
                outcome: Outcome {
                    outcome: "SUCCEEDED".into(),
                    observation_blob: obs,
                    via: "internal".into(),
                },
            },
        )?;
        let pass = tx.state.approved_root == to;
        tx.emit(
            "core:verifier",
            EventBody::PostconditionsEvaluated {
                action_id: act.action_id.clone(),
                result: if pass {
                    CheckResult::Pass
                } else {
                    CheckResult::Fail
                },
                details: "approved_root == candidate_root".into(),
            },
        )?;
        self.fault("during_promotion")?;
        self.commit(tx)?;
        Ok(
            json!({"status": "COMPLETED", "attempt_id": attempt_id, "result": {"from": from, "approved_root": to, "postconditions": if pass {"PASS"} else {"FAIL"}}}),
        )
    }

    fn evaluate_postconditions(
        act: &Action,
        observation: &Value,
        outcome: &str,
    ) -> (CheckResult, String) {
        if outcome != "SUCCEEDED" {
            return (CheckResult::Fail, "tool reported failure".into());
        }
        if act.expected_postconditions.is_empty() {
            return (
                CheckResult::Unknown,
                "no machine-checkable postconditions declared".into(),
            );
        }
        for pc in &act.expected_postconditions {
            // Supported form: `observation.<field> == arguments.<field>`.
            let parts: Vec<&str> = pc.split(" == ").collect();
            let (Some(l), Some(r)) = (
                parts.first().and_then(|x| x.strip_prefix("observation.")),
                parts.get(1).and_then(|x| x.strip_prefix("arguments.")),
            ) else {
                return (
                    CheckResult::Unknown,
                    format!("unsupported postcondition {pc:?}"),
                );
            };
            if parts.len() != 2 {
                return (
                    CheckResult::Unknown,
                    format!("unsupported postcondition {pc:?}"),
                );
            }
            match (observation.get(l), act.arguments.get(r)) {
                (Some(o), Some(x)) if o == x => {}
                (Some(_), Some(_)) => return (CheckResult::Fail, format!("{pc} does not hold")),
                _ => return (CheckResult::Unknown, format!("{pc}: field missing")),
            }
        }
        (
            CheckResult::Pass,
            "all declared postconditions hold on the recorded observation".into(),
        )
    }

    fn record_outcome(&mut self, a: RecordOutcome) -> Result<Value> {
        let sess = self.session(&a.session_id, &["gateway"])?;
        let act = self
            .state
            .actions
            .get(&a.action_id)
            .cloned()
            .ok_or_else(|| CoreError::new("UNKNOWN_ACTION", a.action_id.clone()))?;
        if !matches!(
            act.state,
            DispatchState::DispatchStarted | DispatchState::OutcomeUnknown
        ) {
            return Err(CoreError::new(
                "NOT_EXPECTING_OUTCOME",
                format!("{:?}", act.state),
            ));
        }
        if act.attempts.last().map(|x| &x.attempt_id) != Some(&a.attempt_id) {
            return Err(CoreError::new("ATTEMPT_MISMATCH", a.attempt_id));
        }
        if a.outcome != "SUCCEEDED" && a.outcome != "FAILED" {
            return Err(CoreError::bad_args("outcome must be SUCCEEDED or FAILED"));
        }
        canonical_json(&a.observation)?;
        let mut tx = self.begin();
        // Tool output is untrusted data, stored by reference.
        let blob = tx.put_blob(canonical_json(&a.observation)?.into_bytes());
        tx.emit(
            &sess.principal,
            EventBody::OutcomeObserved {
                action_id: act.action_id.clone(),
                attempt_id: a.attempt_id.clone(),
                outcome: Outcome {
                    outcome: a.outcome.clone(),
                    observation_blob: blob,
                    via: "gateway".into(),
                },
            },
        )?;
        let (result, details) = Self::evaluate_postconditions(&act, &a.observation, &a.outcome);
        tx.emit(
            "core:verifier",
            EventBody::PostconditionsEvaluated {
                action_id: act.action_id.clone(),
                result,
                details: details.clone(),
            },
        )?;
        self.commit(tx)?;
        Ok(json!({"status": "OUTCOME_OBSERVED", "postconditions": result, "details": details}))
    }

    fn record_outcome_unknown(&mut self, a: RecordUnknown) -> Result<Value> {
        let sess = self.session(&a.session_id, &["gateway"])?;
        let act = self
            .state
            .actions
            .get(&a.action_id)
            .cloned()
            .ok_or_else(|| CoreError::new("UNKNOWN_ACTION", a.action_id.clone()))?;
        if act.state != DispatchState::DispatchStarted {
            return Err(CoreError::new(
                "NOT_EXPECTING_OUTCOME",
                format!("{:?}", act.state),
            ));
        }
        let mut tx = self.begin();
        let mut reason = a.error;
        reason.truncate(MAX_SUMMARY);
        tx.emit(
            &sess.principal,
            EventBody::OutcomeUnknown {
                action_id: a.action_id,
                attempt_id: a.attempt_id,
                reason,
            },
        )?;
        self.commit(tx)?;
        Ok(json!({"status": "OUTCOME_UNKNOWN"}))
    }

    fn pending_reconciliation(&self) -> Result<Value> {
        let actions: Vec<Value> = self
            .state
            .actions
            .values()
            .filter(|a| a.state == DispatchState::OutcomeUnknown)
            .map(|a| json!({
                "action_id": a.action_id, "attempt_id": a.attempts.last().map(|x| x.attempt_id.clone()),
                "tool_id": a.tool_id, "arguments": a.arguments, "idempotency_key": a.idempotency_key,
                "dispatch_state": a.state,
            }))
            .collect();
        Ok(json!({"actions": actions}))
    }

    fn record_reconciliation(&mut self, a: RecordReconciliation) -> Result<Value> {
        let sess = self.session(&a.session_id, &["gateway"])?;
        let act = self
            .state
            .actions
            .get(&a.action_id)
            .cloned()
            .ok_or_else(|| CoreError::new("UNKNOWN_ACTION", a.action_id.clone()))?;
        if act.state != DispatchState::OutcomeUnknown {
            return Err(CoreError::new("NOT_UNCERTAIN", format!("{:?}", act.state)));
        }
        let effect: Option<bool> = a.finding.get("effect_observed").and_then(|v| v.as_bool());
        let details = a.finding.get("details").cloned().unwrap_or(Value::Null);
        canonical_json(&details)?;
        let mut tx = self.begin();
        let blob = tx.put_blob(canonical_json(&details)?.into_bytes());
        let attempt = act
            .attempts
            .last()
            .map(|x| x.attempt_id.clone())
            .unwrap_or_default();
        let (new_state, review) = match effect {
            Some(true) => (DispatchState::OutcomeObserved, false),
            // Not applied: never retried automatically; a human decides.
            Some(false) => (DispatchState::ReconciledNotApplied, true),
            None => (DispatchState::OutcomeUnknown, true),
        };
        tx.emit(
            &sess.principal,
            EventBody::ReconciliationRecorded {
                action_id: act.action_id.clone(),
                effect_observed: effect,
                details_blob: blob.clone(),
                new_state,
                requires_human_review: review,
            },
        )?;
        let mut final_state = new_state;
        if effect == Some(true) {
            tx.emit(
                &sess.principal,
                EventBody::OutcomeObserved {
                    action_id: act.action_id.clone(),
                    attempt_id: attempt,
                    outcome: Outcome {
                        outcome: "SUCCEEDED".into(),
                        observation_blob: blob,
                        via: "reconciliation".into(),
                    },
                },
            )?;
            let (result, d) = Self::evaluate_postconditions(&act, &details, "SUCCEEDED");
            tx.emit(
                "core:verifier",
                EventBody::PostconditionsEvaluated {
                    action_id: act.action_id.clone(),
                    result,
                    details: d,
                },
            )?;
            final_state = DispatchState::PostconditionsEvaluated;
        }
        self.commit(tx)?;
        Ok(json!({"dispatch_state": final_state, "requires_human_review": review}))
    }

    // ------------------------------------------------------------------
    // Routing (deterministic rules first; Jev only for residual judgment)
    // ------------------------------------------------------------------

    pub fn route(
        &self,
        task_id: &str,
        failure_codes: &[String],
    ) -> Result<(Vec<String>, Option<String>, String)> {
        let task = self
            .state
            .tasks
            .get(task_id)
            .ok_or_else(|| CoreError::new("UNKNOWN_TASK", task_id.to_string()))?;
        let v = |xs: &[&str]| xs.iter().map(|x| x.to_string()).collect::<Vec<_>>();
        let has = |c: &str| failure_codes.iter().any(|f| f == c);
        if self.state.shutdown || task.status != TaskStatus::Open {
            return Ok((
                v(&["STOP"]),
                Some("STOP".into()),
                "task not open or runtime shut down".into(),
            ));
        }
        if has("MISSING_APPROVAL")
            || has("APPROVAL_EXPIRED")
            || has("APPROVAL_REVOKED")
            || has("UNKNOWN_TOOL")
            || has("UNTRUSTED_ISSUER")
        {
            return Ok((
                v(&["ESCALATE", "STOP"]),
                Some("ESCALATE".into()),
                "requires human authority".into(),
            ));
        }
        if has("REPAIR_BUDGET_EXHAUSTED")
            || (task.failed_candidates > 0 && task.repairs_used >= task.max_repairs)
        {
            return Ok((
                v(&["ESCALATE", "STOP"]),
                Some("ESCALATE".into()),
                "repair budget exhausted".into(),
            ));
        }
        Ok((
            ROUTE_OPTIONS.iter().map(|s| s.to_string()).collect(),
            None,
            "bounded semantic judgment remains".into(),
        ))
    }

    fn route_cmd(&self, a: RouteArgs) -> Result<Value> {
        let (eligible, choice, reason) = self.route(&a.task_id, &a.failure_codes)?;
        Ok(json!({"eligible": eligible, "deterministic_choice": choice, "reason": reason}))
    }

    fn record_assessment(&mut self, a: AssessmentArgs) -> Result<Value> {
        let sess = self.session(&a.session_id, &["advisor"])?;
        let (eligible, deterministic, _) = self.route(&a.task_id, &a.failure_codes)?;
        let policy = self.state.policy()?.clone();
        for (k, p) in &a.probabilities_bp {
            if *p > 10_000 || !ROUTE_OPTIONS.contains(&k.as_str()) {
                return Err(CoreError::bad_args(
                    "probabilities_bp must map route options to 0..=10000",
                ));
            }
        }
        let mut fallback = a.fallback_reason.clone();
        if fallback.is_none() {
            fallback = if deterministic.is_some() {
                Some("DETERMINISTIC_RULE_APPLIES".into())
            } else if a.eligible != eligible {
                Some("ELIGIBLE_SET_MISMATCH".into())
            } else if a.choice.as_ref().is_none_or(|c| !eligible.contains(c)) {
                Some("CHOICE_NOT_ELIGIBLE".into())
            } else if a.mode != "LIVE" {
                Some(format!("MODE_{}", a.mode))
            } else if policy.jev.mode != "LIVE" {
                Some(format!("POLICY_MODE_{}", policy.jev.mode))
            } else if a
                .confidence_bp
                .is_none_or(|c| c < policy.jev.min_confidence_bp)
            {
                Some("LOW_CONFIDENCE".into())
            } else {
                None
            };
        }
        let used = fallback.is_none();
        let applied = if used {
            a.choice.clone().expect("checked")
        } else if let Some(d) = deterministic {
            d
        } else {
            ["REPAIR", "ESCALATE", "STOP"]
                .iter()
                .find(|o| eligible.iter().any(|e| e == *o))
                .map(|s| s.to_string())
                .unwrap_or("STOP".into())
        };
        let mut tx = self.begin();
        let assessment_id = format!("assess:{}", tx.next_seq());
        let assessment = Assessment {
            assessment_id: assessment_id.clone(),
            task_id: a.task_id,
            snapshot: tx.state.sequence,
            dependency_fingerprint: Self::fingerprint(&tx.state)?,
            input_manifest_digest: a.input_manifest_digest,
            question_template_digest: a.question_template_digest,
            eligible,
            choice: a.choice,
            probabilities_bp: a.probabilities_bp,
            confidence_bp: a.confidence_bp,
            model_requested: a.model_requested,
            model_returned: a.model_returned,
            routing_policy_version: a.routing_policy_version,
            provider_response_ref: a.provider_response_ref,
            latency_ms: a.latency_ms,
            usage: a.usage,
            mode: a.mode,
            used_advisor: used,
            applied_choice: applied.clone(),
            fallback_reason: fallback.clone(),
        };
        canonical_json(&assessment.usage)?;
        tx.emit(
            &sess.principal,
            EventBody::AssessmentRecorded { assessment },
        )?;
        self.commit(tx)?;
        Ok(
            json!({"assessment_id": assessment_id, "applied_choice": applied, "used_advisor": used, "fallback_reason": fallback}),
        )
    }

    fn task_transition(&mut self, a: TaskTransitionArgs) -> Result<Value> {
        let to = match a.to.as_str() {
            "STOPPED" => TaskStatus::Stopped,
            "ESCALATED" => TaskStatus::Escalated,
            _ => {
                return Err(CoreError::new(
                    "NOT_PERMITTED",
                    "the engine may only stop or escalate a task",
                ))
            }
        };
        let task = self
            .state
            .tasks
            .get(&a.task_id)
            .ok_or_else(|| CoreError::new("UNKNOWN_TASK", a.task_id.clone()))?;
        if task.status != TaskStatus::Open {
            return Ok(json!({"task_status": task.status}));
        }
        let mut tx = self.begin();
        tx.emit(
            "engine:scheduler",
            EventBody::TaskTransition {
                task_id: a.task_id.clone(),
                to,
                reason: a.reason.unwrap_or_default(),
            },
        )?;
        self.commit(tx)?;
        Ok(json!({"task_status": to}))
    }

    // ------------------------------------------------------------------
    // Operator control path
    // ------------------------------------------------------------------

    fn operator(&mut self, a: OperatorArgs) -> Result<Value> {
        let env = a.envelope;
        verify_operator(&env, &self.state.operator_keys)?;
        let mut body: serde_json::Map<String, Value> = serde_json::from_str(&env.body)
            .map_err(|e| CoreError::bad_args(format!("operator body: {e}")))?;
        let project = body
            .remove("project_id")
            .and_then(|v| v.as_str().map(String::from));
        let nonce = body
            .remove("nonce")
            .and_then(|v| v.as_str().map(String::from));
        let issued_at = body.remove("issued_at").and_then(|v| v.as_u64());
        let (Some(project), Some(nonce), Some(issued_at)) = (project, nonce, issued_at) else {
            return Err(CoreError::bad_args(
                "operator body requires project_id, nonce, issued_at",
            ));
        };
        if project != self.state.project_id {
            return Ok(rejected(vec![Reason::new("WRONG_PROJECT")]));
        }
        if self.state.nonces.contains(&nonce) || nonce.is_empty() {
            return Ok(rejected(vec![Reason::new("NONCE_REUSED")]));
        }
        if issued_at > self.now() + 300 {
            return Ok(rejected(vec![Reason::new("ISSUED_IN_FUTURE")]));
        }
        let op: OperatorOp = serde_json::from_value(Value::Object(body))
            .map_err(|e| CoreError::bad_args(format!("operator op: {e}")))?;
        let actor = format!("operator:{}", &env.key_id[..16]);
        let mut tx = self.begin();
        tx.emit(
            &actor,
            EventBody::OperatorCommandAccepted {
                nonce,
                key_id: env.key_id.clone(),
                body_digest: blob_digest(env.body.as_bytes()),
                op: op.name().into(),
            },
        )?;
        let result = match self.apply_operator(&mut tx, &actor, op) {
            Ok(v) => v,
            Err(reasons) => return Ok(rejected(reasons)), // nothing committed
        };
        let seq = tx.state.sequence;
        self.commit(tx)?;
        Ok(json!({"status": "APPLIED", "event_sequence": seq, "result": result}))
    }

    fn add_node(
        tx: &mut Tx,
        actor: &str,
        id: &str,
        sensitivity: Sensitivity,
        body: NodeBody,
    ) -> std::result::Result<Value, Vec<Reason>> {
        if !valid_ident(id) {
            return Err(vec![Reason::new("INVALID_ID").subject(id)]);
        }
        let entity = format!("{}:{}", body.prefix(), id);
        let revision = tx.state.latest.get(&entity).map_or(1, |r| r + 1);
        let node_ref = format!("{entity}@{revision}");
        let seq = tx.next_seq();
        let node = Node {
            node_ref: node_ref.clone(),
            entity_id: entity,
            revision,
            lifecycle: Lifecycle::Current,
            meta: meta(seq, actor, tx.now, sensitivity),
            body,
        };
        tx.emit(actor, EventBody::NodeAdded { node })
            .map_err(|e| vec![Reason::new(&e.code).subject(&e.message)])?;
        Ok(json!({"ref": node_ref}))
    }

    fn apply_operator(
        &self,
        tx: &mut Tx,
        actor: &str,
        op: OperatorOp,
    ) -> std::result::Result<Value, Vec<Reason>> {
        let internal = |e: CoreError| vec![Reason::new(&e.code).subject(&e.message)];
        let need = |tx: &Tx,
                    r: &str,
                    pred: fn(&NodeBody) -> bool|
         -> std::result::Result<(), Vec<Reason>> {
            match tx.state.nodes.get(r) {
                Some(n) if pred(&n.body) => Ok(()),
                _ => Err(vec![Reason::new("UNKNOWN_REFERENCE").missing(r)]),
            }
        };
        match op {
            OperatorOp::SetPolicy { policy } => {
                policy.validate().map_err(internal)?;
                let d = digest("policy", &policy).map_err(internal)?;
                tx.emit(
                    actor,
                    EventBody::PolicySet {
                        policy,
                        policy_digest: d.clone(),
                    },
                )
                .map_err(internal)?;
                Ok(json!({"policy_digest": d}))
            }
            OperatorOp::AddRequirement {
                requirement_id,
                text,
                sensitivity,
            } => Self::add_node(
                tx,
                actor,
                &requirement_id,
                sensitivity,
                NodeBody::Requirement { text },
            ),
            OperatorOp::AddAssumption {
                assumption_id,
                text,
                context,
                literal,
                expires_at,
                sensitivity,
            } => Self::add_node(
                tx,
                actor,
                &assumption_id,
                sensitivity,
                NodeBody::Assumption {
                    text,
                    context,
                    literal,
                    expires_at,
                },
            ),
            OperatorOp::AddEvidence {
                evidence_id,
                content,
                source_locator,
                collection_method,
                sensitivity,
            } => {
                let d = tx.put_blob(content.into_bytes());
                Self::add_node(
                    tx,
                    actor,
                    &evidence_id,
                    sensitivity,
                    NodeBody::Evidence {
                        original_content_digest: d,
                        source_locator,
                        collection_method,
                        collector: actor.to_string(),
                    },
                )
            }
            OperatorOp::AddClaim {
                claim_id,
                proposition,
                context,
                literal,
                basis,
                support_refs,
                sensitivity,
            } => {
                let b = match basis.as_str() {
                    "observed" => Basis::Observed,
                    "assumed" => Basis::Assumed,
                    "derived" => Basis::Derived,
                    _ => return Err(vec![Reason::new("SCHEMA_INVALID").subject("basis")]),
                };
                for r in &support_refs {
                    match b {
                        Basis::Observed => need(tx, r, |b| matches!(b, NodeBody::Evidence { .. }))?,
                        Basis::Assumed => {
                            need(tx, r, |b| matches!(b, NodeBody::Assumption { .. }))?
                        }
                        Basis::Derived => {
                            return Err(vec![Reason::new("SCHEMA_INVALID")
                                .subject("derived claims take no direct support")])
                        }
                    }
                }
                if matches!(b, Basis::Observed | Basis::Assumed) && support_refs.is_empty() {
                    return Err(vec![Reason::new("MISSING_SUPPORT").subject(&claim_id)]);
                }
                Self::add_node(
                    tx,
                    actor,
                    &claim_id,
                    sensitivity,
                    NodeBody::Claim {
                        proposition,
                        context,
                        literal,
                        basis: b,
                        support_refs,
                    },
                )
            }
            OperatorOp::AddRule {
                rule_id,
                body,
                head,
            } => {
                if body
                    .iter()
                    .chain(std::iter::once(&head))
                    .any(|l| !valid_ident(&l.atom))
                {
                    return Err(vec![Reason::new("SCHEMA_INVALID").subject("atom")]);
                }
                Self::add_node(
                    tx,
                    actor,
                    &rule_id,
                    Sensitivity::Internal,
                    NodeBody::Rule { body, head },
                )
            }
            OperatorOp::AddDerivation {
                derivation_id,
                conclusion,
                premises,
                rule_id,
                context,
            } => {
                need(tx, &conclusion, |b| matches!(b, NodeBody::Claim { .. }))?;
                for p in &premises {
                    need(tx, p, |b| matches!(b, NodeBody::Claim { .. }))?;
                }
                need(tx, &rule_id, |b| matches!(b, NodeBody::Rule { .. }))?;
                Self::add_node(
                    tx,
                    actor,
                    &derivation_id,
                    Sensitivity::Internal,
                    NodeBody::Derivation {
                        conclusion,
                        premises,
                        rule: rule_id,
                        context,
                    },
                )
            }
            OperatorOp::RegisterTestManifest {
                manifest_id,
                cases,
                entrypoint,
            } => {
                let names: BTreeSet<&str> = cases.iter().map(|c| c.name.as_str()).collect();
                if !valid_ident(&manifest_id) || names.len() != cases.len() || cases.is_empty() {
                    return Err(vec![Reason::new("SCHEMA_INVALID").subject("test manifest")]);
                }
                if let Some(e) = &entrypoint {
                    let ident = !e.function.is_empty()
                        && e.function.len() <= 128
                        && !e.function.starts_with(|c: char| c.is_ascii_digit())
                        && e.function
                            .chars()
                            .all(|c| c.is_ascii_alphanumeric() || c == '_');
                    if e.language != "python" || !ident || !e.path.ends_with(".py") {
                        return Err(vec![Reason::new("SCHEMA_INVALID").subject("entrypoint")]);
                    }
                    validate_path(&e.path).map_err(|r| vec![r])?;
                }
                let d = digest(
                    "test_manifest",
                    &json!({"id": manifest_id, "cases": cases, "entrypoint": entrypoint}),
                )
                .map_err(internal)?;
                tx.emit(
                    actor,
                    EventBody::TestManifestRegistered {
                        manifest: TestManifest {
                            manifest_id,
                            digest: d.clone(),
                            cases,
                            entrypoint,
                        },
                    },
                )
                .map_err(internal)?;
                Ok(json!({"digest": d}))
            }
            OperatorOp::RegisterEnvironment {
                environment_id,
                description,
                worker_kind,
            } => {
                if !valid_ident(&environment_id) {
                    return Err(vec![Reason::new("INVALID_ID")]);
                }
                let d = digest("environment", &json!({"id": environment_id, "description": description, "worker_kind": worker_kind})).map_err(internal)?;
                tx.emit(
                    actor,
                    EventBody::EnvironmentRegistered {
                        environment: Environment {
                            environment_id,
                            digest: d.clone(),
                            description,
                            worker_kind,
                        },
                    },
                )
                .map_err(internal)?;
                Ok(json!({"digest": d}))
            }
            OperatorOp::OpenTask {
                task_id,
                title,
                requirement_refs,
                test_manifest,
                environment,
                contexts,
                max_repairs,
            } => {
                if !valid_ident(&task_id) || tx.state.tasks.contains_key(&task_id) {
                    return Err(vec![Reason::new("INVALID_ID").subject(&task_id)]);
                }
                for r in &requirement_refs {
                    need(tx, r, |b| matches!(b, NodeBody::Requirement { .. }))?;
                }
                if let Some(m) = &test_manifest {
                    if !tx.state.test_manifests.contains_key(m) {
                        return Err(vec![Reason::new("UNKNOWN_REFERENCE").missing(m)]);
                    }
                }
                if let Some(e) = &environment {
                    if !tx.state.environments.contains_key(e) {
                        return Err(vec![Reason::new("UNKNOWN_REFERENCE").missing(e)]);
                    }
                }
                let max =
                    max_repairs.unwrap_or(tx.state.policy.as_ref().map_or(2, |p| p.max_repairs));
                let task = Task {
                    task_id: task_id.clone(),
                    title,
                    requirement_refs,
                    test_manifest,
                    environment,
                    contexts,
                    max_repairs: max,
                    status: TaskStatus::Open,
                    candidates: vec![],
                    failed_candidates: 0,
                    repairs_used: 0,
                    completed_by: None,
                };
                tx.emit(actor, EventBody::TaskOpened { task })
                    .map_err(internal)?;
                Ok(json!({"task_id": task_id}))
            }
            OperatorOp::Approve {
                action_digest,
                tool_id,
                expires_at,
            } => {
                if !tx
                    .state
                    .policy
                    .as_ref()
                    .is_some_and(|p| p.tools.contains_key(&tool_id))
                {
                    return Err(vec![Reason::new("UNKNOWN_TOOL").subject(&tool_id)]);
                }
                let approval_id = format!("approval:{}", tx.next_seq());
                let key_id = actor.to_string();
                let approval = Approval {
                    approval_id: approval_id.clone(),
                    action_digest,
                    tool_id,
                    expires_at,
                    policy_digest: tx.state.policy_digest.clone(),
                    key_id,
                    granted_sequence: tx.next_seq(),
                    revoked: false,
                    consumed_by: None,
                };
                tx.emit(actor, EventBody::ApprovalGranted { approval })
                    .map_err(internal)?;
                Ok(json!({"approval_id": approval_id}))
            }
            OperatorOp::Revoke { target, reason } => {
                let (stale_derivations, stale_claims) = if tx.state.nodes.contains_key(&target) {
                    akg::invalidation_closure(&tx.state, &target)
                } else if tx.state.approvals.contains_key(&target) {
                    (vec![], vec![])
                } else {
                    return Err(vec![Reason::new("UNKNOWN_REFERENCE").missing(&target)]);
                };
                tx.emit(
                    actor,
                    EventBody::Revoked {
                        target: target.clone(),
                        reason,
                        stale_derivations: stale_derivations.clone(),
                        stale_claims: stale_claims.clone(),
                    },
                )
                .map_err(internal)?;
                Ok(
                    json!({"revoked": target, "stale_derivations": stale_derivations, "stale_claims": stale_claims}),
                )
            }
            OperatorOp::CancelTask { task_id } => {
                match tx.state.tasks.get(&task_id) {
                    Some(t) if t.status == TaskStatus::Open => {}
                    _ => return Err(vec![Reason::new("TASK_NOT_OPEN").subject(&task_id)]),
                }
                tx.emit(
                    actor,
                    EventBody::TaskTransition {
                        task_id: task_id.clone(),
                        to: TaskStatus::Cancelled,
                        reason: "operator cancellation".into(),
                    },
                )
                .map_err(internal)?;
                Ok(json!({"task_id": task_id, "status": "CANCELLED"}))
            }
            OperatorOp::Shutdown {} => {
                tx.emit(actor, EventBody::ShutdownRequested {})
                    .map_err(internal)?;
                Ok(json!({"shutdown": true}))
            }
        }
    }

    // ------------------------------------------------------------------
    // Reporting and replay
    // ------------------------------------------------------------------

    fn task_report(&self, a: TaskArgs) -> Result<Value> {
        let s = &self.state;
        let task = s
            .tasks
            .get(&a.task_id)
            .ok_or_else(|| CoreError::new("UNKNOWN_TASK", a.task_id.clone()))?;
        let actions: Vec<&Action> = s
            .actions
            .values()
            .filter(|x| x.task_id == task.task_id)
            .collect();
        let used: BTreeSet<&String> = actions
            .iter()
            .flat_map(|x| x.verification_ids.iter())
            .collect();
        let verifications: Vec<Value> = s
            .verifications
            .values()
            .filter(|v| used.contains(&v.verification_id) || v.subject.proposal_id.as_ref().is_some_and(|p| task.candidates.contains(p)))
            .map(|v| json!({
                "verification_id": v.verification_id, "check_kind": v.check_kind, "result": v.result,
                "issuer": v.issuer, "implementation_digest": v.implementation_digest,
                "subject": v.subject, "test_manifest_digest": v.test_manifest_digest,
                "environment_digest": v.environment_digest, "validation_snapshot": v.validation_snapshot,
                "dependency_fingerprint": v.dependency_fingerprint, "failed_cases": v.failed_cases,
                "applicability_limits": v.applicability_limits, "used_for_authorization": used.contains(&v.verification_id),
            }))
            .collect();
        let approvals: Vec<&Approval> = s
            .approvals
            .values()
            .filter(|ap| {
                actions
                    .iter()
                    .any(|x| x.approval_ids.contains(&ap.approval_id))
            })
            .collect();
        let assessments: Vec<&Assessment> = s
            .assessments
            .values()
            .filter(|x| x.task_id == task.task_id)
            .collect();
        let simulated = verifications.iter().any(|v| {
            v["applicability_limits"].as_array().is_some_and(|l| {
                l.iter()
                    .any(|x| x.as_str().is_some_and(|s| s.starts_with("SIMULATED")))
            })
        });
        let mut limitations = vec![
            "Test results are evidence about the exact recorded runs, not proof of general correctness.".to_string(),
            "No claim of absence of security vulnerabilities; no production deployment was performed.".to_string(),
            "Formal checks cover only the bounded ground Horn fragment of the named contexts.".to_string(),
        ];
        if simulated {
            limitations.push("Acceptance checks used the FAKE worker: generated code was never executed (M4 sandbox not enabled).".into());
        }
        let rejections: Vec<Value> = s
            .admission_rejections
            .iter()
            .filter(|(k, _)| {
                task.candidates
                    .iter()
                    .chain(actions.iter().map(|a| &a.proposal_id))
                    .any(|p| k.starts_with(&format!("{p}@")))
                    || s.proposals
                        .get(k.split('@').next().unwrap_or(""))
                        .is_some_and(|p| p.task_id == task.task_id)
            })
            .map(|(k, v)| json!({"proposal": k, "reasons": v}))
            .collect();
        Ok(json!({
            "task": task,
            "approved_root": s.approved_root,
            "root_history": s.root_history,
            "actions": actions,
            "verifications": verifications,
            "approvals": approvals,
            "assessments": assessments,
            "admission_rejections": rejections,
            "limitations": limitations,
            "snapshot_sequence": s.sequence,
            "state_digest": self.digest,
        }))
    }

    fn list_tasks(&self) -> Result<Value> {
        let s = &self.state;
        let tasks: Vec<Value> = s
            .tasks
            .values()
            .map(|t| {
                let requirement = t
                    .requirement_refs
                    .first()
                    .and_then(|r| s.nodes.get(r))
                    .and_then(|n| match &n.body {
                        NodeBody::Requirement { text } => Some(text.clone()),
                        _ => None,
                    });
                let manifest = t.test_manifest.as_ref().and_then(|m| s.test_manifests.get(m));
                let candidates: Vec<Value> = t
                    .candidates
                    .iter()
                    .filter_map(|pid| s.proposals.get(pid))
                    .map(|p| {
                        let root = match &p.body {
                            ProposalBody::Candidate { candidate_root, .. } => candidate_root.clone(),
                            _ => String::new(),
                        };
                        json!({"proposal_id": p.proposal_id, "candidate_root": root, "acceptance": p.acceptance})
                    })
                    .collect();
                json!({
                    "task_id": t.task_id, "title": t.title, "status": t.status,
                    "requirement_refs": t.requirement_refs, "requirement": requirement,
                    "test_manifest": t.test_manifest, "environment": t.environment,
                    "entrypoint": manifest.and_then(|m| m.entrypoint.clone()),
                    "cases": manifest.map(|m| m.cases.clone()),
                    "contexts": t.contexts, "max_repairs": t.max_repairs,
                    "repairs_used": t.repairs_used, "failed_candidates": t.failed_candidates,
                    "candidates": candidates, "completed_by": t.completed_by,
                })
            })
            .collect();
        Ok(json!({"tasks": tasks, "snapshot_sequence": s.sequence}))
    }

    fn read_tree(&self, a: ReadTreeArgs) -> Result<Value> {
        let root = a.root.unwrap_or_else(|| self.state.approved_root.clone());
        Ok(json!({"root": root, "files": self.materialize(&root)?}))
    }

    fn events_since(&self, a: EventsSinceArgs) -> Result<Value> {
        let limit = a.limit.unwrap_or(100).clamp(1, 500);
        let events: Vec<Value> = self
            .store
            .events_after(a.after, limit)?
            .iter()
            .map(|e| {
                json!({
                    "sequence": e.sequence, "type": e.event_type, "actor": e.authenticated_actor,
                    "recorded_at": e.recorded_at, "summary": summarize(&e.body),
                })
            })
            .collect();
        Ok(json!({"events": events, "head_sequence": self.state.sequence}))
    }

    fn status(&self) -> Result<Value> {
        let s = &self.state;
        let policy = s.policy()?;
        Ok(json!({
            "project_id": s.project_id,
            "head_sequence": s.sequence,
            "state_digest": self.digest,
            "approved_root": s.approved_root,
            "root_history": s.root_history,
            "reducer_version": REDUCER_VERSION,
            "schema_version": SCHEMA_VERSION,
            "policy_version": policy.version,
            "policy_digest": s.policy_digest,
            "policy": policy,
            "deductor": {"kind": self.deductor.kind(), "implementation_digest": self.deductor.implementation_digest().unwrap_or_else(|e| format!("unavailable: {}", e.code))},
            "shutdown": s.shutdown,
            "tasks": s.tasks.len(),
            "environments": s.environments.values().map(|e| json!({"id": e.environment_id, "digest": e.digest, "worker_kind": e.worker_kind, "description": e.description})).collect::<Vec<_>>(),
            "test_manifests": s.test_manifests.keys().collect::<Vec<_>>(),
        }))
    }

    pub fn replay_verify(&self) -> Result<Value> {
        let (n, d) = replay_store(&self.store)?;
        Ok(json!({"events": n, "state_digest": d, "matches": d == self.digest}))
    }
}

/// Fold every retained event from genesis, comparing each snapshot digest.
/// Never re-runs models or tools; halts on any unsupported version or
/// integrity failure rather than inventing state.
pub fn replay_store(store: &Store) -> Result<(u64, String)> {
    let events = store.events()?;
    let mut state = State::empty(&store.project_id);
    let mut d = state.digest()?;
    for e in &events {
        state = reduce_with_digest(&state, &d, e)?;
        d = state.digest()?;
        let recorded = store.snapshot_digest(e.sequence)?;
        if recorded != d {
            return Err(CoreError::new(
                "INTEGRITY",
                format!("replayed digest differs at sequence {}", e.sequence),
            ));
        }
    }
    if let Some((seq, head)) = store.head()? {
        if seq != state.sequence || head != d {
            return Err(CoreError::new(
                "INTEGRITY",
                "head does not match replayed state",
            ));
        }
    }
    Ok((events.len() as u64, d))
}

/// One-line, non-sensitive description of an event for the UI event log.
fn summarize(body: &EventBody) -> String {
    match body {
        EventBody::Genesis { approved_root, .. } => {
            format!("project created, approved root {}", short(approved_root))
        }
        EventBody::OperatorCommandAccepted { op, .. } => format!("operator command accepted: {op}"),
        EventBody::PolicySet { policy, .. } => format!("policy set to {}", policy.version),
        EventBody::TreeRegistered { root, manifest } => {
            format!("tree {} registered ({} files)", short(root), manifest.len())
        }
        EventBody::SessionOpened { session } => format!(
            "session {} opened for {}",
            session.session_id, session.principal
        ),
        EventBody::InputRecorded { input } => format!(
            "untrusted input {} recorded from {} ({} bytes{})",
            input.input_id,
            input.principal,
            input.byte_len,
            input
                .provider
                .as_ref()
                .map(|p| format!(", {}", p.model_returned))
                .unwrap_or_default()
        ),
        EventBody::InputRejected { input_id, reasons } => {
            format!("input {input_id} rejected: {}", codes(reasons))
        }
        EventBody::ProposalRecorded { proposal } => format!(
            "{} proposal {} recorded for {}",
            proposal.body.kind(),
            proposal.proposal_id,
            proposal.task_id
        ),
        EventBody::NodeAdded { node } => format!("knowledge record {} added", node.node_ref),
        EventBody::Revoked {
            target,
            stale_derivations,
            ..
        } => format!(
            "{target} revoked ({} derivations made stale)",
            stale_derivations.len()
        ),
        EventBody::TestManifestRegistered { manifest } => format!(
            "acceptance manifest {} registered ({} cases)",
            manifest.manifest_id,
            manifest.cases.len()
        ),
        EventBody::EnvironmentRegistered { environment } => format!(
            "environment {} registered ({})",
            environment.environment_id, environment.worker_kind
        ),
        EventBody::TaskOpened { task } => format!("task {} opened", task.task_id),
        EventBody::TaskTransition { task_id, to, .. } => format!("task {task_id} -> {to:?}"),
        EventBody::CheckRequested { check } => {
            format!("{} check {} requested", check.check_kind, check.check_id)
        }
        EventBody::CheckReportRejected { check_id, reasons } => {
            format!("report for {check_id} rejected: {}", codes(reasons))
        }
        EventBody::VerificationIssued { verification: v } => format!(
            "{} {} -> {:?} (issuer {})",
            v.check_kind, v.verification_id, v.result, v.issuer
        ),
        EventBody::ContextEvaluated { evaluation, .. } => format!(
            "formal context {} evaluated: {}",
            evaluation.context_id,
            if evaluation.consistent {
                "consistent"
            } else {
                "NOT consistent"
            }
        ),
        EventBody::ApprovalGranted { approval } => format!(
            "approval {} granted for {} {}",
            approval.approval_id,
            approval.tool_id,
            short(&approval.action_digest)
        ),
        EventBody::AdmissionRejected {
            proposal_id,
            reasons,
            ..
        } => format!("admission of {proposal_id} rejected: {}", codes(reasons)),
        EventBody::ApplicabilityRecorded { record } => format!(
            "{} applicable at snapshot {}",
            record.verification_id, record.current_snapshot
        ),
        EventBody::IntentAuthorized { action } => format!(
            "action {} ({}) authorized",
            action.action_id, action.tool_id
        ),
        EventBody::DispatchStarted {
            action_id,
            attempt_id,
        } => format!("dispatch {attempt_id} of {action_id} started"),
        EventBody::DispatchStopped {
            action_id,
            state,
            reasons,
        } => format!("{action_id} stopped as {state:?}: {}", codes(reasons)),
        EventBody::ApprovedRootAdvanced { from, to, .. } => {
            format!("approved root {} -> {}", short(from), short(to))
        }
        EventBody::OutcomeObserved {
            action_id, outcome, ..
        } => format!(
            "{action_id} outcome {} (via {})",
            outcome.outcome, outcome.via
        ),
        EventBody::OutcomeUnknown { action_id, .. } => {
            format!("{action_id} outcome UNKNOWN (needs reconciliation)")
        }
        EventBody::PostconditionsEvaluated {
            action_id, result, ..
        } => format!("{action_id} postconditions {result:?}"),
        EventBody::ReconciliationRecorded {
            action_id,
            new_state,
            ..
        } => format!("{action_id} reconciled -> {new_state:?}"),
        EventBody::AssessmentRecorded { assessment: a } => format!(
            "routing assessment {}: advisor chose {:?}, applied {}{}",
            a.assessment_id,
            a.choice,
            a.applied_choice,
            a.fallback_reason
                .as_ref()
                .map(|f| format!(" (fallback: {f})"))
                .unwrap_or_default()
        ),
        EventBody::ShutdownRequested {} => "operator shutdown".into(),
    }
}

fn short(d: &str) -> String {
    d.strip_prefix("sha256:")
        .map(|h| format!("sha256:{}", &h[..h.len().min(12)]))
        .unwrap_or_else(|| d.to_string())
}

fn codes(reasons: &[Reason]) -> String {
    reasons
        .iter()
        .map(|r| r.code.as_str())
        .collect::<Vec<_>>()
        .join(", ")
}

pub fn action_digest(
    s: &State,
    task_id: &str,
    tool_id: &str,
    arguments: &Value,
    premise_refs: &[String],
    post: &[String],
) -> Result<String> {
    digest(
        "action",
        &json!({
            "project_id": s.project_id, "task_id": task_id, "tool_id": tool_id, "arguments": arguments,
            "premise_refs": premise_refs, "expected_postconditions": post,
            // Binds the approved-root transition, not just the target.
            "base_root": s.approved_root,
        }),
    )
}

fn meta(seq: u64, actor: &str, now: u64, sensitivity: Sensitivity) -> RecordMeta {
    RecordMeta {
        schema_version: SCHEMA_VERSION,
        created_event_sequence: seq,
        created_by: actor.to_string(),
        recorded_at: now,
        sensitivity,
    }
}

pub fn validate_path(p: &str) -> std::result::Result<(), Reason> {
    let bad = p.is_empty()
        || p.len() > 512
        || p.starts_with('/')
        || p.contains('\\')
        || p.contains('\0')
        || p.split('/')
            .any(|seg| seg.is_empty() || seg == "." || seg == "..");
    if bad {
        Err(Reason::new("INVALID_PATH").subject(p))
    } else {
        Ok(())
    }
}

/// Extract exactly one fenced ```json block, or the whole text.
fn extract_envelope(raw: &str) -> std::result::Result<&str, &'static str> {
    let fence = "```json";
    let blocks: Vec<usize> = raw.match_indices(fence).map(|(i, _)| i).collect();
    match blocks.len() {
        0 => Ok(raw.trim()),
        1 => {
            let start = blocks[0] + fence.len();
            let rest = &raw[start..];
            let end = rest.find("```").ok_or("UNTERMINATED_ENVELOPE")?;
            Ok(rest[..end].trim())
        }
        _ => Err("AMBIGUOUS_ENVELOPE"),
    }
}

// ---------------------------------------------------------------------------
// Strict argument and envelope schemas
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct OpenSession {
    role: String,
    label: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ViewArgs {
    session_id: String,
    task_id: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct QueryArgs {
    session_id: String,
    #[serde(rename = "ref")]
    node_ref: String,
    #[serde(default)]
    depth: usize,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SubmitArgs {
    session_id: String,
    raw: String,
    #[serde(default)]
    provider_record: Option<ProviderArgs>,
}

/// Provenance of a model response, supplied by the (trusted) provider adapter.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ProviderArgs {
    provider: String,
    model_requested: String,
    model_returned: String,
    #[serde(default)]
    response_id: String,
    /// Full provider response JSON; stored as a protected blob.
    #[serde(default)]
    raw_response: Option<String>,
    #[serde(default)]
    usage: Value,
    #[serde(default)]
    latency_ms: u64,
    #[serde(default)]
    attempts: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RecordInputArgs {
    session_id: String,
    raw: String,
    #[serde(default)]
    provider_record: Option<ProviderArgs>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ReadTreeArgs {
    #[serde(default)]
    root: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct EventsSinceArgs {
    #[serde(default)]
    after: u64,
    #[serde(default)]
    limit: Option<u64>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ProposalArgs {
    proposal_id: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ActionArgs {
    action_id: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TaskArgs {
    task_id: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ContextArgs {
    context_id: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CaseReport {
    name: String,
    status: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ReportCheck {
    session_id: String,
    check_id: String,
    result: String,
    implementation_digest: String,
    cases: Vec<CaseReport>,
    collected: u64,
    completed: bool,
    #[serde(default)]
    summary: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RecordOutcome {
    session_id: String,
    action_id: String,
    attempt_id: String,
    outcome: String,
    observation: Value,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RecordUnknown {
    session_id: String,
    action_id: String,
    attempt_id: String,
    error: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RecordReconciliation {
    session_id: String,
    action_id: String,
    finding: Value,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RouteArgs {
    task_id: String,
    #[serde(default)]
    failure_codes: Vec<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AssessmentArgs {
    session_id: String,
    task_id: String,
    #[serde(default)]
    failure_codes: Vec<String>,
    eligible: Vec<String>,
    choice: Option<String>,
    #[serde(default)]
    probabilities_bp: BTreeMap<String, u64>,
    confidence_bp: Option<u64>,
    model_requested: String,
    model_returned: String,
    mode: String,
    latency_ms: u64,
    #[serde(default)]
    usage: Value,
    question_template_digest: String,
    input_manifest_digest: String,
    routing_policy_version: String,
    provider_response_ref: String,
    fallback_reason: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TaskTransitionArgs {
    task_id: String,
    to: String,
    reason: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct OperatorArgs {
    envelope: OperatorEnvelope,
}

#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum Envelope {
    Plan(PlanEnv),
    Candidate(CandidateEnv),
    Claim(ClaimEnv),
    Action(ActionEnv),
}

impl Envelope {
    fn task_id(&self) -> &str {
        match self {
            Envelope::Plan(x) => &x.task_id,
            Envelope::Candidate(x) => &x.task_id,
            Envelope::Claim(x) => &x.task_id,
            Envelope::Action(x) => &x.task_id,
        }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PlanEnv {
    task_id: String,
    summary: String,
    steps: Vec<String>,
    #[serde(default)]
    premise_refs: Vec<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CandidateEnv {
    task_id: String,
    base_root: String,
    files: BTreeMap<String, String>,
    #[serde(default)]
    deletions: Vec<String>,
    #[serde(default)]
    rationale: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct DerivationSpec {
    premises: Vec<String>,
    rule_id: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ClaimEnv {
    task_id: String,
    claim_id: String,
    proposition: String,
    context: String,
    literal: Option<Literal>,
    derivation: Option<DerivationSpec>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ActionEnv {
    task_id: String,
    tool_id: String,
    arguments: Value,
    #[serde(default)]
    premise_refs: Vec<String>,
    idempotency_key: String,
    #[serde(default)]
    expected_postconditions: Vec<String>,
}

fn default_sensitivity() -> Sensitivity {
    Sensitivity::Internal
}

#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
enum OperatorOp {
    SetPolicy {
        policy: Policy,
    },
    AddRequirement {
        requirement_id: String,
        text: String,
        #[serde(default = "default_sensitivity")]
        sensitivity: Sensitivity,
    },
    AddAssumption {
        assumption_id: String,
        text: String,
        context: String,
        literal: Option<Literal>,
        expires_at: Option<u64>,
        #[serde(default = "default_sensitivity")]
        sensitivity: Sensitivity,
    },
    AddEvidence {
        evidence_id: String,
        content: String,
        source_locator: String,
        collection_method: String,
        #[serde(default = "default_sensitivity")]
        sensitivity: Sensitivity,
    },
    AddClaim {
        claim_id: String,
        proposition: String,
        context: String,
        literal: Option<Literal>,
        basis: String,
        #[serde(default)]
        support_refs: Vec<String>,
        #[serde(default = "default_sensitivity")]
        sensitivity: Sensitivity,
    },
    AddRule {
        rule_id: String,
        body: Vec<Literal>,
        head: Literal,
    },
    AddDerivation {
        derivation_id: String,
        conclusion: String,
        premises: Vec<String>,
        rule_id: String,
        context: String,
    },
    RegisterTestManifest {
        manifest_id: String,
        cases: Vec<TestCase>,
        #[serde(default)]
        entrypoint: Option<Entrypoint>,
    },
    RegisterEnvironment {
        environment_id: String,
        #[serde(default)]
        description: String,
        worker_kind: String,
    },
    OpenTask {
        task_id: String,
        #[serde(default)]
        title: String,
        requirement_refs: Vec<String>,
        #[serde(default)]
        test_manifest: Option<String>,
        #[serde(default)]
        environment: Option<String>,
        #[serde(default)]
        contexts: Vec<String>,
        max_repairs: Option<u64>,
    },
    Approve {
        action_digest: String,
        tool_id: String,
        expires_at: u64,
    },
    Revoke {
        #[serde(rename = "ref")]
        target: String,
        #[serde(default)]
        reason: String,
    },
    CancelTask {
        task_id: String,
    },
    Shutdown {},
}

impl OperatorOp {
    fn name(&self) -> &'static str {
        match self {
            OperatorOp::SetPolicy { .. } => "set_policy",
            OperatorOp::AddRequirement { .. } => "add_requirement",
            OperatorOp::AddAssumption { .. } => "add_assumption",
            OperatorOp::AddEvidence { .. } => "add_evidence",
            OperatorOp::AddClaim { .. } => "add_claim",
            OperatorOp::AddRule { .. } => "add_rule",
            OperatorOp::AddDerivation { .. } => "add_derivation",
            OperatorOp::RegisterTestManifest { .. } => "register_test_manifest",
            OperatorOp::RegisterEnvironment { .. } => "register_environment",
            OperatorOp::OpenTask { .. } => "open_task",
            OperatorOp::Approve { .. } => "approve",
            OperatorOp::Revoke { .. } => "revoke",
            OperatorOp::CancelTask { .. } => "cancel_task",
            OperatorOp::Shutdown {} => "shutdown",
        }
    }
}

#[allow(dead_code)]
fn _assert_epistemic_used(e: Epistemic) -> Epistemic {
    e
}
