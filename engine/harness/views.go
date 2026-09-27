package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

// TaskListItem is one row of GET /api/tasks.
type TaskListItem struct {
	TaskID           string `json:"task_id"`
	Title            string `json:"title"`
	Status           string `json:"status"`
	RepairsUsed      int64  `json:"repairs_used"`
	MaxRepairs       int64  `json:"max_repairs"`
	FailedCandidates int64  `json:"failed_candidates"`
	Candidates       int    `json:"candidates"`
	Phase            string `json:"phase"`
	UpdatedTS        int64  `json:"updated_ts"`
	Running          bool   `json:"running"`
}

// Tasks lists tasks with their live phase.
func (h *Harness) Tasks(ctx context.Context) ([]TaskListItem, error) {
	ts, err := h.opt.Core.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TaskListItem, 0, len(ts.Tasks))
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range ts.Tasks {
		it := TaskListItem{TaskID: t.TaskID, Title: t.Title, Status: t.Status, RepairsUsed: t.RepairsUsed, MaxRepairs: t.MaxRepairs,
			FailedCandidates: t.FailedCandidates, Candidates: len(t.Candidates), Phase: phaseFor(t.Status)}
		if r, ok := h.runs[t.TaskID]; ok {
			it.Running = r.active
			if r.active || t.Status == "OPEN" {
				it.Phase = r.Phase
			}
			it.UpdatedTS = r.updated
		}
		out = append(out, it)
	}
	// Newest first (the core lists tasks in id order; reverse for display).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func phaseFor(status string) string {
	switch status {
	case "COMPLETED":
		return "done"
	case "OPEN":
		return "idle"
	default:
		return lower(status)
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// TaskDetail is GET /api/tasks/{id}.
func (h *Harness) TaskDetail(ctx context.Context, taskID string) (map[string]any, error) {
	t, err := h.taskSummary(ctx, taskID)
	if err != nil {
		return nil, err
	}
	list, _ := h.Tasks(ctx)
	var item any
	for _, it := range list {
		if it.TaskID == taskID {
			item = it
		}
	}
	h.mu.Lock()
	var details map[string][]runner.CaseDetail
	var routes []map[string]any
	if r, ok := h.runs[taskID]; ok {
		details = r.details
		routes = append(routes, r.assessments...)
	}
	cands := make([]map[string]any, 0, len(t.Candidates))
	for _, c := range t.Candidates {
		cands = append(cands, map[string]any{
			"proposal_id": c.ProposalID, "candidate_root": c.CandidateRoot, "acceptance": c.Acceptance,
			"details": details[string(c.ProposalID)],
		})
	}
	h.mu.Unlock()
	var report map[string]any
	if raw, err := h.opt.Core.TaskReport(ctx, taskID); err == nil {
		_ = json.Unmarshal(raw, &report)
	}
	assessments := []map[string]any{}
	if as, ok := report["assessments"].([]any); ok {
		for _, a := range as {
			m, _ := a.(map[string]any)
			if m == nil {
				continue
			}
			assessments = append(assessments, map[string]any{
				"assessment_id": m["assessment_id"], "choice": m["choice"], "confidence_bp": m["confidence_bp"],
				"applied_choice": m["applied_choice"], "used_advisor": m["used_advisor"], "fallback_reason": m["fallback_reason"],
				"mode": m["mode"], "model_returned": m["model_returned"],
			})
		}
	}
	actions := []map[string]any{}
	if as, ok := report["actions"].([]any); ok {
		for _, a := range as {
			m, _ := a.(map[string]any)
			if m == nil {
				continue
			}
			actions = append(actions, map[string]any{"action_id": m["action_id"], "tool_id": m["tool_id"], "state": m["state"], "postconditions": m["postconditions"]})
		}
	}
	req := ""
	if t.Requirement != nil {
		req = *t.Requirement
	}
	return map[string]any{
		"task": item, "requirement": req, "entrypoint": t.Entrypoint, "cases": t.Cases, "candidates": cands,
		"assessments": assessments, "routing": routes, "actions": actions, "report": report,
	}, nil
}

// Tree returns a materialized tree (empty root = approved root).
func (h *Harness) Tree(ctx context.Context, root string) (map[string]any, error) {
	t, err := h.opt.Core.ReadTree(ctx, root)
	if err != nil {
		return nil, err
	}
	return map[string]any{"root": t.Root, "files": t.Files}, nil
}

// Diff compares two trees file by file (the UI renders line diffs).
func (h *Harness) Diff(ctx context.Context, from, to string) (map[string]any, error) {
	a, err := h.opt.Core.ReadTree(ctx, from)
	if err != nil {
		return nil, err
	}
	b, err := h.opt.Core.ReadTree(ctx, to)
	if err != nil {
		return nil, err
	}
	var files []map[string]any
	for p, nc := range b.Files {
		oc, ok := a.Files[p]
		switch {
		case !ok:
			files = append(files, map[string]any{"path": p, "status": "added", "old": "", "new": nc})
		case oc != nc:
			files = append(files, map[string]any{"path": p, "status": "modified", "old": oc, "new": nc})
		}
	}
	for p, oc := range a.Files {
		if _, ok := b.Files[p]; !ok {
			files = append(files, map[string]any{"path": p, "status": "removed", "old": oc, "new": ""})
		}
	}
	sort.Slice(files, func(i, j int) bool { return fmt.Sprint(files[i]["path"]) < fmt.Sprint(files[j]["path"]) })
	if files == nil {
		files = []map[string]any{}
	}
	return map[string]any{"from": a.Root, "to": b.Root, "files": files}, nil
}

// Events pages the core's authoritative event log.
func (h *Harness) Events(ctx context.Context, after, limit int64) (map[string]any, error) {
	ev, err := h.opt.Core.EventsSince(ctx, after, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"events": ev.Events, "head_sequence": ev.HeadSequence}, nil
}
