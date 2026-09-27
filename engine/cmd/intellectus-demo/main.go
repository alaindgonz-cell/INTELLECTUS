// Command intellectus-demo runs the illustrative page-size workflow end to
// end against the REAL intellectus-core binary.
//
// Everything model-, worker-, advisor- and external-target-shaped is
// SIMULATED (recorded provider, fake worker, simulated advisor, in-memory
// fake target). No generated code is ever executed; no network is used.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/gateway"
	"github.com/alaindgonz-cell/intellectus/engine/modes"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
	"github.com/alaindgonz-cell/intellectus/engine/scheduler"
)

func main() {
	corePath := flag.String("core", "", "path to the intellectus-core binary (required)")
	deductor := flag.String("deductor", "rust", "deductor spec passed to `serve --deductor` (rust|mojo:<bin>|cross:<bin>)")
	workdir := flag.String("workdir", "intellectus-demo-work", "working directory (database, keys, logs)")
	flag.Parse()
	if *corePath == "" {
		fmt.Fprintln(os.Stderr, "usage: intellectus-demo --core <intellectus-core> [--deductor rust] [--workdir dir]")
		os.Exit(2)
	}
	d := &demo{core: *corePath, deductor: *deductor, workdir: *workdir, out: os.Stdout}
	if err := d.run(context.Background()); err != nil {
		fmt.Fprintf(os.Stdout, "\n[FAILED] %v\n", err)
		logPath := filepath.Join(*workdir, "core.stderr.log")
		if b, rerr := os.ReadFile(logPath); rerr == nil && len(bytes.TrimSpace(b)) > 0 {
			tail := string(bytes.TrimSpace(b))
			if len(tail) > 1500 {
				tail = "…" + tail[len(tail)-1500:]
			}
			fmt.Fprintf(os.Stdout, "         core stderr: %s\n", tail)
		}
		fmt.Fprintf(os.Stdout, "         wire log: %s   core stderr log: %s\n", filepath.Join(*workdir, "wire.jsonl"), logPath)
		os.Exit(1)
	}
}

type demo struct {
	core, deductor, workdir string
	out                     io.Writer

	proc *coreclient.Process
	c    *coreclient.Client
	op   *coreclient.Operator
	seed []byte

	refs map[string]string
}

// ---- trace ------------------------------------------------------------------

func (d *demo) section(title string) {
	fmt.Fprintf(d.out, "\n== %s %s\n", title, strings.Repeat("=", max(0, 72-len(title))))
}

