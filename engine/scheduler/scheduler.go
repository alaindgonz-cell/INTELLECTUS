// Package scheduler runs one task's loop:
//
//	plan -> candidate(s) -> protected checks -> (fail) route -> repair ...
//	     -> (pass) evaluate contexts -> action proposal -> admit
//	     -> (MISSING_APPROVAL) operator approval -> admit -> (optional) dispatch
//
// The scheduler only issues commands; the core decides everything. It never
// requests COMPLETED, never creates new task or proposal ids to reset a
// budget, stops as soon as the core rejects with REPAIR_BUDGET_EXHAUSTED,
// and never autonomously restarts a cancelled task.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/gateway"
	"github.com/alaindgonz-cell/intellectus/engine/modes"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

// Outcomes of Run.
const (
	OutcomeAuthorized      = "AUTHORIZED"
	OutcomeDispatched      = "DISPATCHED"
	OutcomeBudgetExhausted = "REPAIR_BUDGET_EXHAUSTED"
	OutcomeCancelled       = "CANCELLED"
	OutcomeEscalated       = "ESCALATED"
	OutcomeStopped         = "STOPPED"
	OutcomeRejected        = "REJECTED"
)

// ErrTaskCancelled is returned when Run is asked to (re)start a task that
// was cancelled. Cancelled tasks are never restarted autonomously.
var ErrTaskCancelled = errors.New("scheduler: task was cancelled; not restarting")

// Approver obtains an operator approval for an action digest. It is the
// human/operator boundary; the scheduler never approves on its own.
type Approver interface {
	Approve(ctx context.Context, taskID, actionDigest, toolID string) error
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(ctx context.Context, taskID, actionDigest, toolID string) error

// Approve implements Approver.
func (f ApproverFunc) Approve(ctx context.Context, taskID, actionDigest, toolID string) error {
	return f(ctx, taskID, actionDigest, toolID)
}

// Config wires a Scheduler.
type Config struct {
	Core   *coreclient.Client
	TaskID string

	Planner *modes.Mode
	Coder   *modes.Mode
	Runner  *runner.ProtectedRunner
	Router  *advisor.ShadowRunner

	// Contexts evaluated (evaluate_context) before proposing the action and
	// on GATHER_CONTEXT.
	Contexts []string
	// Fanout > 1 generates that many independent initial candidates in
	// parallel (bounded by MaxParallel).
	Fanout      int
	MaxParallel int
	// MaxIterations is an engine-local safety cap on loop turns. It never
	// resets or extends the core's repair budget (default 16).
	MaxIterations int

	// ActionTool is the tool proposed for a passing candidate (e.g. promote_local).
	ActionTool string
	Approver   Approver
	// Dispatcher, when set, dispatches the authorized action.
	Dispatcher *gateway.Dispatcher

	// Log receives trace lines (optional).
	Log func(format string, args ...any)
}

// CandidateRecord is one candidate submission and its checks.
type CandidateRecord struct {
	Phase      string
	Invocation *modes.Invocation
	Checks     []runner.Outcome
}

// Result is what Run observed.
type Result struct {
	Outcome         string
	Plan            *modes.Invocation
	Candidates      []CandidateRecord
	Decisions       []*advisor.Decision
	Contexts        []*coreclient.EvaluateContextResult
	PassingProposal coreclient.ID
	CandidateRoot   string
	ActionProposal  *modes.Invocation
	Admits          []*coreclient.AdmitResult
	Dispatch        *gateway.DispatchResult
	Reasons         coreclient.Reasons
}

// Scheduler runs tasks. One Scheduler remembers cancelled tasks.
type Scheduler struct {
	cfg       Config
	mu        sync.Mutex
	cancelled map[string]bool
}

// New builds a Scheduler.
func New(cfg Config) *Scheduler {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 16
	}
	if cfg.Fanout <= 0 {
		cfg.Fanout = 1
	}
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 4
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &Scheduler{cfg: cfg, cancelled: map[string]bool{}}
}

// Cancelled reports whether the scheduler has seen taskID cancelled.
func (s *Scheduler) Cancelled(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled[taskID]
}

func (s *Scheduler) markCancelled(taskID string) {
	s.mu.Lock()
	s.cancelled[taskID] = true
	s.mu.Unlock()
}

