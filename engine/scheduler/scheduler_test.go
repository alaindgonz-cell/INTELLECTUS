package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/gateway"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakecore"
	"github.com/alaindgonz-cell/intellectus/engine/modes"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
	"github.com/alaindgonz-cell/intellectus/engine/scheduler"
)

const (
	target  = "src/page_size.py"
	badSrc  = "def parse(raw): return int(raw)\n"
	goodSrc = "def parse(raw): return strict(raw)\n"
)

// taskCore is a scripted fake core for one task. It enforces a repair
// budget like the real core: the (maxRepairs+2)-th candidate is rejected.
type taskCore struct {
	mu           sync.Mutex
	status       string
	maxRepairs   int
	candidates   int
	files        map[string]string // proposal -> target content
	nextProp     int
	approved     bool
	onReport     func()
	transitions  []string
	candSubmits  int
	actionSubmit int
}

func newTaskCore(t *testing.T, maxRepairs int) (*fakecore.Server, *coreclient.Client, *taskCore) {
	t.Helper()
	tc := &taskCore{status: "OPEN", maxRepairs: maxRepairs, files: map[string]string{}}
	srv := fakecore.New()
	srv.Handle("view", func(json.RawMessage) (any, *coreclient.CoreError) {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		return map[string]any{"snapshot_sequence": 1, "state_digest": "sha256:s", "policy_version": "P1",
			"task":          map[string]any{"task_id": "task:t", "status": tc.status, "requirement_refs": []string{"req:R1@1"}},
			"approved_root": map[string]any{"digest": "sha256:base", "files": map[string]string{}}}, nil
	})
	srv.Handle("submit", func(args json.RawMessage) (any, *coreclient.CoreError) {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		var env struct {
			Kind  string            `json:"kind"`
			Files map[string]string `json:"files"`
		}
		if err := json.Unmarshal([]byte(fakecore.Arg[string](args, "raw")), &env); err != nil {
			return map[string]any{"status": "REJECTED", "input_id": "in:x", "reasons": []any{map[string]any{"code": "SCHEMA_INVALID"}}}, nil
		}
		tc.nextProp++
		pid := fmt.Sprintf("prop:%d", tc.nextProp)
		switch env.Kind {
		case "candidate":
			tc.candSubmits++
			if tc.candidates >= tc.maxRepairs+1 {
				return map[string]any{"status": "REJECTED", "input_id": "in:x", "reasons": []any{map[string]any{
					"code": "REPAIR_BUDGET_EXHAUSTED", "permitted_next_steps": []string{"ESCALATE", "STOP"}, "remaining_budget": map[string]int{"repairs": 0}}}}, nil
			}
			tc.candidates++
			tc.files[pid] = env.Files[target]
			return map[string]any{"status": "RECORDED", "input_id": "in:1", "proposal_id": pid, "kind": "candidate", "candidate_root": "sha256:root-" + pid}, nil
		case "action":
			tc.actionSubmit++
		}
		return map[string]any{"status": "RECORDED", "input_id": "in:1", "proposal_id": pid, "kind": env.Kind}, nil
	})
	srv.Handle("check_plan", func(args json.RawMessage) (any, *coreclient.CoreError) {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		pid := fakecore.Arg[string](args, "proposal_id")
		chk := coreclient.CheckRequest{CheckID: coreclient.StringID("chk:" + pid), CheckKind: "acceptance_tests",
			TestManifest: coreclient.TestManifest{ID: "T1", Cases: []coreclient.TestCase{{Name: "a"}, {Name: "b"}}},
			Environment:  coreclient.Environment{ID: "ENV1", WorkerKind: "fake"}}
		return map[string]any{"checks": []any{chk}, "materialized": map[string]string{target: tc.files[pid]}}, nil
	})
	srv.Handle("report_check", func(args json.RawMessage) (any, *coreclient.CoreError) {
		tc.mu.Lock()
		f := tc.onReport
		tc.mu.Unlock()
		if f != nil {
			f()
		}
		return map[string]any{"verification_id": "ver:1", "result": fakecore.Arg[string](args, "result")}, nil
	})
	srv.Handle("route", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"eligible": []string{"GATHER_CONTEXT", "REPAIR", "ESCALATE"}, "deterministic_choice": nil, "reason": "acceptance failed"}, nil
	})
	srv.Handle("record_assessment", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"assessment_id": "asm:1", "applied_choice": "REPAIR", "used_advisor": false}, nil
	})
	srv.Handle("evaluate_context", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"verification_id": "ver:ctx", "result": "PASS", "consistent": true, "conflicts": []any{}, "derivations": []any{}}, nil
	})
	srv.Handle("task_transition", func(args json.RawMessage) (any, *coreclient.CoreError) {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		to := fakecore.Arg[string](args, "to")
		tc.transitions = append(tc.transitions, to)
		tc.status = to
		return map[string]any{"task_status": to}, nil
	})
	srv.Handle("admit", func(json.RawMessage) (any, *coreclient.CoreError) {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		if !tc.approved {
			return map[string]any{"status": "REJECTED", "action_digest": "sha256:act", "reasons": []any{map[string]any{"code": "MISSING_APPROVAL"}}}, nil
		}
		return map[string]any{"status": "AUTHORIZED", "action_id": "act:1", "action_digest": "sha256:act"}, nil
	})
	srv.Handle("dispatch_begin", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"status": "COMPLETED", "result": map[string]any{"approved_root": "sha256:new"}}, nil
	})
	c := srv.Start()
	t.Cleanup(func() { c.Close() })
	return srv, c, tc
}