// line prints one labelled trace line. Labels:
//
//	EXECUTED  a command ran against the real core; the result shown is the core's
//	SIMULATED produced by a recorded provider / fake worker / simulated advisor / fake target
//	VERIFIED  checked by the core itself (e.g. replay digest match)
//	DESIGNED  specified but deliberately not enabled in this build
func (d *demo) line(label, format string, args ...any) {
	fmt.Fprintf(d.out, "  %-10s %s\n", "["+label+"]", fmt.Sprintf(format, args...))
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	s := b.String()
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

func codes(rs coreclient.Reasons) string {
	if len(rs) == 0 {
		return "[]"
	}
	return "[" + strings.Join(rs.Codes(), ", ") + "]"
}

// ---- run --------------------------------------------------------------------

func (d *demo) run(ctx context.Context) error {
	d.refs = map[string]string{}
	fmt.Fprintln(d.out, "THE INTELLECTUS — engine demo (page-size workflow)")
	fmt.Fprintln(d.out, "Labels: EXECUTED = ran against the real core; SIMULATED = recorded model / fake worker /")
	fmt.Fprintln(d.out, "        simulated advisor / fake external target; VERIFIED = confirmed by the core;")
	fmt.Fprintln(d.out, "        DESIGNED = specified but not enabled in this build.")

	if err := d.setup(ctx); err != nil {
		return err
	}
	defer func() {
		if d.proc != nil {
			_ = d.proc.Close(5 * time.Second)
		}
	}()
	if err := d.seedKnowledge(ctx); err != nil {
		return err
	}
	res, sessions, err := d.runTask(ctx)
	if err != nil {
		return err
	}
	if err := d.bindingDemos(ctx, res, sessions); err != nil {
		return err
	}
	if err := d.externalEffect(ctx, sessions); err != nil {
		return err
	}
	if err := d.promote(ctx, res, sessions); err != nil {
		return err
	}
	return d.report(ctx)
}

type sessions struct {
	planner, coder, runner, gateway, advisor coreclient.ID
	provider                                 *modes.RecordedProvider
	coderMode                                *modes.Mode
}

// ---- 1. keys, policy, root, init, serve ------------------------------------

func (d *demo) setup(ctx context.Context) error {
	d.section("1. operator key, policy, root, genesis")
	if err := os.MkdirAll(d.workdir, 0o755); err != nil {
		return err
	}
	seed, pub, err := coreclient.GenerateOperatorSeed()
	if err != nil {
		return err
	}
	d.seed = seed
	keyPath := filepath.Join(d.workdir, "operator.key")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		return err
	}
	d.line("EXECUTED", "ed25519 operator key generated (Go crypto/ed25519); seed -> %s; key_id %s…", keyPath, pub[:16])

	policyPath := filepath.Join(d.workdir, "policy.json")
	if err := os.WriteFile(policyPath, []byte(policyJSON), 0o644); err != nil {
		return err
	}
	rootDir := filepath.Join(d.workdir, "root")
	_ = os.RemoveAll(rootDir)
	for p, content := range map[string]string{
		"src/page_size.py":                   stubSrc,
		"tests/acceptance/test_page_size.py": acceptanceTestSrc,
	} {
		full := filepath.Join(rootDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	db := filepath.Join(d.workdir, "intellectus.db")
	for _, f := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
		_ = os.Remove(f) // fresh genesis for every demo run (demo-owned files only)
	}
	out, err := coreclient.RunCLI(ctx, d.core, "init", "--db", db, "--project", projectID,
		"--operator-pubkey", pub, "--policy", policyPath, "--root-dir", rootDir)
	if err != nil {
		return fmt.Errorf("core init: %w", err)
	}
	d.line("EXECUTED", "intellectus-core init -> %s", strings.TrimSpace(string(out)))

	logf, err := os.Create(filepath.Join(d.workdir, "core.stderr.log"))
	if err != nil {
		return err
	}
	d.proc, err = coreclient.Spawn(d.core, []string{"serve", "--db", db, "--deductor", d.deductor}, logf)
	if err != nil {
		return err
	}
	d.c = d.proc.Client
	wire, err := os.Create(filepath.Join(d.workdir, "wire.jsonl"))
	if err != nil {
		return err
	}
	var wmu sync.Mutex
	d.c.SetTrace(func(dir string, line []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		fmt.Fprintf(wire, "%s %s\n", dir, line)
	})
	hello, err := d.c.Hello(ctx)
	if err != nil {
		return fmt.Errorf("core did not answer hello (serve failed to start?): %w", err)
	}
	d.line("EXECUTED", "serve --deductor %s; hello -> %s", d.deductor, compact(hello.Raw))
	d.op, err = coreclient.NewOperator(seed, hello.ProjectID)
	return err
}

// ---- 2. operator knowledge + task -------------------------------------------

func (d *demo) opApply(ctx context.Context, name, op string, params map[string]any) (*coreclient.OperatorResult, error) {
	res, err := d.c.OperatorOp(ctx, d.op, op, params)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", op, err)
	}
	if res.Status != "APPLIED" {
		d.line("EXECUTED", "operator %-24s -> %s %s", op, res.Status, codes(res.Reasons))
		return res, fmt.Errorf("operator %s rejected: %s", op, compact(res.Raw))
	}
	ref := res.Ref()
	if ref != "" {
		d.refs[name] = ref
	}
	d.line("EXECUTED", "operator %-24s -> APPLIED seq=%d %s", op, res.EventSequence, compact(res.Result))
	return res, nil
}

