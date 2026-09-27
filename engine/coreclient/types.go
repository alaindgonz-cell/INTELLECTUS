package coreclient

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ID is an identifier issued by the core (session_id, proposal_id,
// action_id, attempt_id, check_id, ...). PROTOCOL.md does not pin whether
// these are JSON strings or numbers, so ID stores the exact JSON token the
// core sent and echoes it back verbatim. It is comparable and usable as a
// map key. Construct engine-chosen ids with StringID.
type ID string

// StringID builds an ID holding a JSON string.
func StringID(s string) ID {
	b, _ := json.Marshal(s)
	return ID(b)
}

// MarshalJSON emits the stored token (null when empty).
func (i ID) MarshalJSON() ([]byte, error) {
	if i == "" {
		return []byte("null"), nil
	}
	if !json.Valid([]byte(i)) {
		return nil, fmt.Errorf("coreclient: ID %q is not a JSON token (use StringID)", string(i))
	}
	return []byte(i), nil
}

// UnmarshalJSON stores the raw token.
func (i *ID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		*i = ""
		return nil
	}
	*i = ID(append([]byte(nil), b...))
	return nil
}

// String returns the human form (unquoted when the token is a string).
func (i ID) String() string {
	var s string
	if json.Unmarshal([]byte(i), &s) == nil {
		return s
	}
	return string(i)
}

// IsZero reports whether no id was received.
func (i ID) IsZero() bool { return i == "" }

type rawSetter interface{ setRaw(json.RawMessage) }

// WithRaw keeps the exact result object the core returned (for traces).
type WithRaw struct {
	Raw json.RawMessage `json:"-"`
}

func (w *WithRaw) setRaw(b json.RawMessage) { w.Raw = b }

// Reason is the structured rejection reason of PROTOCOL.md §2.
type Reason struct {
	Code                    string           `json:"code"`
	FailedCheck             *string          `json:"failed_check,omitempty"`
	SubjectReference        *string          `json:"subject_reference,omitempty"`
	MissingDependency       json.RawMessage  `json:"missing_dependency,omitempty"`
	CounterexampleReference json.RawMessage  `json:"counterexample_reference,omitempty"`
	PermittedNextSteps      []string         `json:"permitted_next_steps,omitempty"`
	RemainingBudget         map[string]int64 `json:"remaining_budget,omitempty"`
	Raw                     json.RawMessage  `json:"-"`
}

// UnmarshalJSON accepts the structured object and, defensively, a bare
// code string.
func (r *Reason) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var code string
		if err := json.Unmarshal(b, &code); err != nil {
			return err
		}
		*r = Reason{Code: code, Raw: append([]byte(nil), b...)}
		return nil
	}
	type plain Reason
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = Reason(p)
	r.Raw = append([]byte(nil), b...)
	return nil
}

// Reasons is a list of structured reasons.
type Reasons []Reason

// Has reports whether any reason carries code.
func (rs Reasons) Has(code string) bool {
	for _, r := range rs {
		if r.Code == code {
			return true
		}
	}
	return false
}

// Codes returns the reason codes in order.
func (rs Reasons) Codes() []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Code)
	}
	return out
}

// Literal is an AKG literal as used in operator bodies and envelopes.
type Literal struct {
	Atom     string `json:"atom"`
	Positive bool   `json:"positive"`
}

// ---- results -------------------------------------------------------------

// HelloResult answers `hello`.
type HelloResult struct {
	WithRaw
	Protocol       int             `json:"protocol"`
	ProjectID      string          `json:"project_id"`
	HeadSequence   int64           `json:"head_sequence"`
	ReducerVersion json.RawMessage `json:"reducer_version"`
	Deductor       struct {
		Kind                 string `json:"kind"`
		ImplementationDigest string `json:"implementation_digest"`
	} `json:"deductor"`
}

// Session answers `open_session`.
type Session struct {
	WithRaw
	SessionID ID     `json:"session_id"`
	Principal string `json:"principal"`
}

// ViewCandidate is one candidate entry in a task view.
type ViewCandidate struct {
	ProposalID    ID     `json:"proposal_id"`
	CandidateRoot string `json:"candidate_root"`
	Acceptance    string `json:"acceptance"`
}

// ViewFailure is one entry of task.last_failures.
type ViewFailure struct {
	ProposalID  ID       `json:"proposal_id"`
	FailedCases []string `json:"failed_cases"`
}

// ViewTask is the task section of a view.
type ViewTask struct {
	TaskID          string          `json:"task_id"`
	Status          string          `json:"status"`
	RequirementRefs []string        `json:"requirement_refs"`
	RepairsUsed     int64           `json:"repairs_used"`
	MaxRepairs      int64           `json:"max_repairs"`
	Candidates      []ViewCandidate `json:"candidates"`
	LastFailures    []ViewFailure   `json:"last_failures"`
}

