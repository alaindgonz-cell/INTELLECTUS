package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

// approval is an action awaiting the operator's decision. The approval the
// operator signs is bound to ActionDigest (the exact tool, arguments and
// base->target transition), never to a description of it.
type approval struct {
	ID           string
	MessageID    string
	TaskID       string
	ToolID       string
	ProposalID   coreclient.ID
	ActionDigest string
	FromRoot     string
	ToRoot       string
	Title        string
	Summary      string
	Checks       []CheckView
	Status       string // pending|approved|rejected|stale
	decision     chan bool
}

func (a *approval) card() *Card {
	return &Card{
		Type: "approval", ApprovalID: a.ID, Status: a.Status, TaskID: strPtr(a.TaskID), ToolID: a.ToolID,
		Title: a.Title, ActionDigest: a.ActionDigest, FromRoot: a.FromRoot, ToRoot: a.ToRoot, Checks: a.Checks, Summary: a.Summary,
	}
}

func (h *Harness) refreshApprovalCard(a *approval) {
	m, ok := h.chat.get(a.MessageID)
	if !ok {
		return
	}
	m.Card = a.card()
	h.repost(m)
}

// proposeAction submits a runtime-built action and returns the pending
// approval (MISSING_APPROVAL is the expected first admission result).
func (h *Harness) proposeAction(ctx context.Context, taskID, tool string, args map[string]any, post []string, key string) (coreclient.ID, *coreclient.AdmitResult, error) {
	env, _ := json.Marshal(map[string]any{
		"kind": "action", "task_id": taskID, "tool_id": tool, "arguments": args,
		"premise_refs": []string{}, "idempotency_key": key, "expected_postconditions": post,
	})
	sub, err := h.opt.Core.Submit(ctx, h.sess.scheduler, string(env))
	if err != nil {
		return "", nil, err
	}
	if sub.Status != "RECORDED" {
		return "", nil, fmt.Errorf("core rejected the action proposal: %s", strings.Join(sub.Reasons.Codes(), ", "))
	}
	adm, err := h.opt.Core.Admit(ctx, sub.ProposalID)
	if err != nil {
		return "", nil, err
	}
	return sub.ProposalID, adm, nil
}

func onlyMissingApproval(adm *coreclient.AdmitResult) bool {
	codes := adm.Reasons.Codes()
	return adm.Status == "REJECTED" && len(codes) == 1 && codes[0] == "MISSING_APPROVAL"
}

func (h *Harness) newApproval(a *approval, text string) *approval {
	h.mu.Lock()
	h.nextAppr++
	a.ID = fmt.Sprintf("a-%d", h.nextAppr)
	a.Status = "pending"
	a.decision = make(chan bool, 1)
	h.approvals[a.ID] = a
	h.mu.Unlock()
	m := h.post("assistant", text, a.TaskID, a.card())
	h.mu.Lock()
	a.MessageID = m.ID
	h.mu.Unlock()
	h.act(a.TaskID, "approval", "warn", "Waiting for operator approval: "+a.Title, "action "+shortDigest(a.ActionDigest), map[string]any{"approval_id": a.ID})
	return a
}

// wait blocks until the operator decides or the task context ends.
func (h *Harness) wait(ctx context.Context, a *approval) bool {
	select {
	case ok := <-a.decision:
		return ok
	case <-ctx.Done():
		h.mu.Lock()
		if a.Status == "pending" {
			a.Status = "stale"
		}
		h.mu.Unlock()
		h.refreshApprovalCard(a)
		return false
	}
}