func (d *demo) seedKnowledge(ctx context.Context) error {
	d.section("2. signed operator commands: requirement, tests, environment, AKG, task")
	lit := func(atom string) coreclient.Literal { return coreclient.Literal{Atom: atom, Positive: true} }
	steps := []struct {
		name, op string
		params   func() map[string]any
	}{
		{"R1", "add_requirement", func() map[string]any {
			return map[string]any{"requirement_id": "R1", "text": r1Text, "sensitivity": "internal"}
		}},
		{"T1", "register_test_manifest", func() map[string]any {
			return map[string]any{"manifest_id": "T1", "cases": manifestCases}
		}},
		{"ENV1", "register_environment", func() map[string]any {
			return map[string]any{"environment_id": "ENV1", "description": "fake worker: scripted outcomes, no code execution (SIMULATED)", "worker_kind": "fake"}
		}},
		{"E1", "add_evidence", func() map[string]any {
			return map[string]any{"evidence_id": "E1", "content": "R1 text as captured: " + r1Text, "source_locator": "operator:contract/R1",
				"collection_method": "manual transcription", "sensitivity": "internal"}
		}},
		{"c_lz", "add_claim", func() map[string]any {
			return map[string]any{"claim_id": "c_lz", "proposition": "leading zeros allowed", "context": ctxID, "literal": lit("leading_zeros_ok"),
				"basis": "observed", "support_refs": []string{d.refs["E1"]}, "sensitivity": "internal"}
		}},
		{"c_rng", "add_claim", func() map[string]any {
			return map[string]any{"claim_id": "c_rng", "proposition": "accepted values are 1..100 inclusive", "context": ctxID, "literal": lit("range_1_100"),
				"basis": "observed", "support_refs": []string{d.refs["E1"]}, "sensitivity": "internal"}
		}},
		{"r1", "add_rule", func() map[string]any {
			return map[string]any{"rule_id": "r1", "body": []coreclient.Literal{lit("leading_zeros_ok"), lit("range_1_100")}, "head": lit("accepts_001")}
		}},
		{"c_001", "add_claim", func() map[string]any {
			return map[string]any{"claim_id": "c_001", "proposition": "001 is accepted", "context": ctxID, "literal": lit("accepts_001"),
				"basis": "derived", "support_refs": []string{}, "sensitivity": "internal"}
		}},
		{"d1", "add_derivation", func() map[string]any {
			return map[string]any{"derivation_id": "d1", "conclusion": d.refs["c_001"], "premises": []string{d.refs["c_lz"], d.refs["c_rng"]},
				"rule_id": d.refs["r1"], "context": ctxID}
		}},
		{"task", "open_task", func() map[string]any {
			return map[string]any{"task_id": taskID, "requirement_refs": []string{d.refs["R1"]}, "test_manifest": "T1",
				"environment": "ENV1", "contexts": []string{ctxID}}
		}},
	}
	for _, s := range steps {
		if _, err := d.opApply(ctx, s.name, s.op, s.params()); err != nil {
			return err
		}
	}
	return nil
}

// ---- 3. scheduler: plan -> A0 -> FAIL -> route -> A1 -> PASS -> admit -------

func (d *demo) openSession(ctx context.Context, role, label string) (coreclient.ID, error) {
	s, err := d.c.OpenSession(ctx, role, label)
	if err != nil {
		return "", fmt.Errorf("open_session %s: %w", role, err)
	}
	d.line("EXECUTED", "open_session %-8s -> %s principal=%s", role, s.SessionID, s.Principal)
	return s.SessionID, nil
}

func candidateEnvelope(base, src, rationale string) string {
	b, _ := json.Marshal(map[string]any{"kind": "candidate", "task_id": taskID, "base_root": base,
		"files": map[string]string{target: src}, "deletions": []string{}, "rationale": rationale})
	return string(b)
}

func actionEnvelope(tool string, args map[string]any, premises []string, key string, post []string) string {
	b, _ := json.Marshal(map[string]any{"kind": "action", "task_id": taskID, "tool_id": tool, "arguments": args,
		"premise_refs": premises, "idempotency_key": key, "expected_postconditions": post})
	return string(b)
}

// recordedModel is the SIMULATED model: scripted text keyed by phase. It
// only templates core-issued values (roots, keys, refs) into fixed text.
func recordedModel(r modes.Request) (string, error) {
	premises := strings.Split(r.Vars["requirement_refs"], ",")
	switch {
	case r.Role == "planner":
		b, _ := json.Marshal(map[string]any{"kind": "plan", "task_id": taskID,
			"summary":      "Implement parse_page_size strictly per R1 and satisfy the protected acceptance manifest T1.",
			"steps":        []string{"validate type", "validate ASCII-digit syntax (1-3 chars)", "check range 1..100", "return int"},
			"premise_refs": premises})
		return "Here is my plan (the core extracts the fenced block):\n```json\n" + string(b) + "\n```\nLet me know if anything is unclear.", nil
	case r.Phase == "initial":
		return candidateEnvelope(r.Vars["base_root"], a0Src, "straightforward int() conversion with range check"), nil
	case r.Phase == "repair":
		return candidateEnvelope(r.Vars["base_root"], a1Src, "reject non-ASCII-digit syntax with an anchored regex before int()"), nil
	case r.Phase == "action":
		return actionEnvelope(r.Vars["tool_id"], map[string]any{"candidate_root": r.Vars["candidate_root"]}, premises,
			r.Vars["idempotency_key"], []string{"approved_root == candidate_root"}), nil
	case r.Phase == "scripted":
		return r.Vars["text"], nil
	}
	return "", fmt.Errorf("no recording for role=%s phase=%s", r.Role, r.Phase)
}