// ViewResult is the permission-filtered view of §4.1. Unknown fields are
// kept in Raw; model adapters are given Raw verbatim.
type ViewResult struct {
	WithRaw
	SnapshotSequence      int64     `json:"snapshot_sequence"`
	StateDigest           string    `json:"state_digest"`
	DependencyFingerprint string    `json:"dependency_fingerprint"`
	PolicyVersion         string    `json:"policy_version"`
	Task                  *ViewTask `json:"task"`
	Requirements          []struct {
		Ref  string `json:"ref"`
		Text string `json:"text"`
	} `json:"requirements"`
	ApprovedRoot *struct {
		Digest string            `json:"digest"`
		Files  map[string]string `json:"files"`
	} `json:"approved_root"`
	TestManifest json.RawMessage `json:"test_manifest"`
	Claims       json.RawMessage `json:"claims"`
}

// SubmitResult answers `submit`.
type SubmitResult struct {
	WithRaw
	Status        string  `json:"status"`
	InputID       ID      `json:"input_id"`
	ProposalID    ID      `json:"proposal_id"`
	Kind          string  `json:"kind"`
	PayloadDigest string  `json:"payload_digest"`
	CandidateRoot string  `json:"candidate_root"`
	Reasons       Reasons `json:"reasons"`
}

// TestCase is one manifest case. Input/Expect are kept raw (input may be null).
type TestCase struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Expect json.RawMessage `json:"expect"`
}

// TestManifest as carried in a CheckRequest.
type TestManifest struct {
	ID     string     `json:"id"`
	Digest string     `json:"digest"`
	Cases  []TestCase `json:"cases"`
}

// CheckSubject binds a check to one proposal payload.
type CheckSubject struct {
	ProposalID    ID     `json:"proposal_id"`
	BaseRoot      string `json:"base_root"`
	CandidateRoot string `json:"candidate_root"`
	PayloadDigest string `json:"payload_digest"`
}

// Environment is the registered execution environment of a check.
type Environment struct {
	ID         string `json:"id"`
	Digest     string `json:"digest"`
	WorkerKind string `json:"worker_kind"`
}

// CheckRequest is §4.3.
type CheckRequest struct {
	CheckID               ID           `json:"check_id"`
	CheckKind             string       `json:"check_kind"`
	Subject               CheckSubject `json:"subject"`
	TestManifest          TestManifest `json:"test_manifest"`
	Environment           Environment  `json:"environment"`
	ValidationSnapshot    int64        `json:"validation_snapshot"`
	DependencyFingerprint string       `json:"dependency_fingerprint"`
	PolicyVersion         string       `json:"policy_version"`
}

// CheckPlanResult answers `check_plan`.
type CheckPlanResult struct {
	WithRaw
	Checks       []CheckRequest    `json:"checks"`
	Materialized map[string]string `json:"materialized"`
}

// CaseResult is one per-case status reported by a runner.
type CaseResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ReportCheckArgs are the args of `report_check`.
type ReportCheckArgs struct {
	SessionID            ID           `json:"session_id"`
	CheckID              ID           `json:"check_id"`
	Result               string       `json:"result"`
	ImplementationDigest string       `json:"implementation_digest"`
	Cases                []CaseResult `json:"cases"`
	Collected            int          `json:"collected"`
	Completed            bool         `json:"completed"`
	Summary              string       `json:"summary"`
}

// ReportCheckResult answers `report_check` (the core may downgrade).
type ReportCheckResult struct {
	WithRaw
	// Status is "ISSUED", or "REJECTED" (e.g. UNTRUSTED_ISSUER) with Reasons.
	Status         string   `json:"status"`
	VerificationID ID       `json:"verification_id"`
	Result         string   `json:"result"`
	FailedCases    []string `json:"failed_cases"`
	Reasons        Reasons  `json:"reasons"`
}

// EvaluateContextResult answers `evaluate_context`.
type EvaluateContextResult struct {
	WithRaw
	VerificationID ID                `json:"verification_id"`
	Result         string            `json:"result"`
	Consistent     bool              `json:"consistent"`
	Conflicts      []json.RawMessage `json:"conflicts"`
	Derivations    []struct {
		DerivationID ID      `json:"derivation_id"`
		Status       string  `json:"status"`
		Reason       *string `json:"reason"`
	} `json:"derivations"`
}

// AdmitResult answers `admit`.
type AdmitResult struct {
	WithRaw
	Status        string  `json:"status"`
	ActionID      ID      `json:"action_id"`
	ActionDigest  string  `json:"action_digest"`
	DispatchState string  `json:"dispatch_state"`
	Reasons       Reasons `json:"reasons"`
}