// ApproveAction signs an approval bound to the exact action digest. It is
// only ever called from an explicit operator request.
func (h *Harness) ApproveAction(ctx context.Context, id string) error {
	h.mu.Lock()
	a, ok := h.approvals[id]
	if !ok || a.Status != "pending" {
		h.mu.Unlock()
		return fmt.Errorf("approval %s is not pending", id)
	}
	a.Status = "approving"
	h.mu.Unlock()
	res, err := h.opt.Core.OperatorOp(ctx, h.opt.Operator, "approve", map[string]any{
		"action_digest": a.ActionDigest, "tool_id": a.ToolID, "expires_at": h.opt.Now().Add(h.opt.ApprovalTTL).Unix(),
	})
	if err == nil && res.Status != "APPLIED" {
		err = fmt.Errorf("approval rejected by the core: %s", strings.Join(res.Reasons.Codes(), ", "))
	}
	h.mu.Lock()
	if err != nil {
		a.Status = "pending"
	} else {
		a.Status = "approved"
	}
	h.mu.Unlock()
	if err != nil {
		return err
	}
	h.refreshApprovalCard(a)
	h.act(a.TaskID, "approval", "ok", "Operator approved "+a.Title, "approval bound to action "+shortDigest(a.ActionDigest), nil)
	a.decision <- true
	return nil
}

// RejectAction declines a pending approval.
func (h *Harness) RejectAction(id string) error {
	h.mu.Lock()
	a, ok := h.approvals[id]
	if !ok || a.Status != "pending" {
		h.mu.Unlock()
		return fmt.Errorf("approval %s is not pending", id)
	}
	a.Status = "rejected"
	h.mu.Unlock()
	h.refreshApprovalCard(a)
	h.act(a.TaskID, "approval", "warn", "Operator rejected "+a.Title, "", nil)
	a.decision <- false
	return nil
}

// ---- promotion ------------------------------------------------------------------

func (h *Harness) requestPromotion(ctx context.Context, r *taskRun, sub *coreclient.SubmitResult, outs []runner.Outcome, ev *coreclient.EvaluateContextResult) {
	st, err := h.opt.Core.Status(ctx)
	if err != nil {
		h.stopOnError(ctx, r, err)
		return
	}
	key := fmt.Sprintf("promote:%s:%s", r.TaskID, strings.TrimPrefix(sub.CandidateRoot, "sha256:")[:16])
	prop, adm, err := h.proposeAction(ctx, r.TaskID, "promote_local", map[string]any{"candidate_root": sub.CandidateRoot}, nil, key)
	if err != nil {
		h.stopOnError(ctx, r, err)
		return
	}
	if !onlyMissingApproval(adm) && adm.Status != "AUTHORIZED" && adm.Status != "EXISTING" {
		h.finish(ctx, r, "ESCALATED", "the core refused the promotion: "+strings.Join(adm.Reasons.Codes(), ", "))
		return
	}
	checks := []CheckView{}
	for _, o := range outs {
		checks = append(checks, CheckView{Kind: o.Check.CheckKind, Result: o.CoreResult, Issuer: "runner:" + h.opt.Worker.Label(), Detail: o.Reported.Summary})
	}
	checks = append(checks, CheckView{Kind: "formal_context", Result: ev.Result, Issuer: "core:deductor", Detail: "approved tests are mutually consistent"})
	changed, _ := h.changedFiles(ctx, st.ApprovedRoot, sub.CandidateRoot)
	a := h.newApproval(&approval{
		TaskID: r.TaskID, ToolID: "promote_local", ProposalID: prop, ActionDigest: adm.ActionDigest,
		FromRoot: st.ApprovedRoot, ToRoot: sub.CandidateRoot, Title: "Promote candidate into the approved project",
		Summary: "Changed files: " + strings.Join(changed, ", "), Checks: checks,
	}, fmt.Sprintf("The candidate for %s (%q) passed its checks. Review the diff and approve to promote it into the approved project.", r.TaskID, r.Title))
	h.setPhase(r, "awaiting_approval")
	if !h.wait(ctx, a) {
		if ctx.Err() == nil {
			h.finish(ctx, r, "STOPPED", "the operator rejected the promotion")
		}
		return
	}
	h.setPhase(r, "promoting")
	adm, err = h.opt.Core.Admit(ctx, prop)
	if err != nil {
		h.stopOnError(ctx, r, err)
		return
	}
	if adm.Status != "AUTHORIZED" && adm.Status != "EXISTING" {
		h.mu.Lock()
		a.Status = "stale"
		h.mu.Unlock()
		h.refreshApprovalCard(a)
		h.finish(ctx, r, "ESCALATED", "the promotion was no longer admissible: "+strings.Join(adm.Reasons.Codes(), ", "))
		return
	}
	act := h.act(r.TaskID, "core", "running", "Promoting "+shortDigest(sub.CandidateRoot), "", nil)
	res, err := h.dispatcher().Dispatch(ctx, adm.ActionID)
	if err != nil || res.Begin.Status != "COMPLETED" {
		msg := "dispatch did not complete"
		if err != nil {
			msg = err.Error()
		} else {
			msg += ": " + res.Begin.Status + " " + strings.Join(res.Begin.Reasons.Codes(), ", ")
		}
		h.update(act, "fail", "Promotion failed", msg, nil)
		h.finish(ctx, r, "ESCALATED", msg)
		return
	}
	h.update(act, "ok", "Approved project advanced to "+shortDigest(sub.CandidateRoot), "", nil)
	h.setPhase(r, "done")
	h.reportPromotion(ctx, r, st.ApprovedRoot, sub.CandidateRoot, changed, outs, ev)
	h.publishStatus()
}