func (d *demo) runTask(ctx context.Context) (*scheduler.Result, *sessions, error) {
	d.section("3. sessions and the task loop (scheduler)")
	s := &sessions{}
	var err error
	if s.planner, err = d.openSession(ctx, "planner", "recorded"); err != nil {
		return nil, nil, err
	}
	if s.coder, err = d.openSession(ctx, "coder", "recorded"); err != nil {
		return nil, nil, err
	}
	if s.runner, err = d.openSession(ctx, "runner", "fake"); err != nil {
		return nil, nil, err
	}
	if s.gateway, err = d.openSession(ctx, "gateway", "fake-target"); err != nil {
		return nil, nil, err
	}
	if s.advisor, err = d.openSession(ctx, "advisor", "simulated"); err != nil {
		return nil, nil, err
	}

	s.provider = modes.NewRecordedProvider()
	s.provider.Func = recordedModel
	s.coderMode = modes.NewCoder(d.c, s.coder, s.provider)
	s.coderMode.Budget = &modes.Budget{MaxCalls: 12}

	worker := runner.NewFakeWorker(target, map[string]map[string]string{
		runner.ContentKey(a0Src): script("plus", "space", "arabic_indic"),
		runner.ContentKey(a1Src): script(),
		runner.ContentKey(a2Src): script("over"),
	})
	d.line("SIMULATED", "fake worker (worker_kind=fake, implementation_digest=%s): outcomes scripted by sha256 of %s; code is NEVER executed", worker.ImplementationDigest(), target)
	if _, err := (runner.SandboxWorker{}).Run(ctx, coreclient.CheckRequest{}, nil); err != nil {
		d.line("DESIGNED", "SandboxWorker refuses: %v", err)
	}
	adv := &advisor.SimulatedAdvisor{Script: []advisor.Advice{{
		Choice: coreclient.RouteGatherContext, ConfidenceBP: 9100,
		ProbabilitiesBP: map[string]int64{"GATHER_CONTEXT": 9100, "REPAIR": 700, "REPLAN": 100, "ESCALATE": 100, "STOP": 0},
		Usage:           map[string]int64{"input_tokens": 0, "output_tokens": 0},
	}}}

	approver := scheduler.ApproverFunc(func(ctx context.Context, _, digest, tool string) error {
		_, err := d.approve(ctx, digest, tool)
		return err
	})
	sch := scheduler.New(scheduler.Config{
		Core: d.c, TaskID: taskID,
		Planner:    modes.NewPlanner(d.c, s.planner, s.provider),
		Coder:      s.coderMode,
		Runner:     &runner.ProtectedRunner{Core: d.c, Session: s.runner, Worker: worker},
		Router:     &advisor.ShadowRunner{Core: d.c, Session: s.advisor, Advisor: adv, Mode: advisor.ModeShadow, Timeout: 2 * time.Second},
		Contexts:   []string{ctxID},
		ActionTool: "promote_local",
		Approver:   approver,
		Log: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			label := "EXECUTED"
			switch {
			case strings.HasPrefix(msg, "advisor"):
				label = "SIMULATED"
			case strings.HasPrefix(msg, "runner:"):
				msg += "  <- worker SIMULATED; decision by core"
			case strings.HasPrefix(msg, "planner:") || strings.HasPrefix(msg, "coder["):
				msg += "  <- model text SIMULATED (recorded), passed to submit unmodified"
			}
			d.line(label, "%s", msg)
		},
	})
	res, err := sch.Run(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: %w", err)
	}
	d.line("EXECUTED", "scheduler outcome: %s (passing proposal %s, candidate_root %s)", res.Outcome, res.PassingProposal, res.CandidateRoot)
	if res.Outcome != scheduler.OutcomeAuthorized {
		return res, s, fmt.Errorf("expected AUTHORIZED, got %s %s", res.Outcome, codes(res.Reasons))
	}
	return res, s, nil
}