// OperatorResult answers `operator`.
type OperatorResult struct {
	WithRaw
	Status        string          `json:"status"`
	EventSequence int64           `json:"event_sequence"`
	Result        json.RawMessage `json:"result"`
	Reasons       Reasons         `json:"reasons"`
}

// Ref returns result.ref when the applied operation reported one.
func (r *OperatorResult) Ref() string {
	var v struct {
		Ref string `json:"ref"`
	}
	if len(r.Result) == 0 || json.Unmarshal(r.Result, &v) != nil {
		return ""
	}
	return v.Ref
}

// DispatchBeginResult answers `dispatch_begin`.
type DispatchBeginResult struct {
	WithRaw
	Status         string          `json:"status"`
	AttemptID      ID              `json:"attempt_id"`
	DispatchState  string          `json:"dispatch_state"`
	ToolID         string          `json:"tool_id"`
	Arguments      json.RawMessage `json:"arguments"`
	IdempotencyKey string          `json:"idempotency_key"`
	Result         json.RawMessage `json:"result"`
	Reasons        Reasons         `json:"reasons"`
}

// RecordOutcomeResult answers `record_outcome` / `record_outcome_unknown`.
type RecordOutcomeResult struct {
	WithRaw
	Status         string `json:"status"`
	Postconditions string `json:"postconditions"`
	Details        string `json:"details"`
}

// PendingAction is one action awaiting reconciliation.
type PendingAction struct {
	ActionID       ID              `json:"action_id"`
	AttemptID      ID              `json:"attempt_id"`
	ToolID         string          `json:"tool_id"`
	Arguments      json.RawMessage `json:"arguments"`
	IdempotencyKey string          `json:"idempotency_key"`
	DispatchState  string          `json:"dispatch_state"`
}

// PendingReconciliationResult answers `pending_reconciliation`.
type PendingReconciliationResult struct {
	WithRaw
	Actions []PendingAction `json:"actions"`
}

// Finding is the result of an adapter's reconcile read. EffectObserved is
// tri-state: true, false, or nil (could not determine).
type Finding struct {
	EffectObserved *bool `json:"effect_observed"`
	Details        any   `json:"details"`
}

// RecordReconciliationResult answers `record_reconciliation`.
type RecordReconciliationResult struct {
	WithRaw
	DispatchState       string `json:"dispatch_state"`
	RequiresHumanReview bool   `json:"requires_human_review"`
}

// RouteResult answers `route`.
type RouteResult struct {
	WithRaw
	Eligible            []string `json:"eligible"`
	DeterministicChoice *string  `json:"deterministic_choice"`
	Reason              string   `json:"reason"`
}

// AssessmentArgs are the args of `record_assessment`. Probabilities and
// confidence are integer basis points (0..=10000); no floats anywhere.
// FailureCodes are the codes given to `route`, so the core can recompute
// the same eligible set when validating the assessment.
type AssessmentArgs struct {
	SessionID              ID               `json:"session_id"`
	TaskID                 string           `json:"task_id"`
	FailureCodes           []string         `json:"failure_codes"`
	Eligible               []string         `json:"eligible"`
	Choice                 *string          `json:"choice"`
	ProbabilitiesBP        map[string]int64 `json:"probabilities_bp"`
	ConfidenceBP           int64            `json:"confidence_bp"`
	ModelRequested         string           `json:"model_requested"`
	ModelReturned          string           `json:"model_returned"`
	Mode                   string           `json:"mode"`
	LatencyMS              int64            `json:"latency_ms"`
	Usage                  map[string]int64 `json:"usage"`
	QuestionTemplateDigest string           `json:"question_template_digest"`
	InputManifestDigest    string           `json:"input_manifest_digest"`
	RoutingPolicyVersion   string           `json:"routing_policy_version"`
	ProviderResponseRef    string           `json:"provider_response_ref"`
	FallbackReason         *string          `json:"fallback_reason"`
}

// RecordAssessmentResult answers `record_assessment`.
type RecordAssessmentResult struct {
	WithRaw
	AssessmentID  ID     `json:"assessment_id"`
	AppliedChoice string `json:"applied_choice"`
	UsedAdvisor   bool   `json:"used_advisor"`
}

// TaskTransitionResult answers `task_transition`.
type TaskTransitionResult struct {
	WithRaw
	TaskStatus string `json:"task_status"`
}

// ReplayVerifyResult answers `replay_verify`.
type ReplayVerifyResult struct {
	WithRaw
	Events      int64  `json:"events"`
	StateDigest string `json:"state_digest"`
	Matches     bool   `json:"matches"`
}