func (h *Harness) changedFiles(ctx context.Context, from, to string) ([]string, error) {
	a, err := h.opt.Core.ReadTree(ctx, from)
	if err != nil {
		return nil, err
	}
	b, err := h.opt.Core.ReadTree(ctx, to)
	if err != nil {
		return nil, err
	}
	var out []string
	for p, c := range b.Files {
		if old, ok := a.Files[p]; !ok {
			out = append(out, p+" (added)")
		} else if old != c {
			out = append(out, p)
		}
	}
	for p := range a.Files {
		if _, ok := b.Files[p]; !ok {
			out = append(out, p+" (removed)")
		}
	}
	sort.Strings(out)
	return out, nil
}

func (h *Harness) reportPromotion(ctx context.Context, r *taskRun, from, to string, changed []string, outs []runner.Outcome, ev *coreclient.EvaluateContextResult) {
	lines := []string{
		fmt.Sprintf("Promoted %s into the approved project (previous root %s).", shortDigest(to), shortDigest(from)),
		"Changed files: " + strings.Join(changed, ", "),
	}
	for _, o := range outs {
		lines = append(lines, fmt.Sprintf("%s: %s — %s (verification %s).", o.Check.CheckKind, o.CoreResult, o.Reported.Summary, o.VerificationID))
	}
	lines = append(lines, fmt.Sprintf("formal_context: %s (verification %s).", ev.Result, ev.VerificationID))
	h.mu.Lock()
	calls := r.calls
	routes := append([]map[string]any(nil), r.assessments...)
	h.mu.Unlock()
	for _, d := range routes {
		s := fmt.Sprintf("Routing: applied %v (%v)", d["applied"], d["source"])
		if c, ok := d["jev_choice"]; ok {
			s += fmt.Sprintf("; Jev suggested %v at %.0f%% confidence", c, float64(toInt(d["confidence_bp"]))/100)
		}
		lines = append(lines, s+".")
	}
	lines = append(lines, fmt.Sprintf("Model calls for this task: %d.", calls))
	var limitations []string
	if raw, err := h.opt.Core.TaskReport(ctx, r.TaskID); err == nil {
		var rep struct {
			Limitations []string `json:"limitations"`
		}
		if json.Unmarshal(raw, &rep) == nil {
			limitations = rep.Limitations
		}
	}
	h.post("assistant", fmt.Sprintf("Done: %s (%q) is complete and promoted.", r.TaskID, r.Title), r.TaskID,
		&Card{Type: "report", TaskID: strPtr(r.TaskID), Status: "COMPLETED", ApprovedRoot: to, Lines: lines, Limitations: limitations})
	h.act(r.TaskID, "core", "ok", "Task "+r.TaskID+" completed", "", nil)
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}

// ---- export -----------------------------------------------------------------------