func (d *demo) approve(ctx context.Context, digest, tool string) (*coreclient.OperatorResult, error) {
	exp := time.Now().Add(time.Hour).Unix()
	res, err := d.opApply(ctx, "approval", "approve", map[string]any{"action_digest": digest, "tool_id": tool, "expires_at": exp})
	if err == nil {
		d.line("EXECUTED", "  (operator approved exact action_digest %s for %s, expires_at now+3600)", digest, tool)
	}
	return res, err
}

// ---- 4. binding and idempotency rejections ----------------------------------

func (d *demo) submitScripted(ctx context.Context, s *sessions, text string) (*coreclient.SubmitResult, error) {
	inv, err := s.coderMode.Invoke(ctx, modes.Request{TaskID: taskID, Phase: "scripted", Vars: map[string]string{"text": text}})
	if err != nil {
		return nil, err
	}
	return inv.Submit, nil
}

func (d *demo) bindingDemos(ctx context.Context, res *scheduler.Result, s *sessions) error {
	d.section("4. artifact binding and idempotency")
	view, err := d.c.View(ctx, s.coder, taskID)
	if err != nil {
		return err
	}
	base := view.ApprovedRoot.Digest
	premises := view.Task.RequirementRefs

	sub, err := d.submitScripted(ctx, s, candidateEnvelope(base, a2Src, "raise the upper bound to 200"))
	if err != nil {
		return err
	}
	d.line("SIMULATED", "coder submits A2 (upper bound 200) -> %s %s root=%s %s", sub.Status, sub.ProposalID, sub.CandidateRoot, codes(sub.Reasons))
	if sub.Status != "RECORDED" {
		return fmt.Errorf("A2 not recorded: %s", compact(sub.Raw))
	}
	a2Root := sub.CandidateRoot

	act, err := d.submitScripted(ctx, s, actionEnvelope("promote_local", map[string]any{"candidate_root": a2Root}, premises,
		"idem:demo:A2:promote_local", []string{"approved_root == candidate_root"}))
	if err != nil {
		return err
	}
	d.line("SIMULATED", "coder proposes promote_local(A2 root) -> %s %s", act.Status, act.ProposalID)
	adm, err := d.c.Admit(ctx, act.ProposalID)
	if err != nil {
		return err
	}
	d.line("EXECUTED", "admit -> %s reasons=%s", adm.Status, codes(adm.Reasons))
	for _, r := range adm.Reasons {
		if r.Code == "ARTIFACT_BINDING_MISMATCH" {
			d.line("EXECUTED", "  ARTIFACT_BINDING_MISMATCH: %s", compact(r.Raw))
		}
	}
	if !adm.Reasons.Has("ARTIFACT_BINDING_MISMATCH") {
		d.line("EXECUTED", "  NOTE: expected ARTIFACT_BINDING_MISMATCH, core answered %s", compact(adm.Raw))
	}

	key := res.ActionProposal.Request.Vars["idempotency_key"]
	conf, err := d.submitScripted(ctx, s, actionEnvelope("promote_local", map[string]any{"candidate_root": a2Root}, premises,
		key, []string{"approved_root == candidate_root"}))
	if err != nil {
		return err
	}
	d.line("SIMULATED", "coder reuses A1's idempotency key %q with the A2 root -> %s %s", key, conf.Status, conf.ProposalID)
	if conf.Status == "RECORDED" {
		adm2, err := d.c.Admit(ctx, conf.ProposalID)
		if err != nil {
			return err
		}
		d.line("EXECUTED", "admit -> %s reasons=%s", adm2.Status, codes(adm2.Reasons))
	} else {
		d.line("EXECUTED", "submit rejected: %s", codes(conf.Reasons))
	}
	return nil
}

// ---- 5a. external effect: crash after apply -> unknown -> reconcile ---------

