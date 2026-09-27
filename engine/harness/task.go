package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/llm"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

// taskRun is the in-memory state of a task's work loop.
type taskRun struct {
	TaskID  string
	Title   string
	Phase   string
	active  bool
	cancel  context.CancelFunc
	calls   int
	updated int64
	// details per candidate proposal id, for the UI and repair prompts.
	details     map[string][]runner.CaseDetail
	assessments []map[string]any
}

func (h *Harness) startRun(taskID, title string, resume bool) {
	ctx, cancel := context.WithCancel(h.rootCtx)
	h.mu.Lock()
	r := &taskRun{TaskID: taskID, Title: title, Phase: "planning", active: true, cancel: cancel, updated: h.now(), details: map[string][]runner.CaseDetail{}}
	if old, ok := h.runs[taskID]; ok {
		r.details, r.assessments = old.details, old.assessments
	}
	h.runs[taskID] = r
	h.mu.Unlock()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer cancel()
		h.runTask(ctx, r, resume)
		h.mu.Lock()
		r.active = false
		h.mu.Unlock()
		h.bus.Publish("tasks", map[string]any{"changed": taskID})
		h.publishStatus()
	}()
}

func (h *Harness) setPhase(r *taskRun, phase string) {
	h.mu.Lock()
	r.Phase, r.updated = phase, h.now()
	h.mu.Unlock()
	h.bus.Publish("tasks", map[string]any{"changed": r.TaskID})
}

// attemptInfo is what the next Coder call learns about the last failure.
type attemptInfo struct {
	failures  string // rendered failing cases / rejection reasons
	diagnosis string // Tester analysis (GATHER_CONTEXT)
	candidate map[string]string
}

var errBudget = errors.New("model-call budget for this task is exhausted")

func (h *Harness) callModel(ctx context.Context, r *taskRun, call llm.Call, what string) (llm.Result, error) {
	h.mu.Lock()
	if r.calls >= h.opt.MaxModelCallsPerTask {
		h.mu.Unlock()
		return llm.Result{}, errBudget
	}
	r.calls++
	h.mu.Unlock()
	act := h.act(r.TaskID, "model", "running", fmt.Sprintf("%s (%s) is %s", roleName(call.Role), h.model(call.Role), what), "", nil)
	res, err := h.opt.LLM.Complete(ctx, call)
	if err != nil {
		h.update(act, "fail", roleName(call.Role)+" call failed", err.Error(), nil)
		return res, err
	}
	h.meter(res)
	h.update(act, "ok", fmt.Sprintf("%s (%s) finished %s", roleName(call.Role), res.ModelReturned, what),
		"", map[string]any{"input_tokens": res.Usage.InputTokens, "output_tokens": res.Usage.OutputTokens, "latency_ms": res.LatencyMS, "attempts": res.Attempts})
	return res, nil
}