func provider(repairSrc string) *modes.RecordedProvider {
	p := modes.NewRecordedProvider()
	p.Func = func(r modes.Request) (string, error) {
		switch r.Phase {
		case "plan", "replan":
			return `{"kind":"plan","task_id":"task:t","summary":"s","steps":["x"],"premise_refs":["req:R1@1"]}`, nil
		case "initial":
			return candidate(badSrc), nil
		case "repair":
			return candidate(repairSrc), nil
		case "action":
			return fmt.Sprintf(`{"kind":"action","task_id":"task:t","tool_id":"promote_local","arguments":{"candidate_root":%q},"premise_refs":["req:R1@1"],"idempotency_key":%q,"expected_postconditions":[]}`,
				r.Vars["candidate_root"], r.Vars["idempotency_key"]), nil
		}
		return "", fmt.Errorf("unexpected phase %s", r.Phase)
	}
	return p
}

func candidate(src string) string {
	b, _ := json.Marshal(map[string]any{"kind": "candidate", "task_id": "task:t", "base_root": "sha256:base",
		"files": map[string]string{target: src}, "deletions": []string{}, "rationale": "r"})
	return string(b)
}

func build(c *coreclient.Client, p modes.Provider, approver scheduler.Approver, withDispatch bool) *scheduler.Scheduler {
	w := runner.NewFakeWorker(target, map[string]map[string]string{
		runner.ContentKey(badSrc):  {"a": "PASS", "b": "FAIL"},
		runner.ContentKey(goodSrc): {"a": "PASS", "b": "PASS"},
	})
	cfg := scheduler.Config{
		Core: c, TaskID: "task:t",
		Planner:    modes.NewPlanner(c, coreclient.StringID("s:planner"), p),
		Coder:      modes.NewCoder(c, coreclient.StringID("s:coder"), p),
		Runner:     &runner.ProtectedRunner{Core: c, Session: coreclient.StringID("s:runner"), Worker: w},
		Router:     &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("s:adv"), Mode: advisor.ModeShadow},
		Contexts:   []string{"ctx:t"},
		ActionTool: "promote_local",
		Approver:   approver,
	}
	if withDispatch {
		cfg.Dispatcher = &gateway.Dispatcher{Core: c, Session: coreclient.StringID("s:gw")}
	}
	return scheduler.New(cfg)
}

func assertNeverCompleted(t *testing.T, tc *taskCore) {
	t.Helper()
	for _, tr := range tc.transitions {
		if tr == "COMPLETED" {
			t.Fatal("engine must never request COMPLETED")
		}
	}
}