// Run drives the task loop until authorization/dispatch, a terminal route,
// budget exhaustion, rejection, or cancellation.
func (s *Scheduler) Run(ctx context.Context) (*Result, error) {
	c := s.cfg
	res := &Result{}
	if s.Cancelled(c.TaskID) {
		res.Outcome = OutcomeCancelled
		return res, ErrTaskCancelled
	}
	cancelled := func() (*Result, error) {
		s.markCancelled(c.TaskID)
		res.Outcome = OutcomeCancelled
		c.Log("scheduler: context cancelled; stopping task %s (no further commands, never restarted)", c.TaskID)
		return res, fmt.Errorf("%w: %v", ErrTaskCancelled, context.Cause(ctx))
	}
	if ctx.Err() != nil {
		return cancelled()
	}

	view, err := c.Core.View(ctx, c.Coder.Session, c.TaskID)
	if err != nil {
		if ctx.Err() != nil {
			return cancelled()
		}
		return res, fmt.Errorf("view: %w", err)
	}
	if view.Task == nil {
		return res, fmt.Errorf("view: task %s not visible", c.TaskID)
	}
	switch view.Task.Status {
	case "OPEN", "":
	case "CANCELLED":
		s.markCancelled(c.TaskID)
		res.Outcome = OutcomeCancelled
		return res, ErrTaskCancelled
	default:
		return res, fmt.Errorf("task %s is %s; scheduler only drives OPEN tasks", c.TaskID, view.Task.Status)
	}
	vars := baseVars(c.TaskID, view)

	// ---- plan ----
	if c.Planner != nil {
		inv, err := c.Planner.Invoke(ctx, modes.Request{TaskID: c.TaskID, Phase: "plan", Prompt: prompt("plan", view), Vars: vars})
		if ctx.Err() != nil {
			return cancelled()
		}
		if err != nil {
			return res, err
		}
		res.Plan = inv
		c.Log("planner: submit -> %s %s %v", inv.Submit.Status, inv.Submit.ProposalID, inv.Submit.Reasons.Codes())
		if inv.Submit.Status != "RECORDED" {
			res.Outcome, res.Reasons = OutcomeRejected, inv.Submit.Reasons
			return res, nil
		}
	}

	// ---- candidate / check / route loop ----
	phase := "initial"
	var lastFailures []string
	for iter := 0; ; iter++ {
		if ctx.Err() != nil {
			return cancelled()
		}
		if iter >= c.MaxIterations {
			c.Log("scheduler: engine safety cap (%d iterations) reached; escalating", c.MaxIterations)
			return s.transition(ctx, res, "ESCALATED", OutcomeEscalated, "engine safety cap reached")
		}
		n := 1
		if phase == "initial" {
			n = c.Fanout
		}
		reqs := make([]modes.Request, n)
		for i := range reqs {
			v := cloneVars(vars)
			v["candidate_index"] = fmt.Sprint(i)
			v["last_failures"] = strings.Join(lastFailures, ",")
			reqs[i] = modes.Request{TaskID: c.TaskID, Phase: phase, Prompt: prompt(phase, view), Vars: v}
		}
		invs, errs := c.Coder.InvokeParallel(ctx, reqs, c.MaxParallel)
		if ctx.Err() != nil {
			return cancelled()
		}
		var recorded []*modes.Invocation
		var rejectCodes []string
		for i, inv := range invs {
			if errs[i] != nil {
				return res, errs[i]
			}
			sub := inv.Submit
			c.Log("coder[%s]: submit -> %s %s root=%s %v", phase, sub.Status, sub.ProposalID, short(sub.CandidateRoot), sub.Reasons.Codes())
			if sub.Status != "RECORDED" {
				res.Candidates = append(res.Candidates, CandidateRecord{Phase: phase, Invocation: inv})
				if sub.Reasons.Has("REPAIR_BUDGET_EXHAUSTED") {
					// The core enforces the limit; the engine stops here and
					// never opens new ids to reset the budget.
					res.Reasons = sub.Reasons
					c.Log("scheduler: core rejected with REPAIR_BUDGET_EXHAUSTED; stopping repairs")
					r, err := s.transition(ctx, res, "ESCALATED", OutcomeBudgetExhausted, "core rejected repair: REPAIR_BUDGET_EXHAUSTED")
					r.Outcome = OutcomeBudgetExhausted
					return r, err
				}
				rejectCodes = append(rejectCodes, sub.Reasons.Codes()...)
				continue
			}
			recorded = append(recorded, inv)
		}

		var passing *modes.Invocation
		var failureCodes []string
		for _, inv := range recorded {
			if ctx.Err() != nil {
				return cancelled()
			}
			outs, err := c.Runner.RunChecks(ctx, inv.Submit.ProposalID)
			rec := CandidateRecord{Phase: phase, Invocation: inv, Checks: outs}
			res.Candidates = append(res.Candidates, rec)
			if ctx.Err() != nil {
				return cancelled()
			}
			if err != nil {
				return res, err
			}
			for _, o := range outs {
				c.Log("runner: %s %s reported=%s core=%s (%s)", o.Check.CheckID, o.Check.CheckKind, o.Reported.Result, o.CoreResult, o.Reported.Summary)
			}
			if runner.AllPass(outs) {
				passing = inv
				break
			}
			failureCodes = append(failureCodes, runner.FailureCodes(outs)...)
		}
		if passing != nil {
			res.PassingProposal = passing.Submit.ProposalID
			res.CandidateRoot = passing.Submit.CandidateRoot
			break
		}
		failureCodes = append(failureCodes, rejectCodes...)
		lastFailures = failureCodes

		if ctx.Err() != nil {
			return cancelled()
		}
		dec, err := c.Router.Decide(ctx, c.TaskID, failureCodes)
		if ctx.Err() != nil {
			return cancelled()
		}
		if err != nil {
			return res, err
		}
		res.Decisions = append(res.Decisions, dec)
		logDecision(c.Log, dec)

		switch dec.Applied {
		case coreclient.RouteRepair:
			phase = "repair"
		case coreclient.RouteGatherContext:
			if err := s.evalContexts(ctx, res); err != nil {
				return res, err
			}
			phase = "repair"
		case coreclient.RouteReplan:
			if c.Planner == nil {
				return s.transition(ctx, res, "ESCALATED", OutcomeEscalated, "REPLAN routed but no planner configured")
			}
			inv, err := c.Planner.Invoke(ctx, modes.Request{TaskID: c.TaskID, Phase: "replan", Prompt: prompt("replan", view), Vars: vars})
			if err != nil {
				return res, err
			}
			if inv.Submit.Status != "RECORDED" {
				res.Outcome, res.Reasons = OutcomeRejected, inv.Submit.Reasons
				return res, nil
			}
			phase = "repair"
		case coreclient.RouteEscalate:
			return s.transition(ctx, res, "ESCALATED", OutcomeEscalated, "route applied ESCALATE")
		case coreclient.RouteStop:
			return s.transition(ctx, res, "STOPPED", OutcomeStopped, "route applied STOP")
		default:
			return res, fmt.Errorf("unknown applied route %q", dec.Applied)
		}
		// Refresh the view so the next prompt sees the core's latest state.
		if v, err := c.Core.View(ctx, c.Coder.Session, c.TaskID); err == nil {
			view = v
		} else if ctx.Err() != nil {
			return cancelled()
		}
	}

	// ---- contexts, action, admission ----
	if ctx.Err() != nil {
		return cancelled()
	}
	if err := s.evalContexts(ctx, res); err != nil {
		if ctx.Err() != nil {
			return cancelled()
		}
		return res, err
	}
	if c.ActionTool == "" {
		res.Outcome = OutcomeAuthorized
		return res, nil
	}
	av := cloneVars(vars)
	av["tool_id"] = c.ActionTool
	av["candidate_root"] = res.CandidateRoot
	av["proposal_id"] = res.PassingProposal.String()
	// Stable per (task, proposal, tool): a retry reuses the same key.
	av["idempotency_key"] = fmt.Sprintf("idem:%s:%s:%s", c.TaskID, res.PassingProposal.String(), c.ActionTool)
	inv, err := c.Coder.Invoke(ctx, modes.Request{TaskID: c.TaskID, Phase: "action", Prompt: prompt("action", view), Vars: av})
	if ctx.Err() != nil {
		return cancelled()
	}
	if err != nil {
		return res, err
	}
	res.ActionProposal = inv
	c.Log("coder[action]: submit -> %s %s %v", inv.Submit.Status, inv.Submit.ProposalID, inv.Submit.Reasons.Codes())
	if inv.Submit.Status != "RECORDED" {
		res.Outcome, res.Reasons = OutcomeRejected, inv.Submit.Reasons
		return res, nil
	}
	adm, err := c.Core.Admit(ctx, inv.Submit.ProposalID)
	if err != nil {
		return res, fmt.Errorf("admit: %w", err)
	}
	res.Admits = append(res.Admits, adm)
	c.Log("admit: %s action_digest=%s %v", adm.Status, adm.ActionDigest, adm.Reasons.Codes())
	if adm.Status == "REJECTED" && adm.Reasons.Has("MISSING_APPROVAL") && c.Approver != nil && adm.ActionDigest != "" {
		if ctx.Err() != nil {
			return cancelled()
		}
		if err := c.Approver.Approve(ctx, c.TaskID, adm.ActionDigest, c.ActionTool); err != nil {
			if ctx.Err() != nil {
				return cancelled()
			}
			return res, fmt.Errorf("approval: %w", err)
		}
		adm, err = c.Core.Admit(ctx, inv.Submit.ProposalID)
		if err != nil {
			return res, fmt.Errorf("admit: %w", err)
		}
		res.Admits = append(res.Admits, adm)
		c.Log("admit: %s action_id=%s action_digest=%s %v", adm.Status, adm.ActionID, adm.ActionDigest, adm.Reasons.Codes())
	}
	if adm.Status != "AUTHORIZED" && adm.Status != "EXISTING" {
		res.Outcome, res.Reasons = OutcomeRejected, adm.Reasons
		return res, nil
	}
	res.Outcome = OutcomeAuthorized
	if c.Dispatcher == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return cancelled()
	}
	d, err := c.Dispatcher.Dispatch(ctx, adm.ActionID)
	if errors.Is(err, gateway.ErrCancelled) {
		return cancelled()
	}
	res.Dispatch = d
	if err != nil {
		return res, err
	}
	res.Outcome = OutcomeDispatched
	return res, nil
}