func roleName(role string) string {
	if role == "" {
		return "Model"
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

// runTask drives one task to an approval request or a stop.
func (h *Harness) runTask(ctx context.Context, r *taskRun, resume bool) {
	if !h.opt.LLM.Describe().Configured {
		h.finish(ctx, r, "ESCALATED", "Claude is not configured (set ANTHROPIC_API_KEY)")
		return
	}
	tsum, err := h.taskSummary(ctx, r.TaskID)
	if err != nil {
		h.act(r.TaskID, "error", "fail", "Cannot load task", err.Error(), nil)
		return
	}
	if resume {
		h.act(r.TaskID, "system", "info", "Resuming "+r.TaskID, "", nil)
	}
	plan, err := h.plan(ctx, r, tsum, "")
	if err != nil {
		h.stopOnError(ctx, r, err)
		return
	}
	var last attemptInfo
	for {
		if ctx.Err() != nil {
			return
		}
		h.setPhase(r, "coding")
		sub, cand, err := h.code(ctx, r, tsum, plan, last)
		if err != nil {
			h.stopOnError(ctx, r, err)
			return
		}
		var codes []string
		if sub.Status != "RECORDED" {
			codes = sub.Reasons.Codes()
			if contains(codes, "REPAIR_BUDGET_EXHAUSTED") || contains(codes, "TASK_NOT_OPEN") {
				h.finish(ctx, r, "ESCALATED", "The core refused another candidate: "+strings.Join(codes, ", "))
				return
			}
			last = attemptInfo{failures: "The core rejected the candidate: " + strings.Join(codes, ", ") + ". Fix the submission (e.g. do not touch protected paths).", candidate: cand}
			h.act(r.TaskID, "core", "fail", "Candidate rejected by the core", strings.Join(codes, ", "), nil)
		} else {
			h.setPhase(r, "testing")
			outs, ok, err := h.test(ctx, r, sub)
			if err != nil {
				h.stopOnError(ctx, r, err)
				return
			}
			if ok {
				h.setPhase(r, "verifying")
				ev, err := h.opt.Core.EvaluateContext(ctx, "ctx:"+r.TaskID)
				if err != nil {
					h.stopOnError(ctx, r, err)
					return
				}
				if ev.Result != "PASS" {
					h.act(r.TaskID, "core", "fail", "Spec-consistency check "+ev.Result, "The approved acceptance tests contradict each other; the operator must revise them.", nil)
					h.finish(ctx, r, "ESCALATED", "the approved tests are inconsistent (formal context "+ev.Result+")")
					return
				}
				h.act(r.TaskID, "core", "ok", "Spec-consistency check PASS", "The deductor found no input that the approved tests both accept and reject.", map[string]any{"verification_id": ev.VerificationID})
				h.requestPromotion(ctx, r, sub, outs, ev)
				return
			}
			codes = runner.FailureCodes(outs)
			last = attemptInfo{failures: renderFailures(outs), candidate: cand}
		}
		// Route the failure: deterministic rules first, then (optionally) Jev.
		h.setPhase(r, "routing")
		d, err := h.route(ctx, r, codes)
		if err != nil {
			h.stopOnError(ctx, r, err)
			return
		}
		switch d.Applied {
		case coreclient.RouteRepair:
		case coreclient.RouteGatherContext:
			diag, err := h.diagnose(ctx, r, tsum, last)
			if err != nil {
				h.stopOnError(ctx, r, err)
				return
			}
			last.diagnosis = diag
		case coreclient.RouteReplan:
			if plan, err = h.plan(ctx, r, tsum, last.failures); err != nil {
				h.stopOnError(ctx, r, err)
				return
			}
		case coreclient.RouteEscalate:
			h.finish(ctx, r, "ESCALATED", "routing chose ESCALATE ("+d.Route.Reason+")")
			return
		default:
			h.finish(ctx, r, "STOPPED", "routing chose "+d.Applied)
			return
		}
	}
}

func (h *Harness) taskSummary(ctx context.Context, taskID string) (*coreclient.TaskSummary, error) {
	ts, err := h.opt.Core.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ts.Tasks {
		if ts.Tasks[i].TaskID == taskID {
			return &ts.Tasks[i], nil
		}
	}
	return nil, fmt.Errorf("unknown task %s", taskID)
}

func (h *Harness) stopOnError(ctx context.Context, r *taskRun, err error) {
	if ctx.Err() != nil {
		return // cancelled: the canceller reports
	}
	var refusal *llm.RefusalError
	switch {
	case errors.Is(err, errBudget):
		h.finish(ctx, r, "ESCALATED", err.Error())
	case errors.As(err, &refusal):
		h.finish(ctx, r, "ESCALATED", "the model declined the request: "+refusal.Error())
	default:
		h.finish(ctx, r, "ESCALATED", "an error stopped the work: "+err.Error())
	}
}

// finish moves an open task to STOPPED/ESCALATED and reports in chat. It
// never marks a task complete (only the core does, on a verified promotion).
func (h *Harness) finish(ctx context.Context, r *taskRun, to, reason string) {
	h.setPhase(r, strings.ToLower(to))
	if _, err := h.opt.Core.TaskTransition(context.WithoutCancel(ctx), r.TaskID, to, reason); err != nil {
		h.opt.Logf("task_transition: %v", err)
	}
	kind, status := "system", "warn"
	h.act(r.TaskID, kind, status, fmt.Sprintf("Task %s %s", r.TaskID, strings.ToLower(to)), reason, nil)
	h.post("assistant", fmt.Sprintf("I stopped working on %s (%q): %s. The approved project is unchanged.", r.TaskID, r.Title, reason), r.TaskID,
		&Card{Type: "report", TaskID: strPtr(r.TaskID), Status: to, Lines: []string{reason}, Limitations: []string{"No candidate from this task was promoted."}})
}

func strPtr(s string) *string { return &s }

// ---- roles -------------------------------------------------------------------

func (h *Harness) taskBrief(ctx context.Context, t *coreclient.TaskSummary) (string, map[string]string, error) {
	tree, err := h.opt.Core.ReadTree(ctx, "")
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	req := ""
	if t.Requirement != nil {
		req = *t.Requirement
	}
	fmt.Fprintf(&b, "<requirement>\n%s\n</requirement>\n\n", req)
	if t.Entrypoint != nil {
		fmt.Fprintf(&b, "<entrypoint>\nfile: %s\nfunction: %s(arg)  (called with exactly one positional argument: the test input)\n</entrypoint>\n\n", t.Entrypoint.Path, t.Entrypoint.Function)
	}
	b.WriteString("<acceptance_tests>\n")
	for _, c := range t.Cases {
		fmt.Fprintf(&b, "- %s: input %s -> %s\n", c.Name, string(c.Input), describeExpect(c.Expect))
	}
	b.WriteString("</acceptance_tests>\n\nProtected paths (never modify): tests/acceptance/\n\n")
	first := []string{}
	if t.Entrypoint != nil {
		first = append(first, t.Entrypoint.Path)
	}
	b.WriteString("<project>\n" + renderFiles(tree.Files, contextBudget, first) + "</project>\n")
	return b.String(), tree.Files, nil
}

func describeExpect(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	if v, ok := m["returns"]; ok {
		return "returns " + string(v)
	}
	if v, ok := m["raises"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		return "raises " + s
	}
	return string(raw)
}

func (h *Harness) plan(ctx context.Context, r *taskRun, t *coreclient.TaskSummary, failures string) (string, error) {
	h.setPhase(r, "planning")
	brief, _, err := h.taskBrief(ctx, t)
	if err != nil {
		return "", err
	}
	user := brief
	what := "planning"
	if failures != "" {
		user += "\n<previous_attempt_failed>\n" + failures + "\n</previous_attempt_failed>\nThe previous approach failed. Write a revised plan."
		what = "revising the plan"
	}
	res, err := h.callModel(ctx, r, llm.Call{Role: "planner", System: plannerSystem, Messages: []llm.Message{{Role: "user", Text: user}}, Tool: submitPlanTool, RequireTool: true, MaxTokens: 16000}, what)
	if err != nil {
		return "", err
	}
	var in struct {
		Summary string   `json:"summary"`
		Steps   []string `json:"steps"`
	}
	if err := strictDecode(res.ToolInput, &in); err != nil {
		return "", fmt.Errorf("planner output: %w", err)
	}
	env, _ := json.Marshal(map[string]any{"kind": "plan", "task_id": r.TaskID, "summary": in.Summary, "steps": in.Steps, "premise_refs": t.RequirementRefs})
	sub, err := h.opt.Core.SubmitWithProvider(ctx, h.sess.planner, string(env), providerRecord(res))
	if err != nil {
		return "", err
	}
	if sub.Status != "RECORDED" {
		return "", fmt.Errorf("core rejected the plan: %s", strings.Join(sub.Reasons.Codes(), ", "))
	}
	text := in.Summary
	for i, s := range in.Steps {
		text += fmt.Sprintf("\n%d. %s", i+1, s)
	}
	h.act(r.TaskID, "core", "info", "Plan recorded ("+string(sub.ProposalID)+")", text, nil)
	return text, nil
}

func (h *Harness) code(ctx context.Context, r *taskRun, t *coreclient.TaskSummary, plan string, last attemptInfo) (*coreclient.SubmitResult, map[string]string, error) {
	brief, _, err := h.taskBrief(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	st, err := h.opt.Core.Status(ctx)
	if err != nil {
		return nil, nil, err
	}
	user := brief + "\n<plan>\n" + plan + "\n</plan>\n"
	what := "writing a candidate"
	if last.failures != "" {
		what = "repairing the candidate"
		var prev strings.Builder
		paths := make([]string, 0, len(last.candidate))
		for p := range last.candidate {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&prev, "<file path=%q>\n%s\n</file>\n", p, last.candidate[p])
		}
		user += "\n<previous_candidate>\n" + prev.String() + "</previous_candidate>\n\n<failures>\n" + last.failures + "\n</failures>\n"
		if last.diagnosis != "" {
			user += "\n<tester_diagnosis>\n" + last.diagnosis + "\n</tester_diagnosis>\n"
		}
		user += "\nFix the candidate so that it satisfies the requirement."
	}
	res, err := h.callModel(ctx, r, llm.Call{Role: "coder", System: coderSystem, Messages: []llm.Message{{Role: "user", Text: user}}, Tool: submitCandidateTool, RequireTool: true}, what)
	if err != nil {
		return nil, nil, err
	}
	var in struct {
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"files"`
		Rationale string `json:"rationale"`
	}
	if err := strictDecode(res.ToolInput, &in); err != nil {
		return nil, nil, fmt.Errorf("coder output: %w", err)
	}
	changed := map[string]string{}
	for _, f := range in.Files {
		changed[f.Path] = f.Content
	}
	// Repair and diagnosis prompts show the candidate's changed files (the
	// rest of the project is in the brief already).
	cand := map[string]string{}
	for p, c := range changed {
		cand[p] = c
	}
	// The runtime binds task and base root; the model supplies only content.
	env, _ := json.Marshal(map[string]any{"kind": "candidate", "task_id": r.TaskID, "base_root": st.ApprovedRoot, "files": changed, "rationale": in.Rationale})
	sub, err := h.opt.Core.SubmitWithProvider(ctx, h.sess.coder, string(env), providerRecord(res))
	if err != nil {
		return nil, nil, err
	}
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if sub.Status == "RECORDED" {
		h.act(r.TaskID, "core", "ok", fmt.Sprintf("Candidate %s recorded (%s)", sub.ProposalID, shortDigest(sub.CandidateRoot)),
			"Changed: "+strings.Join(paths, ", ")+"\n"+in.Rationale, map[string]any{"proposal_id": sub.ProposalID, "candidate_root": sub.CandidateRoot})
	}
	return sub, cand, nil
}

func (h *Harness) test(ctx context.Context, r *taskRun, sub *coreclient.SubmitResult) ([]runner.Outcome, bool, error) {
	if h.opt.Worker == nil {
		return nil, false, errors.New("no sandbox worker")
	}
	act := h.act(r.TaskID, "sandbox", "running", "Running protected acceptance tests in the "+h.opt.Worker.Kind()+" sandbox", "candidate "+shortDigest(sub.CandidateRoot), nil)
	pr := &runner.ProtectedRunner{Core: h.opt.Core, Session: h.sess.runner, Worker: h.opt.Worker}
	outs, err := pr.RunChecks(ctx, sub.ProposalID)
	if err != nil {
		h.update(act, "fail", "Test run failed", err.Error(), nil)
		return nil, false, err
	}
	if len(outs) == 0 {
		h.update(act, "fail", "No checks were planned for this candidate", "", nil)
		return nil, false, errors.New("the core planned no acceptance checks")
	}
	h.mu.Lock()
	r.details[string(sub.ProposalID)] = outs[0].Reported.Details
	h.mu.Unlock()
	ok := runner.AllPass(outs)
	status := "fail"
	if ok {
		status = "ok"
	}
	o := outs[0]
	title := fmt.Sprintf("Tests %s: %s", o.CoreResult, o.Reported.Summary)
	h.update(act, status, title, renderCaseTable(o.Reported.Details), map[string]any{"verification_id": o.VerificationID, "result": o.CoreResult, "proposal_id": sub.ProposalID})
	h.bus.Publish("tasks", map[string]any{"changed": r.TaskID})
	return outs, ok, nil
}

func renderCaseTable(ds []runner.CaseDetail) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "%-7s %s", d.Status, d.Name)
		if d.Status != "PASS" {
			fmt.Fprintf(&b, "  input=%s expected=%s observed=%s", string(d.Input), string(d.Expected), string(d.Observed))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderFailures(outs []runner.Outcome) string {
	var b strings.Builder
	for _, o := range outs {
		fmt.Fprintf(&b, "Check %s: %s (%s)\n", o.Check.CheckKind, o.CoreResult, o.Reported.Summary)
		for _, d := range o.Reported.Details {
			if d.Status == "PASS" {
				continue
			}
			fmt.Fprintf(&b, "- %s [%s]: input %s, expected %s, observed %s\n", d.Name, d.Status, string(d.Input), describeExpect(d.Expected), string(d.Observed))
			if s := strings.TrimSpace(d.Stderr); s != "" {
				fmt.Fprintf(&b, "  stderr: %s\n", truncate(s, 800))
			}
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (h *Harness) diagnose(ctx context.Context, r *taskRun, t *coreclient.TaskSummary, last attemptInfo) (string, error) {
	h.setPhase(r, "diagnosing")
	brief, _, err := h.taskBrief(ctx, t)
	if err != nil {
		return "", err
	}
	var cand strings.Builder
	for p, c := range last.candidate {
		fmt.Fprintf(&cand, "<file path=%q>\n%s\n</file>\n", p, c)
	}
	user := brief + "\n<candidate>\n" + cand.String() + "</candidate>\n\n<failures>\n" + last.failures + "\n</failures>"
	res, err := h.callModel(ctx, r, llm.Call{Role: "tester", System: testerSystem, Messages: []llm.Message{{Role: "user", Text: user}}, Tool: submitDiagnosisTool, RequireTool: true, MaxTokens: 16000}, "diagnosing the failures")
	if err != nil {
		return "", err
	}
	var in struct {
		Diagnosis    string `json:"diagnosis"`
		RootCause    string `json:"root_cause"`
		SuggestedFix string `json:"suggested_fix"`
	}
	if err := strictDecode(res.ToolInput, &in); err != nil {
		return "", fmt.Errorf("tester output: %w", err)
	}
	if _, err := h.opt.Core.RecordInput(ctx, h.sess.tester, recordText(res), providerRecord(res)); err != nil {
		h.opt.Logf("record_input: %v", err)
	}
	text := fmt.Sprintf("Diagnosis: %s\nRoot cause: %s\nSuggested fix: %s", in.Diagnosis, in.RootCause, in.SuggestedFix)
	h.act(r.TaskID, "model", "info", "Tester diagnosis", text, nil)
	return text, nil
}

// route asks the core (and, when no deterministic rule applies, Jev).
func (h *Harness) route(ctx context.Context, r *taskRun, codes []string) (*advisor.Decision, error) {
	st, err := h.opt.Core.Status(ctx)
	if err != nil {
		return nil, err
	}
	mode, _ := dig(decodePolicy(st.Policy), "jev", "mode").(string)
	if mode == "" {
		mode = advisor.ModeOff
	}
	var adv advisor.DecisionAdvisor
	if h.opt.AdvisorInfo.Configured {
		adv = h.opt.Advisor
	}
	sr := &advisor.ShadowRunner{Core: h.opt.Core, Session: h.sess.advisor, Advisor: adv, Mode: mode, Timeout: h.opt.AdvisorTimeout}
	act := h.act(r.TaskID, "jev", "running", "Routing the failure ("+strings.Join(codes, ", ")+")", "", nil)
	d, err := sr.Decide(ctx, r.TaskID, codes)
	if err != nil {
		h.update(act, "fail", "Routing failed", err.Error(), nil)
		return nil, err
	}
	h.meterJev(d.Advice)
	var detail string
	data := map[string]any{"applied": d.Applied, "source": d.Source, "policy_mode": mode}
	switch {
	case d.Source == "deterministic":
		detail = "Deterministic rule: " + d.Route.Reason + " (Jev not consulted)"
	case d.Advice != nil:
		detail = fmt.Sprintf("Jev (%s) suggested %s with confidence %.0f%%", d.Advice.ModelReturned, d.Advice.Choice, float64(d.Advice.ConfidenceBP)/100)
		if d.FallbackReason != "" {
			detail += "; not applied (" + d.FallbackReason + ")"
		} else {
			detail += "; applied"
		}
		data["jev_choice"], data["confidence_bp"] = d.Advice.Choice, d.Advice.ConfidenceBP
	case d.AdviceErr != nil:
		detail = "Jev unavailable: " + d.AdviceErr.Error() + "; deterministic fallback applied"
	default:
		detail = "Jev not consulted (" + d.FallbackReason + "); deterministic fallback applied"
	}
	if d.Assessment != nil {
		data["assessment_id"] = d.Assessment.AssessmentID
	}
	h.mu.Lock()
	r.assessments = append(r.assessments, data)
	h.mu.Unlock()
	h.update(act, "ok", "Next step: "+d.Applied, detail, data)
	return d, nil
}

func strictDecode(raw json.RawMessage, v any) error {
	if raw == nil {
		return errors.New("no tool input")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func shortDigest(d string) string {
	if strings.HasPrefix(d, "sha256:") && len(d) > 19 {
		return d[:19]
	}
	return d
}

// ResumeTask restarts the work loop of an open task (e.g. after a restart).
func (h *Harness) ResumeTask(ctx context.Context, taskID string) error {
	t, err := h.taskSummary(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status != "OPEN" || t.TestManifest == nil {
		return fmt.Errorf("task %s is %s and cannot be resumed", taskID, t.Status)
	}
	h.mu.Lock()
	if r, ok := h.runs[taskID]; ok && r.active {
		h.mu.Unlock()
		return fmt.Errorf("task %s is already running", taskID)
	}
	h.mu.Unlock()
	h.startRun(taskID, t.Title, true)
	return nil
}

// CancelTask cancels an open task through the operator path.
func (h *Harness) CancelTask(ctx context.Context, taskID string) error {
	h.mu.Lock()
	if r, ok := h.runs[taskID]; ok && r.cancel != nil {
		r.cancel()
	}
	for _, a := range h.approvals {
		if a.TaskID == taskID && a.Status == "pending" {
			a.Status = "rejected"
			select {
			case a.decision <- false:
			default:
			}
		}
	}
	h.mu.Unlock()
	res, err := h.opt.Core.OperatorOp(ctx, h.opt.Operator, "cancel_task", map[string]any{"task_id": taskID})
	if err != nil {
		return err
	}
	if res.Status != "APPLIED" {
		return fmt.Errorf("cancel rejected: %s", strings.Join(res.Reasons.Codes(), ", "))
	}
	h.act(taskID, "approval", "warn", "Operator cancelled "+taskID, "", nil)
	h.post("system", "Task "+taskID+" was cancelled by the operator. The approved project is unchanged by it.", taskID, nil)
	h.bus.Publish("tasks", map[string]any{"changed": taskID})
	return nil
}
