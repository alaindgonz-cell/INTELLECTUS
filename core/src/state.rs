//! Authoritative state, events, and the pure reducer.
//!
//! `S[n+1] = reduce(S[n], E[n+1])`. The reducer performs no I/O, reads no
//! clock, generates no randomness and calls no model or tool: every such value
//! is already an input recorded in the event. Decisions are made by the
//! coordinator (the impure shell) and recorded as events; replay folds those
//! events and never re-decides or re-executes anything.

use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::canonical::digest;
use crate::error::{CoreError, Reason, Result};
use crate::policy::{Effect, Policy, Sensitivity};

pub const REDUCER_VERSION: u32 = 1;
pub const SCHEMA_VERSION: u32 = 1;

// ---------------------------------------------------------------------------
// Knowledge graph records
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Literal {
    pub atom: String,
    pub positive: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Lifecycle {
    Current,
    Stale,
    Superseded,
    Revoked,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Basis {
    /// Supported by recorded evidence.
    Observed,
    /// Explicitly stipulated premise.
    Assumed,
    /// Only supportable through a derivation.
    Derived,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RecordMeta {
    pub schema_version: u32,
    pub created_event_sequence: u64,
    pub created_by: String,
    pub recorded_at: u64,
    pub sensitivity: Sensitivity,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum NodeBody {
    Requirement {
        text: String,
    },
    Assumption {
        text: String,
        context: String,
        literal: Option<Literal>,
        expires_at: Option<u64>,
    },
    Evidence {
        original_content_digest: String,
        source_locator: String,
        collection_method: String,
        collector: String,
    },
    Claim {
        proposition: String,
        context: String,
        literal: Option<Literal>,
        basis: Basis,
        support_refs: Vec<String>,
    },
    Rule {
        body: Vec<Literal>,
        head: Literal,
    },
    Derivation {
        conclusion: String,
        premises: Vec<String>,
        rule: String,
        context: String,
    },
}

impl NodeBody {
    pub fn prefix(&self) -> &'static str {
        match self {
            NodeBody::Requirement { .. } => "req",
            NodeBody::Assumption { .. } => "assumption",
            NodeBody::Evidence { .. } => "evidence",
            NodeBody::Claim { .. } => "claim",
            NodeBody::Rule { .. } => "rule",
            NodeBody::Derivation { .. } => "derivation",
        }
    }

    /// Records this node depends on (forward edges).
    pub fn dependencies(&self) -> Vec<String> {
        match self {
            NodeBody::Claim { support_refs, .. } => support_refs.clone(),
            NodeBody::Derivation { premises, rule, .. } => {
                let mut v = premises.clone();
                v.push(rule.clone());
                v
            }
            _ => vec![],
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Node {
    #[serde(rename = "ref")]
    pub node_ref: String,
    pub entity_id: String,
    pub revision: u64,
    pub lifecycle: Lifecycle,
    pub meta: RecordMeta,
    pub body: NodeBody,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum DerivationStatus {
    Pending,
    Valid,
    Invalid,
    Stale,
    Indeterminate,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DerivationEval {
    pub status: DerivationStatus,
    pub evaluated_sequence: u64,
    pub reason: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ContextEval {
    pub context_id: String,
    pub evaluated_sequence: u64,
    pub dependency_fingerprint: String,
    pub premise_refs: Vec<String>,
    pub consistent: bool,
    pub conflicts: Vec<String>,
    pub verification_id: String,
}

// ---------------------------------------------------------------------------
// Sessions, inputs, proposals
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Session {
    pub session_id: String,
    pub role: String,
    pub principal: String,
    pub opened_sequence: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct InputRecord {
    pub input_id: String,
    pub session_id: String,
    pub principal: String,
    /// Untrusted raw bytes are stored as a protected blob, referenced here.
    pub blob: String,
    pub byte_len: u64,
    pub rejected: Option<Vec<Reason>>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ProposalBody {
    Plan {
        summary: String,
        steps: Vec<String>,
        premise_refs: Vec<String>,
    },
    Candidate {
        base_root: String,
        candidate_root: String,
        changed_paths: Vec<String>,
        deletions: Vec<String>,
        rationale: String,
        is_repair: bool,
    },
    Claim {
        claim_ref: String,
        derivation_ref: Option<String>,
    },
    Action {
        tool_id: String,
        arguments: Value,
        premise_refs: Vec<String>,
        expected_postconditions: Vec<String>,
        idempotency_key: String,
    },
}

impl ProposalBody {
    pub fn kind(&self) -> &'static str {
        match self {
            ProposalBody::Plan { .. } => "plan",
            ProposalBody::Candidate { .. } => "candidate",
            ProposalBody::Claim { .. } => "claim",
            ProposalBody::Action { .. } => "action",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Proposal {
    pub proposal_id: String,
    pub input_id: String,
    pub session_id: String,
    pub principal: String,
    pub task_id: String,
    pub payload_digest: String,
    pub recorded_sequence: u64,
    pub body: ProposalBody,
    /// Latest acceptance-test result for candidates (PASS/FAIL/...).
    pub acceptance: Option<CheckResult>,
}

// ---------------------------------------------------------------------------
// Tasks, checks, verification
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum TaskStatus {
    Open,
    Completed,
    Stopped,
    Escalated,
    Cancelled,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Task {
    pub task_id: String,
    pub requirement_refs: Vec<String>,
    pub test_manifest: String,
    pub environment: String,
    pub contexts: Vec<String>,
    pub max_repairs: u64,
    pub status: TaskStatus,
    pub candidates: Vec<String>,
    pub failed_candidates: u64,
    pub repairs_used: u64,
    pub completed_by: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TestCase {
    pub name: String,
    pub input: Value,
    pub expect: Value,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct TestManifest {
    pub manifest_id: String,
    pub digest: String,
    pub cases: Vec<TestCase>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Environment {
    pub environment_id: String,
    pub digest: String,
    pub description: String,
    pub worker_kind: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum CheckResult {
    Pass,
    Fail,
    Unknown,
    Error,
    Timeout,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Subject {
    pub proposal_id: Option<String>,
    pub base_root: Option<String>,
    pub candidate_root: Option<String>,
    pub payload_digest: Option<String>,
    pub context_id: Option<String>,
    pub premise_digest: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct CheckRequest {
    pub check_id: String,
    pub check_kind: String,
    pub subject: Subject,
    pub test_manifest_id: String,
    pub test_manifest_digest: String,
    pub environment_id: String,
    pub environment_digest: String,
    pub validation_snapshot: u64,
    pub dependency_fingerprint: String,
    pub policy_digest: String,
    pub reported: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Verification {
    pub verification_id: String,
    pub check_id: Option<String>,
    pub check_kind: String,
    pub issuer: String,
    pub implementation_digest: String,
    pub subject: Subject,
    pub test_manifest_digest: Option<String>,
    pub environment_digest: Option<String>,
    pub validation_snapshot: u64,
    pub dependency_fingerprint: String,
    pub policy_digest: String,
    pub result: CheckResult,
    pub failed_cases: Vec<String>,
    pub applicability_limits: Vec<String>,
    pub public_summary: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ApplicabilityRecord {
    pub verification_id: String,
    pub current_snapshot: u64,
    pub current_dependency_fingerprint: String,
    pub current_policy_digest: String,
    pub checker: String,
    pub applicable: bool,
}

// ---------------------------------------------------------------------------
// Authorization and effects
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Approval {
    pub approval_id: String,
    pub action_digest: String,
    pub tool_id: String,
    pub expires_at: u64,
    pub policy_digest: String,
    pub key_id: String,
    pub granted_sequence: u64,
    pub revoked: bool,
    pub consumed_by: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum DispatchState {
    AuthorizedIntent,
    DispatchStarted,
    OutcomeObserved,
    PostconditionsEvaluated,
    Cancelled,
    Expired,
    OutcomeUnknown,
    ReconciledNotApplied,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Attempt {
    pub attempt_id: String,
    pub started_sequence: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Outcome {
    pub outcome: String,
    pub observation_blob: String,
    pub via: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Action {
    pub action_id: String,
    pub proposal_id: String,
    pub task_id: String,
    pub requesting_principal: String,
    pub tool_id: String,
    pub effect: Effect,
    pub arguments: Value,
    pub premise_refs: Vec<String>,
    pub expected_postconditions: Vec<String>,
    pub idempotency_key: String,
    pub action_digest: String,
    pub base_snapshot_sequence: u64,
    pub base_root: String,
    pub candidate_root: Option<String>,
    pub dependency_fingerprint: String,
    pub policy_digest: String,
    pub approval_ids: Vec<String>,
    pub verification_ids: Vec<String>,
    pub reservation_scope: String,
    pub state: DispatchState,
    pub attempts: Vec<Attempt>,
    pub outcome: Option<Outcome>,
    pub postconditions: Option<CheckResult>,
    pub requires_human_review: bool,
    pub reasons: Vec<Reason>,
}

impl Action {
    pub fn holds_reservation(&self) -> bool {
        matches!(
            self.state,
            DispatchState::AuthorizedIntent
                | DispatchState::DispatchStarted
                | DispatchState::OutcomeUnknown
        )
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Assessment {
    pub assessment_id: String,
    pub task_id: String,
    pub snapshot: u64,
    pub dependency_fingerprint: String,
    pub input_manifest_digest: String,
    pub question_template_digest: String,
    pub eligible: Vec<String>,
    pub choice: Option<String>,
    pub probabilities_bp: BTreeMap<String, u64>,
    pub confidence_bp: Option<u64>,
    pub model_requested: String,
    pub model_returned: String,
    pub routing_policy_version: String,
    pub provider_response_ref: String,
    pub latency_ms: u64,
    pub usage: Value,
    pub mode: String,
    pub used_advisor: bool,
    pub applied_choice: String,
    pub fallback_reason: Option<String>,
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct State {
    pub project_id: String,
    pub sequence: u64,
    pub reducer_version: u32,
    pub operator_keys: BTreeSet<String>,
    pub policy: Option<Policy>,
    pub policy_digest: String,
    pub approved_root: String,
    pub root_history: Vec<(u64, String)>,
    pub trees: BTreeMap<String, BTreeMap<String, String>>,
    pub sessions: BTreeMap<String, Session>,
    pub inputs: BTreeMap<String, InputRecord>,
    pub proposals: BTreeMap<String, Proposal>,
    pub nodes: BTreeMap<String, Node>,
    pub latest: BTreeMap<String, u64>,
    /// Reverse dependency index: ref -> refs that depend on it.
    pub dependents: BTreeMap<String, BTreeSet<String>>,
    pub derivation_status: BTreeMap<String, DerivationEval>,
    pub contexts: BTreeMap<String, ContextEval>,
    pub test_manifests: BTreeMap<String, TestManifest>,
    pub environments: BTreeMap<String, Environment>,
    pub tasks: BTreeMap<String, Task>,
    pub checks: BTreeMap<String, CheckRequest>,
    pub verifications: BTreeMap<String, Verification>,
    pub applicability: Vec<ApplicabilityRecord>,
    pub approvals: BTreeMap<String, Approval>,
    pub actions: BTreeMap<String, Action>,
    pub idempotency: BTreeMap<String, String>,
    pub assessments: BTreeMap<String, Assessment>,
    pub admission_rejections: BTreeMap<String, Vec<Reason>>,
    pub nonces: BTreeSet<String>,
    pub shutdown: bool,
}

impl State {
    pub fn empty(project_id: &str) -> State {
        State {
            project_id: project_id.to_string(),
            sequence: 0,
            reducer_version: REDUCER_VERSION,
            operator_keys: BTreeSet::new(),
            policy: None,
            policy_digest: String::new(),
            approved_root: String::new(),
            root_history: vec![],
            trees: BTreeMap::new(),
            sessions: BTreeMap::new(),
            inputs: BTreeMap::new(),
            proposals: BTreeMap::new(),
            nodes: BTreeMap::new(),
            latest: BTreeMap::new(),
            dependents: BTreeMap::new(),
            derivation_status: BTreeMap::new(),
            contexts: BTreeMap::new(),
            test_manifests: BTreeMap::new(),
            environments: BTreeMap::new(),
            tasks: BTreeMap::new(),
            checks: BTreeMap::new(),
            verifications: BTreeMap::new(),
            applicability: vec![],
            approvals: BTreeMap::new(),
            actions: BTreeMap::new(),
            idempotency: BTreeMap::new(),
            assessments: BTreeMap::new(),
            admission_rejections: BTreeMap::new(),
            nonces: BTreeSet::new(),
            shutdown: false,
        }
    }

    pub fn digest(&self) -> Result<String> {
        digest("state", self)
    }

    pub fn policy(&self) -> Result<&Policy> {
        self.policy
            .as_ref()
            .ok_or_else(|| CoreError::new("NOT_INITIALIZED", "project has no genesis"))
    }

    /// Current (latest) revision ref of an entity id like `req:R1`.
    pub fn current_ref(&self, entity_id: &str) -> Option<String> {
        self.latest
            .get(entity_id)
            .map(|rev| format!("{entity_id}@{rev}"))
    }

    pub fn node(&self, r: &str) -> Option<&Node> {
        self.nodes.get(r)
    }
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum EventBody {
    Genesis {
        operator_keys: Vec<String>,
        policy: Policy,
        policy_digest: String,
        approved_root: String,
        manifest: BTreeMap<String, String>,
    },
    OperatorCommandAccepted {
        nonce: String,
        key_id: String,
        body_digest: String,
        op: String,
    },
    PolicySet {
        policy: Policy,
        policy_digest: String,
    },
    TreeRegistered {
        root: String,
        manifest: BTreeMap<String, String>,
    },
    SessionOpened {
        session: Session,
    },
    InputRecorded {
        input: InputRecord,
    },
    InputRejected {
        input_id: String,
        reasons: Vec<Reason>,
    },
    ProposalRecorded {
        proposal: Proposal,
    },
    NodeAdded {
        node: Node,
    },
    Revoked {
        target: String,
        reason: String,
        /// The invalidation boundary: every derivation this revocation made stale.
        stale_derivations: Vec<String>,
        stale_claims: Vec<String>,
    },
    TestManifestRegistered {
        manifest: TestManifest,
    },
    EnvironmentRegistered {
        environment: Environment,
    },
    TaskOpened {
        task: Task,
    },
    TaskTransition {
        task_id: String,
        to: TaskStatus,
        reason: String,
    },
    CheckRequested {
        check: CheckRequest,
    },
    CheckReportRejected {
        check_id: String,
        reasons: Vec<Reason>,
    },
    VerificationIssued {
        verification: Verification,
    },
    ContextEvaluated {
        evaluation: ContextEval,
        derivations: BTreeMap<String, DerivationEval>,
    },
    ApprovalGranted {
        approval: Approval,
    },
    AdmissionRejected {
        proposal_id: String,
        action_digest: Option<String>,
        reasons: Vec<Reason>,
    },
    ApplicabilityRecorded {
        record: ApplicabilityRecord,
    },
    IntentAuthorized {
        action: Action,
    },
    DispatchStarted {
        action_id: String,
        attempt_id: String,
    },
    DispatchStopped {
        action_id: String,
        state: DispatchState,
        reasons: Vec<Reason>,
    },
    ApprovedRootAdvanced {
        action_id: String,
        from: String,
        to: String,
    },
    OutcomeObserved {
        action_id: String,
        attempt_id: String,
        outcome: Outcome,
    },
    OutcomeUnknown {
        action_id: String,
        attempt_id: String,
        reason: String,
    },
    PostconditionsEvaluated {
        action_id: String,
        result: CheckResult,
        details: String,
    },
    ReconciliationRecorded {
        action_id: String,
        effect_observed: Option<bool>,
        details_blob: String,
        new_state: DispatchState,
        requires_human_review: bool,
    },
    AssessmentRecorded {
        assessment: Assessment,
    },
    ShutdownRequested {},
}

impl EventBody {
    pub fn event_type(&self) -> String {
        let v = serde_json::to_value(self).expect("event serializes");
        v.get("type")
            .and_then(|t| t.as_str())
            .unwrap_or("unknown")
            .to_string()
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Event {
    pub event_id: String,
    pub project_id: String,
    pub sequence: u64,
    pub schema_version: u32,
    pub reducer_version: u32,
    pub event_type: String,
    pub authenticated_actor: String,
    pub recorded_at: u64,
    pub prior_state_digest: String,
    pub body: EventBody,
}

// ---------------------------------------------------------------------------
// Reducer
// ---------------------------------------------------------------------------

fn integrity(msg: impl Into<String>) -> CoreError {
    CoreError::new("INTEGRITY", msg)
}

/// Pure transition. `prior_digest` must be the digest of `state` (callers
/// cache it; `reduce` recomputes it).
pub fn reduce_with_digest(state: &State, prior_digest: &str, event: &Event) -> Result<State> {
    if event.reducer_version != REDUCER_VERSION {
        return Err(CoreError::new(
            "UNSUPPORTED_REDUCER",
            format!(
                "event {} requires reducer version {}",
                event.sequence, event.reducer_version
            ),
        ));
    }
    if event.schema_version != SCHEMA_VERSION {
        return Err(CoreError::new(
            "UNSUPPORTED_SCHEMA",
            format!("schema {}", event.schema_version),
        ));
    }
    if event.sequence != state.sequence + 1 {
        return Err(integrity(format!(
            "sequence {} after {}",
            event.sequence, state.sequence
        )));
    }
    if event.project_id != state.project_id {
        return Err(integrity("project mismatch"));
    }
    if event.prior_state_digest != prior_digest {
        return Err(integrity(format!(
            "prior state digest mismatch at {}",
            event.sequence
        )));
    }
    if event.event_type != event.body.event_type() {
        return Err(integrity("event type mismatch"));
    }
    let mut s = state.clone();
    s.sequence = event.sequence;
    apply_v1(&mut s, event)?;
    Ok(s)
}

pub fn reduce(state: &State, event: &Event) -> Result<State> {
    reduce_with_digest(state, &state.digest()?, event)
}

fn require<'a, T>(m: &'a mut BTreeMap<String, T>, k: &str, what: &str) -> Result<&'a mut T> {
    m.get_mut(k)
        .ok_or_else(|| integrity(format!("unknown {what} {k}")))
}

fn apply_v1(s: &mut State, e: &Event) -> Result<()> {
    if s.policy.is_none() && !matches!(e.body, EventBody::Genesis { .. }) {
        return Err(integrity("first event must be genesis"));
    }
    match &e.body {
        EventBody::Genesis {
            operator_keys,
            policy,
            policy_digest,
            approved_root,
            manifest,
        } => {
            if s.policy.is_some() {
                return Err(integrity("duplicate genesis"));
            }
            s.operator_keys = operator_keys.iter().cloned().collect();
            s.policy = Some(policy.clone());
            s.policy_digest = policy_digest.clone();
            s.trees.insert(approved_root.clone(), manifest.clone());
            s.approved_root = approved_root.clone();
            s.root_history.push((e.sequence, approved_root.clone()));
        }
        EventBody::OperatorCommandAccepted { nonce, .. } => {
            if !s.nonces.insert(nonce.clone()) {
                return Err(integrity("operator nonce reused"));
            }
        }
        EventBody::PolicySet {
            policy,
            policy_digest,
        } => {
            s.policy = Some(policy.clone());
            s.policy_digest = policy_digest.clone();
        }
        EventBody::TreeRegistered { root, manifest } => {
            s.trees.insert(root.clone(), manifest.clone());
        }
        EventBody::SessionOpened { session } => {
            if s.sessions
                .insert(session.session_id.clone(), session.clone())
                .is_some()
            {
                return Err(integrity("duplicate session"));
            }
        }
        EventBody::InputRecorded { input } => {
            s.inputs.insert(input.input_id.clone(), input.clone());
        }
        EventBody::InputRejected { input_id, reasons } => {
            require(&mut s.inputs, input_id, "input")?.rejected = Some(reasons.clone());
        }
        EventBody::ProposalRecorded { proposal } => {
            if let ProposalBody::Candidate { is_repair, .. } = &proposal.body {
                let task = require(&mut s.tasks, &proposal.task_id, "task")?;
                task.candidates.push(proposal.proposal_id.clone());
                if *is_repair {
                    task.repairs_used += 1;
                }
            }
            if s.proposals
                .insert(proposal.proposal_id.clone(), proposal.clone())
                .is_some()
            {
                return Err(integrity("duplicate proposal"));
            }
        }
        EventBody::NodeAdded { node } => {
            for dep in node.body.dependencies() {
                if !s.nodes.contains_key(&dep) {
                    return Err(integrity(format!(
                        "node {} depends on unknown {dep}",
                        node.node_ref
                    )));
                }
                s.dependents
                    .entry(dep)
                    .or_default()
                    .insert(node.node_ref.clone());
            }
            if let NodeBody::Derivation { conclusion, .. } = &node.body {
                if !s.nodes.contains_key(conclusion) {
                    return Err(integrity("derivation conclusion unknown"));
                }
                // The conclusion's support depends on this derivation.
                s.dependents
                    .entry(node.node_ref.clone())
                    .or_default()
                    .insert(conclusion.clone());
                s.derivation_status.insert(
                    node.node_ref.clone(),
                    DerivationEval {
                        status: DerivationStatus::Pending,
                        evaluated_sequence: e.sequence,
                        reason: None,
                    },
                );
            }
            // A new revision supersedes the previous one.
            if let Some(prev) = s.latest.get(&node.entity_id).copied() {
                if node.revision != prev + 1 {
                    return Err(integrity("non-sequential revision"));
                }
                if let Some(old) = s.nodes.get_mut(&format!("{}@{prev}", node.entity_id)) {
                    if old.lifecycle == Lifecycle::Current {
                        old.lifecycle = Lifecycle::Superseded;
                    }
                }
            } else if node.revision != 1 {
                return Err(integrity("first revision must be 1"));
            }
            s.latest.insert(node.entity_id.clone(), node.revision);
            if s.nodes
                .insert(node.node_ref.clone(), node.clone())
                .is_some()
            {
                return Err(integrity("duplicate node ref"));
            }
        }
        EventBody::Revoked {
            target,
            stale_derivations,
            stale_claims,
            ..
        } => {
            if let Some(n) = s.nodes.get_mut(target) {
                n.lifecycle = Lifecycle::Revoked;
            } else if let Some(a) = s.approvals.get_mut(target) {
                a.revoked = true;
            } else {
                return Err(integrity(format!("revoke unknown {target}")));
            }
            for d in stale_derivations {
                let ev = require(&mut s.derivation_status, d, "derivation")?;
                ev.status = DerivationStatus::Stale;
                ev.evaluated_sequence = e.sequence;
                ev.reason = Some(format!("premise invalidated by revocation of {target}"));
            }
            for c in stale_claims {
                let n = require(&mut s.nodes, c, "claim")?;
                if n.lifecycle == Lifecycle::Current {
                    n.lifecycle = Lifecycle::Stale;
                }
            }
        }
        EventBody::TestManifestRegistered { manifest } => {
            s.test_manifests
                .insert(manifest.manifest_id.clone(), manifest.clone());
        }
        EventBody::EnvironmentRegistered { environment } => {
            s.environments
                .insert(environment.environment_id.clone(), environment.clone());
        }
        EventBody::TaskOpened { task } => {
            if s.tasks.insert(task.task_id.clone(), task.clone()).is_some() {
                return Err(integrity("duplicate task"));
            }
        }
        EventBody::TaskTransition { task_id, to, .. } => {
            let task = require(&mut s.tasks, task_id, "task")?;
            if task.status != TaskStatus::Open {
                return Err(integrity("task is not open"));
            }
            task.status = *to;
            if *to == TaskStatus::Cancelled {
                for a in s.actions.values_mut() {
                    if &a.task_id == task_id && a.state == DispatchState::AuthorizedIntent {
                        a.state = DispatchState::Cancelled;
                    }
                }
            }
        }
        EventBody::CheckRequested { check } => {
            s.checks.insert(check.check_id.clone(), check.clone());
        }
        EventBody::CheckReportRejected { check_id, .. } => {
            require(&mut s.checks, check_id, "check")?;
        }
        EventBody::VerificationIssued { verification } => {
            if let Some(cid) = &verification.check_id {
                let chk = require(&mut s.checks, cid, "check")?;
                if chk.reported {
                    return Err(integrity("check already reported"));
                }
                chk.reported = true;
            }
            if verification.check_kind == "acceptance_tests" {
                if let Some(pid) = &verification.subject.proposal_id {
                    let p = require(&mut s.proposals, pid, "proposal")?;
                    let first_failure =
                        p.acceptance.is_none() && verification.result != CheckResult::Pass;
                    p.acceptance = Some(verification.result);
                    let tid = p.task_id.clone();
                    if first_failure {
                        require(&mut s.tasks, &tid, "task")?.failed_candidates += 1;
                    }
                }
            }
            s.verifications
                .insert(verification.verification_id.clone(), verification.clone());
        }
        EventBody::ContextEvaluated {
            evaluation,
            derivations,
        } => {
            for (d, ev) in derivations {
                s.derivation_status.insert(d.clone(), ev.clone());
            }
            s.contexts
                .insert(evaluation.context_id.clone(), evaluation.clone());
        }
        EventBody::ApprovalGranted { approval } => {
            s.approvals
                .insert(approval.approval_id.clone(), approval.clone());
        }
        EventBody::AdmissionRejected {
            proposal_id,
            reasons,
            ..
        } => {
            s.admission_rejections
                .insert(format!("{proposal_id}@{}", e.sequence), reasons.clone());
        }
        EventBody::ApplicabilityRecorded { record } => {
            s.applicability.push(record.clone());
        }
        EventBody::IntentAuthorized { action } => {
            if s.idempotency.contains_key(&action.idempotency_key) {
                return Err(integrity("idempotency key already bound"));
            }
            for ap in &action.approval_ids {
                let a = require(&mut s.approvals, ap, "approval")?;
                if a.consumed_by.is_some() || a.revoked {
                    return Err(integrity("approval not usable"));
                }
                a.consumed_by = Some(action.action_id.clone());
            }
            s.idempotency
                .insert(action.idempotency_key.clone(), action.action_id.clone());
            s.actions.insert(action.action_id.clone(), action.clone());
        }
        EventBody::DispatchStarted {
            action_id,
            attempt_id,
        } => {
            let a = require(&mut s.actions, action_id, "action")?;
            if a.state != DispatchState::AuthorizedIntent {
                return Err(integrity("dispatch from ineligible state"));
            }
            a.state = DispatchState::DispatchStarted;
            a.attempts.push(Attempt {
                attempt_id: attempt_id.clone(),
                started_sequence: e.sequence,
            });
        }
        EventBody::DispatchStopped {
            action_id,
            state,
            reasons,
        } => {
            let a = require(&mut s.actions, action_id, "action")?;
            a.state = *state;
            a.reasons = reasons.clone();
        }
        EventBody::ApprovedRootAdvanced { from, to, .. } => {
            if &s.approved_root != from || !s.trees.contains_key(to) {
                return Err(integrity("approved root transition mismatch"));
            }
            s.approved_root = to.clone();
            s.root_history.push((e.sequence, to.clone()));
        }
        EventBody::OutcomeObserved {
            action_id, outcome, ..
        } => {
            let a = require(&mut s.actions, action_id, "action")?;
            a.state = DispatchState::OutcomeObserved;
            a.outcome = Some(outcome.clone());
            a.requires_human_review = false;
        }
        EventBody::OutcomeUnknown { action_id, .. } => {
            let a = require(&mut s.actions, action_id, "action")?;
            a.state = DispatchState::OutcomeUnknown;
            a.requires_human_review = true;
        }
        EventBody::PostconditionsEvaluated {
            action_id, result, ..
        } => {
            let a = require(&mut s.actions, action_id, "action")?;
            a.state = DispatchState::PostconditionsEvaluated;
            a.postconditions = Some(*result);
            if *result == CheckResult::Pass && a.effect == Effect::Internal {
                let tid = a.task_id.clone();
                let aid = a.action_id.clone();
                let task = require(&mut s.tasks, &tid, "task")?;
                if task.status == TaskStatus::Open && a_is_promotion(&s.actions[&aid]) {
                    task.status = TaskStatus::Completed;
                    task.completed_by = Some(aid);
                }
            }
        }
        EventBody::ReconciliationRecorded {
            action_id,
            new_state,
            requires_human_review,
            ..
        } => {
            let a = require(&mut s.actions, action_id, "action")?;
            a.state = *new_state;
            a.requires_human_review = *requires_human_review;
        }
        EventBody::AssessmentRecorded { assessment } => {
            s.assessments
                .insert(assessment.assessment_id.clone(), assessment.clone());
        }
        EventBody::ShutdownRequested {} => {
            s.shutdown = true;
            for a in s.actions.values_mut() {
                if a.state == DispatchState::AuthorizedIntent {
                    a.state = DispatchState::Cancelled;
                }
            }
        }
    }
    Ok(())
}

fn a_is_promotion(a: &Action) -> bool {
    a.tool_id == crate::coordinator::PROMOTE_TOOL
}