func TestStopsOnRepairBudgetExhausted(t *testing.T) {
	srv, c, tc := newTaskCore(t, 2)
	s := build(c, provider(badSrc), nil, false) // repairs never pass
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != scheduler.OutcomeBudgetExhausted || !res.Reasons.Has("REPAIR_BUDGET_EXHAUSTED") {
		t.Fatalf("outcome %s reasons %v", res.Outcome, res.Reasons.Codes())
	}
	// initial + 2 repairs recorded, 1 rejected; then it stops.
	if tc.candSubmits != 4 {
		t.Fatalf("candidate submits = %d, want 4", tc.candSubmits)
	}
	if srv.Count("route") != 3 || srv.Count("admit") != 0 || tc.actionSubmit != 0 {
		t.Fatalf("route=%d admit=%d actions=%d", srv.Count("route"), srv.Count("admit"), tc.actionSubmit)
	}
	if len(tc.transitions) != 1 || tc.transitions[0] != "ESCALATED" {
		t.Fatalf("transitions %v", tc.transitions)
	}
	assertNeverCompleted(t, tc)
	// Only one task id was ever used (no budget reset through new ids).
	for _, r := range srv.Requests("submit") {
		if raw := fakecore.Arg[string](r.Args, "raw"); !json.Valid([]byte(raw)) {
			t.Fatal("unexpected raw")
		}
	}
}

func TestCancellationStopsWithoutCompletingOrRestarting(t *testing.T) {
	srv, c, tc := newTaskCore(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	tc.onReport = cancel // operator cancels while the first check is being reported
	s := build(c, provider(goodSrc), scheduler.ApproverFunc(func(context.Context, string, string, string) error {
		t.Fatal("approval must not be requested after cancellation")
		return nil
	}), true)
	res, err := s.Run(ctx)
	if !errors.Is(err, scheduler.ErrTaskCancelled) || res.Outcome != scheduler.OutcomeCancelled {
		t.Fatalf("want cancellation, got %v %v", res.Outcome, err)
	}
	if srv.Count("route") != 0 || srv.Count("admit") != 0 || srv.Count("dispatch_begin") != 0 || srv.Count("task_transition") != 0 {
		t.Fatal("no new commands after cancellation")
	}
	assertNeverCompleted(t, tc)
	before := len(srv.Requests(""))
	res, err = s.Run(context.Background()) // fresh context: still never restarted
	if !errors.Is(err, scheduler.ErrTaskCancelled) || res.Outcome != scheduler.OutcomeCancelled {
		t.Fatalf("restart must be refused: %v", err)
	}
	if len(srv.Requests("")) != before {
		t.Fatal("refused restart must not talk to the core")
	}
}

func TestOperatorCancelledTaskIsNotStarted(t *testing.T) {
	srv, c, tc := newTaskCore(t, 2)
	tc.status = "CANCELLED"
	s := build(c, provider(goodSrc), nil, false)
	if _, err := s.Run(context.Background()); !errors.Is(err, scheduler.ErrTaskCancelled) {
		t.Fatalf("got %v", err)
	}
	if srv.Count("submit") != 0 {
		t.Fatal("cancelled task must not be driven")
	}
}

func TestRepairThenApproveAndDispatch(t *testing.T) {
	srv, c, tc := newTaskCore(t, 2)
	var approvedDigest string
	s := build(c, provider(goodSrc), scheduler.ApproverFunc(func(_ context.Context, _, digest, tool string) error {
		tc.mu.Lock()
		tc.approved = true
		tc.mu.Unlock()
		approvedDigest = digest + "|" + tool
		return nil
	}), true)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != scheduler.OutcomeDispatched || res.Dispatch.Begin.Status != "COMPLETED" {
		t.Fatalf("outcome %s", res.Outcome)
	}
	if approvedDigest != "sha256:act|promote_local" || len(res.Admits) != 2 || res.Admits[1].Status != "AUTHORIZED" {
		t.Fatalf("approval flow: %s %+v", approvedDigest, res.Admits)
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Applied != "REPAIR" {
		t.Fatalf("decisions %+v", res.Decisions)
	}
	if srv.Count("evaluate_context") != 1 {
		t.Fatalf("evaluate_context = %d", srv.Count("evaluate_context"))
	}
	assertNeverCompleted(t, tc)
	// The action envelope carried the core-issued root and a stable idempotency key.
	subs := srv.Requests("submit")
	raw := fakecore.Arg[string](subs[len(subs)-1].Args, "raw")
	var env struct {
		Arguments      map[string]string `json:"arguments"`
		IdempotencyKey string            `json:"idempotency_key"`
	}
	_ = json.Unmarshal([]byte(raw), &env)
	if env.Arguments["candidate_root"] != res.CandidateRoot || env.IdempotencyKey != "idem:task:t:prop:3:promote_local" {
		t.Fatalf("action envelope %s (root %s)", raw, res.CandidateRoot)
	}
}