func (s *Scheduler) evalContexts(ctx context.Context, res *Result) error {
	for _, cx := range s.cfg.Contexts {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, err := s.cfg.Core.EvaluateContext(ctx, cx)
		if err != nil {
			return fmt.Errorf("evaluate_context %s: %w", cx, err)
		}
		res.Contexts = append(res.Contexts, ev)
		s.cfg.Log("evaluate_context %s -> %s consistent=%v", cx, ev.Result, ev.Consistent)
	}
	return nil
}

func (s *Scheduler) transition(ctx context.Context, res *Result, to, outcome, reason string) (*Result, error) {
	res.Outcome = outcome
	if ctx.Err() != nil {
		return res, nil
	}
	tr, err := s.cfg.Core.TaskTransition(ctx, s.cfg.TaskID, to, reason)
	if err != nil {
		return res, fmt.Errorf("task_transition %s: %w", to, err)
	}
	s.cfg.Log("task_transition %s -> %s", to, tr.TaskStatus)
	return res, nil
}

func logDecision(log func(string, ...any), d *advisor.Decision) {
	det := "null"
	if d.Route.DeterministicChoice != nil {
		det = *d.Route.DeterministicChoice
	}
	log("route: eligible=%v deterministic_choice=%s reason=%q", d.Route.Eligible, det, d.Route.Reason)
	if d.Advice != nil {
		log("advisor [SIMULATED unless LIVE] mode=%s suggested=%s confidence=%dbp probabilities=%v", d.Advice.Mode, d.Advice.Choice, d.Advice.ConfidenceBP, d.Advice.ProbabilitiesBP)
	}
	if d.Assessment != nil {
		log("record_assessment: assessment_id=%s used_advisor=%v applied_choice=%s fallback=%q", d.Assessment.AssessmentID, d.Assessment.UsedAdvisor, d.Assessment.AppliedChoice, d.FallbackReason)
	}
	log("applied route: %s (source=%s)", d.Applied, d.Source)
}

func baseVars(taskID string, v *coreclient.ViewResult) map[string]string {
	m := map[string]string{"task_id": taskID}
	if v.Task != nil {
		m["requirement_refs"] = strings.Join(v.Task.RequirementRefs, ",")
	}
	if v.ApprovedRoot != nil {
		m["base_root"] = v.ApprovedRoot.Digest
	}
	return m
}

func cloneVars(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func prompt(phase string, v *coreclient.ViewResult) string {
	return "phase: " + phase + "\nview:\n" + string(v.Raw)
}

func short(s string) string {
	if len(s) > 19 {
		return s[:19] + "…"
	}
	return s
}
