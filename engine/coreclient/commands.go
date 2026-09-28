package coreclient

import (
	"context"
	"encoding/json"
	"fmt"
)

// Route options (PROTOCOL.md §4).
const (
	RouteGatherContext = "GATHER_CONTEXT"
	RouteRepair        = "REPAIR"
	RouteReplan        = "REPLAN"
	RouteEscalate      = "ESCALATE"
	RouteStop          = "STOP"
)

// RouteOptions lists every route option the protocol defines.
var RouteOptions = []string{RouteGatherContext, RouteRepair, RouteReplan, RouteEscalate, RouteStop}

// Roles (PROTOCOL.md §4).
const (
	RolePlanner = "planner"
	RoleCoder   = "coder"
	RoleTester  = "tester"
	RoleRunner  = "runner"
	RoleGateway = "gateway"
	RoleAdvisor = "advisor"
)

func call[T any, PT interface {
	*T
	rawSetter
}](ctx context.Context, c *Client, cmd string, args any) (*T, error) {
	raw, err := c.CallRaw(ctx, cmd, args)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("coreclient: decode %s result: %w", cmd, err)
	}
	PT(&v).setRaw(raw)
	return &v, nil
}

// Hello: `hello {}`.
func (c *Client) Hello(ctx context.Context) (*HelloResult, error) {
	return call[HelloResult](ctx, c, "hello", struct{}{})
}

// OpenSession: `open_session {role, label}`.
func (c *Client) OpenSession(ctx context.Context, role, label string) (*Session, error) {
	return call[Session](ctx, c, "open_session", map[string]string{"role": role, "label": label})
}

// View: `view {session_id, task_id}`.
func (c *Client) View(ctx context.Context, session ID, taskID string) (*ViewResult, error) {
	return call[ViewResult](ctx, c, "view", map[string]any{"session_id": session, "task_id": taskID})
}

// Submit passes raw model text to the core unmodified: `submit {session_id, raw}`.
func (c *Client) Submit(ctx context.Context, session ID, raw string) (*SubmitResult, error) {
	return call[SubmitResult](ctx, c, "submit", map[string]any{"session_id": session, "raw": raw})
}

// CheckPlan: `check_plan {proposal_id}`.
func (c *Client) CheckPlan(ctx context.Context, proposalID ID) (*CheckPlanResult, error) {
	return call[CheckPlanResult](ctx, c, "check_plan", map[string]any{"proposal_id": proposalID})
}

// ReportCheck: `report_check {...}`.
func (c *Client) ReportCheck(ctx context.Context, args ReportCheckArgs) (*ReportCheckResult, error) {
	if args.Cases == nil {
		args.Cases = []CaseResult{}
	}
	return call[ReportCheckResult](ctx, c, "report_check", args)
}

// EvaluateContext: `evaluate_context {context_id}`.
func (c *Client) EvaluateContext(ctx context.Context, contextID string) (*EvaluateContextResult, error) {
	return call[EvaluateContextResult](ctx, c, "evaluate_context", map[string]string{"context_id": contextID})
}

// Admit: `admit {proposal_id}`.
func (c *Client) Admit(ctx context.Context, proposalID ID) (*AdmitResult, error) {
	return call[AdmitResult](ctx, c, "admit", map[string]any{"proposal_id": proposalID})
}

// Operator: `operator {envelope}`.
func (c *Client) Operator(ctx context.Context, env Envelope) (*OperatorResult, error) {
	return call[OperatorResult](ctx, c, "operator", map[string]any{"envelope": env})
}

// OperatorOp signs op with o and sends it.
func (c *Client) OperatorOp(ctx context.Context, o *Operator, op string, params map[string]any) (*OperatorResult, error) {
	env, _, err := o.Envelope(op, params)
	if err != nil {
		return nil, err
	}
	return c.Operator(ctx, env)
}