// ProposeExport opens an export task (operator action) and asks for the
// approval of the exact export action.
func (h *Harness) ProposeExport(ctx context.Context) (string, error) {
	if h.opt.ExportDir == "" {
		return "", errors.New("no export directory configured")
	}
	st, err := h.opt.Core.Status(ctx)
	if err != nil {
		return "", err
	}
	tasks, err := h.opt.Core.ListTasks(ctx)
	if err != nil {
		return "", err
	}
	taskID := fmt.Sprintf("export-%d", len(tasks.Tasks)+1)
	res, err := h.opt.Core.OperatorOp(ctx, h.opt.Operator, "open_task", map[string]any{
		"task_id": taskID, "title": "Export the approved project", "requirement_refs": []string{},
	})
	if err != nil {
		return "", err
	}
	if res.Status != "APPLIED" {
		return "", fmt.Errorf("open_task rejected: %s", strings.Join(res.Reasons.Codes(), ", "))
	}
	target := fmt.Sprintf("%s-%s", st.ProjectID, strings.TrimPrefix(st.ApprovedRoot, "sha256:")[:12])
	args := map[string]any{"approved_root": st.ApprovedRoot, "target": target}
	prop, adm, err := h.proposeAction(ctx, taskID, "export_view", args,
		[]string{"observation.exported_root == arguments.approved_root"}, "export:"+taskID)
	if err != nil {
		return "", err
	}
	if !onlyMissingApproval(adm) {
		return "", fmt.Errorf("export not admissible: %s", strings.Join(adm.Reasons.Codes(), ", "))
	}
	a := h.newApproval(&approval{
		TaskID: taskID, ToolID: "export_view", ProposalID: prop, ActionDigest: adm.ActionDigest, FromRoot: st.ApprovedRoot, ToRoot: st.ApprovedRoot,
		Title: "Export the approved project to " + target, Summary: "Writes the approved tree into the export directory as " + target + ".",
		Checks: []CheckView{},
	}, "Approve to export the approved project tree to the export directory.")
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.runExport(h.rootCtx, a)
	}()
	return a.ID, nil
}

func (h *Harness) runExport(ctx context.Context, a *approval) {
	if !h.wait(ctx, a) {
		if ctx.Err() == nil {
			_, _ = h.opt.Core.TaskTransition(ctx, a.TaskID, "STOPPED", "operator rejected the export")
		}
		return
	}
	adm, err := h.opt.Core.Admit(ctx, a.ProposalID)
	if err != nil || (adm.Status != "AUTHORIZED" && adm.Status != "EXISTING") {
		msg := "not admissible"
		if err != nil {
			msg = err.Error()
		} else {
			msg += ": " + strings.Join(adm.Reasons.Codes(), ", ")
		}
		h.post("system", "Export failed: "+msg, a.TaskID, nil)
		return
	}
	act := h.act(a.TaskID, "gateway", "running", "Exporting the approved project", "", nil)
	res, err := h.dispatcher().Dispatch(ctx, adm.ActionID)
	switch {
	case err != nil:
		h.update(act, "fail", "Export failed", err.Error(), nil)
		h.post("system", "Export failed: "+err.Error(), a.TaskID, nil)
	case res.Outcome != nil && res.Outcome.Status == "OUTCOME_UNKNOWN":
		h.update(act, "warn", "Export outcome unknown; reconciliation required", "", nil)
		h.post("system", "The export's outcome is unknown; it will not be retried automatically.", a.TaskID, nil)
	case res.Outcome != nil:
		obs := res.Observation.Data
		h.update(act, "ok", "Export "+res.Outcome.Postconditions, fmt.Sprintf("%v", obs["path"]), nil)
		h.post("assistant", fmt.Sprintf("Exported the approved project to %v (postconditions %s).", obs["path"], res.Outcome.Postconditions), a.TaskID,
			&Card{Type: "report", TaskID: strPtr(a.TaskID), Status: "COMPLETED", ApprovedRoot: a.FromRoot,
				Lines: []string{fmt.Sprintf("Wrote %v files to %v.", obs["files"], obs["path"])}})
	default:
		h.update(act, "fail", "Export not started: "+res.Begin.Status, strings.Join(res.Begin.Reasons.Codes(), ", "), nil)
	}
	h.bus.Publish("tasks", map[string]any{"changed": a.TaskID})
}