func (d *demo) externalEffect(ctx context.Context, s *sessions) error {
	d.section("5a. external tool export_view: crash after effect -> OUTCOME_UNKNOWN -> reconcile")
	d.line("EXECUTED", "(runs before the promotion: promoting completes the task, after which the core accepts no new proposals)")
	view, err := d.c.View(ctx, s.coder, taskID)
	if err != nil {
		return err
	}
	root := view.ApprovedRoot.Digest
	sub, err := d.submitScripted(ctx, s, actionEnvelope("export_view", map[string]any{"approved_root": root}, view.Task.RequirementRefs,
		"idem:demo:export_view:1", []string{"observation.approved_root == arguments.approved_root"}))
	if err != nil {
		return err
	}
	d.line("SIMULATED", "coder proposes export_view(approved_root=%s) -> %s %s", root, sub.Status, sub.ProposalID)
	if sub.Status != "RECORDED" {
		return fmt.Errorf("export action not recorded: %s", compact(sub.Raw))
	}
	adm, err := d.c.Admit(ctx, sub.ProposalID)
	if err != nil {
		return err
	}
	d.line("EXECUTED", "admit -> %s action_digest=%s reasons=%s", adm.Status, adm.ActionDigest, codes(adm.Reasons))
	if adm.Status == "REJECTED" && adm.Reasons.Has("MISSING_APPROVAL") {
		if _, err := d.approve(ctx, adm.ActionDigest, "export_view"); err != nil {
			return err
		}
		if adm, err = d.c.Admit(ctx, sub.ProposalID); err != nil {
			return err
		}
		d.line("EXECUTED", "admit -> %s action_id=%s", adm.Status, adm.ActionID)
	}
	if adm.Status != "AUTHORIZED" {
		return fmt.Errorf("export_view not authorized: %s", compact(adm.Raw))
	}

	target := gateway.NewFakeTargetAdapter()
	target.SetFailMode(gateway.FailCrashAfterEffect)
	adapters := map[string]gateway.Adapter{"export_view": target}
	disp := &gateway.Dispatcher{Core: d.c, Session: s.gateway, Adapters: adapters}
	dr, err := disp.Dispatch(ctx, adm.ActionID)
	if err != nil {
		return fmt.Errorf("dispatch export_view: %w", err)
	}
	d.line("EXECUTED", "dispatch_begin -> %s attempt=%s tool=%s key=%s", dr.Begin.Status, dr.Begin.AttemptID, dr.Begin.ToolID, dr.Begin.IdempotencyKey)
	d.line("SIMULATED", "fake target applied the effect, then the connection 'crashed': %v", dr.ExecErr)
	if dr.Outcome != nil {
		d.line("EXECUTED", "record_outcome_unknown -> %s  (failure is never assumed)", dr.Outcome.Status)
	}

	rec := &gateway.Reconciler{Core: d.c, Session: s.gateway, Adapters: adapters}
	rs, err := rec.RunOnce(ctx)
	if err != nil {
		return err
	}
	for _, r := range rs {
		obs := "null"
		if r.Finding.EffectObserved != nil {
			obs = fmt.Sprint(*r.Finding.EffectObserved)
		}
		d.line("SIMULATED", "reconcile read of fake target by idempotency key %s -> effect_observed=%s", r.Action.IdempotencyKey, obs)
		d.line("EXECUTED", "record_reconciliation %s -> dispatch_state=%s requires_human_review=%v", r.Action.ActionID, r.Result.DispatchState, r.Result.RequiresHumanReview)
	}
	d.line("EXECUTED", "adapter Execute calls = %d (reconciler never re-executes)", target.ExecuteCount())
	if target.ExecuteCount() != 1 {
		return errors.New("adapter executed more than once")
	}
	return nil
}

// ---- 5b. promotion ----------------------------------------------------------

func (d *demo) promote(ctx context.Context, res *scheduler.Result, s *sessions) error {
	d.section("5b. dispatch the authorized promote_local (A1)")
	last := res.Admits[len(res.Admits)-1]
	disp := &gateway.Dispatcher{Core: d.c, Session: s.gateway}
	dr, err := disp.Dispatch(ctx, last.ActionID)
	if err != nil {
		return err
	}
	d.line("EXECUTED", "dispatch_begin %s -> %s %s", last.ActionID, dr.Begin.Status, compact(dr.Begin.Result))
	if dr.Begin.Status != "COMPLETED" {
		return fmt.Errorf("promotion not completed: %s", compact(dr.Begin.Raw))
	}
	view, err := d.c.View(ctx, s.coder, taskID)
	if err != nil {
		return err
	}
	d.line("EXECUTED", "approved_root is now %s (A1 candidate_root %s); task status %s", view.ApprovedRoot.Digest, res.CandidateRoot, view.Task.Status)
	return nil
}