// DispatchBegin: `dispatch_begin {action_id}`.
func (c *Client) DispatchBegin(ctx context.Context, actionID ID) (*DispatchBeginResult, error) {
	return call[DispatchBeginResult](ctx, c, "dispatch_begin", map[string]any{"action_id": actionID})
}

// RecordOutcome: `record_outcome {...}`. outcome is SUCCEEDED or FAILED.
func (c *Client) RecordOutcome(ctx context.Context, session, actionID, attemptID ID, outcome string, observation any) (*RecordOutcomeResult, error) {
	if observation == nil {
		observation = map[string]any{}
	}
	return call[RecordOutcomeResult](ctx, c, "record_outcome", map[string]any{
		"session_id": session, "action_id": actionID, "attempt_id": attemptID,
		"outcome": outcome, "observation": observation,
	})
}

// RecordOutcomeUnknown: `record_outcome_unknown {...}`.
func (c *Client) RecordOutcomeUnknown(ctx context.Context, session, actionID, attemptID ID, errText string) (*RecordOutcomeResult, error) {
	return call[RecordOutcomeResult](ctx, c, "record_outcome_unknown", map[string]any{
		"session_id": session, "action_id": actionID, "attempt_id": attemptID, "error": errText,
	})
}

// PendingReconciliation: `pending_reconciliation {}`.
func (c *Client) PendingReconciliation(ctx context.Context) (*PendingReconciliationResult, error) {
	return call[PendingReconciliationResult](ctx, c, "pending_reconciliation", struct{}{})
}

// RecordReconciliation: `record_reconciliation {...}`.
func (c *Client) RecordReconciliation(ctx context.Context, session, actionID ID, finding Finding) (*RecordReconciliationResult, error) {
	if finding.Details == nil {
		finding.Details = map[string]any{}
	}
	return call[RecordReconciliationResult](ctx, c, "record_reconciliation", map[string]any{
		"session_id": session, "action_id": actionID, "finding": finding,
	})
}

// Route: `route {task_id, failure_codes}`.
func (c *Client) Route(ctx context.Context, taskID string, failureCodes []string) (*RouteResult, error) {
	if failureCodes == nil {
		failureCodes = []string{}
	}
	return call[RouteResult](ctx, c, "route", map[string]any{"task_id": taskID, "failure_codes": failureCodes})
}

// RecordAssessment: `record_assessment {...}`.
func (c *Client) RecordAssessment(ctx context.Context, a AssessmentArgs) (*RecordAssessmentResult, error) {
	if a.ProbabilitiesBP == nil {
		a.ProbabilitiesBP = map[string]int64{}
	}
	if a.Usage == nil {
		a.Usage = map[string]int64{}
	}
	if a.Eligible == nil {
		a.Eligible = []string{}
	}
	if a.FailureCodes == nil {
		a.FailureCodes = []string{}
	}
	return call[RecordAssessmentResult](ctx, c, "record_assessment", a)
}

// TaskTransition: `task_transition {task_id, to, reason?}`; to is STOPPED or
// ESCALATED. The engine never requests COMPLETED.
func (c *Client) TaskTransition(ctx context.Context, taskID, to, reason string) (*TaskTransitionResult, error) {
	if to != "STOPPED" && to != "ESCALATED" {
		return nil, fmt.Errorf("coreclient: engine may only request STOPPED or ESCALATED, not %q", to)
	}
	args := map[string]string{"task_id": taskID, "to": to}
	if reason != "" {
		args["reason"] = reason
	}
	return call[TaskTransitionResult](ctx, c, "task_transition", args)
}

// TaskReport: `task_report {task_id}` (returned raw; §4.3 final report).
func (c *Client) TaskReport(ctx context.Context, taskID string) (json.RawMessage, error) {
	return c.CallRaw(ctx, "task_report", map[string]string{"task_id": taskID})
}

// ReplayVerify: `replay_verify {}`.
func (c *Client) ReplayVerify(ctx context.Context) (*ReplayVerifyResult, error) {
	return call[ReplayVerifyResult](ctx, c, "replay_verify", struct{}{})
}