// ---- 6. report and replay ---------------------------------------------------

func (d *demo) report(ctx context.Context) error {
	d.section("6. task report and replay")
	raw, err := d.c.TaskReport(ctx, taskID)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, raw, "", "  ")
	reportPath := filepath.Join(d.workdir, "task_report.json")
	_ = os.WriteFile(reportPath, pretty.Bytes(), 0o644)
	var rep struct {
		Task struct {
			Status      string `json:"status"`
			RepairsUsed int    `json:"repairs_used"`
			CompletedBy string `json:"completed_by"`
		} `json:"task"`
		ApprovedRoot  string `json:"approved_root"`
		Verifications []struct {
			ID     string   `json:"verification_id"`
			Kind   string   `json:"check_kind"`
			Result string   `json:"result"`
			Issuer string   `json:"issuer"`
			Failed []string `json:"failed_cases"`
			Limits []string `json:"applicability_limits"`
			Used   bool     `json:"used_for_authorization"`
		} `json:"verifications"`
		Assessments []struct {
			Choice   *string `json:"choice"`
			Applied  string  `json:"applied_choice"`
			Used     bool    `json:"used_advisor"`
			Mode     string  `json:"mode"`
			Conf     *int64  `json:"confidence_bp"`
			Fallback *string `json:"fallback_reason"`
		} `json:"assessments"`
		Rejections []struct {
			Proposal string             `json:"proposal"`
			Reasons  coreclient.Reasons `json:"reasons"`
		} `json:"admission_rejections"`
		Limitations []string `json:"limitations"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		d.line("EXECUTED", "task_report (unparsed): %s", compact(raw))
	} else {
		d.line("EXECUTED", "task_report: status=%s completed_by=%s repairs_used=%d approved_root=%s", rep.Task.Status, rep.Task.CompletedBy, rep.Task.RepairsUsed, rep.ApprovedRoot)
		for _, v := range rep.Verifications {
			label := "EXECUTED"
			for _, l := range v.Limits {
				if strings.HasPrefix(l, "SIMULATED") {
					label = "SIMULATED"
				}
			}
			d.line(label, "  verification %s %s=%s issuer=%s failed=%v used_for_authorization=%v", v.ID, v.Kind, v.Result, v.Issuer, v.Failed, v.Used)
		}
		for _, a := range rep.Assessments {
			ch, fb, conf := "null", "null", int64(0)
			if a.Choice != nil {
				ch = *a.Choice
			}
			if a.Fallback != nil {
				fb = *a.Fallback
			}
			if a.Conf != nil {
				conf = *a.Conf
			}
			d.line("SIMULATED", "  assessment mode=%s advisor_choice=%s confidence=%dbp used_advisor=%v applied=%s fallback=%s", a.Mode, ch, conf, a.Used, a.Applied, fb)
		}
		for _, r := range rep.Rejections {
			d.line("EXECUTED", "  admission rejection %s %s", r.Proposal, codes(r.Reasons))
		}
		for _, l := range rep.Limitations {
			d.line("EXECUTED", "  limitation: %s", l)
		}
		d.line("EXECUTED", "  full report -> %s", reportPath)
	}

	rv, err := d.c.ReplayVerify(ctx)
	if err != nil {
		return err
	}
	label := "EXECUTED"
	if rv.Matches {
		label = "VERIFIED"
	}
	d.line(label, "replay_verify -> events=%d state_digest=%s matches=%v", rv.Events, rv.StateDigest, rv.Matches)

	if _, err := d.opApply(ctx, "shutdown", "shutdown", map[string]any{}); err != nil {
		return err
	}
	if err := d.proc.Close(5 * time.Second); err != nil {
		d.line("EXECUTED", "core exited: %v", err)
	}
	d.proc = nil
	out, err := coreclient.RunCLI(ctx, d.core, "replay", "--db", filepath.Join(d.workdir, "intellectus.db"))
	if err != nil {
		return fmt.Errorf("offline replay: %w", err)
	}
	d.line("VERIFIED", "intellectus-core replay (offline, from genesis) -> %s", strings.TrimSpace(string(out)))
	if !rv.Matches {
		return errors.New("replay mismatch")
	}
	fmt.Fprintln(d.out, "\nDone. SIMULATED components: recorded model provider, fake worker, simulated advisor (SHADOW), fake external target.")
	return nil
}
