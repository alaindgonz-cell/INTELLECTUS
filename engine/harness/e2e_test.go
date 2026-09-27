package harness_test

// End-to-end: the real Rust core, the real bubblewrap sandbox (candidate
// code really executes), the real Claude and Jev clients — pointed at local
// fake HTTP APIs that play the model roles. Skipped when the core binary or
// the sandbox is unavailable.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/claude"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/harness"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakeapi"
	"github.com/alaindgonz-cell/intellectus/engine/jev"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
	"github.com/alaindgonz-cell/intellectus/engine/sandbox"
)

const a0 = "def parse_page_size(raw):\n    return int(raw)\n"

const a1 = `import re

def parse_page_size(raw):
    if not isinstance(raw, str):
        raise ValueError("page size must be a string")
    if re.fullmatch(r"[0-9]{1,3}", raw) is None:
        raise ValueError("invalid page size syntax")
    value = int(raw)
    if not 1 <= value <= 100:
        raise ValueError("page size out of range")
    return value
`

func coreBinary(t *testing.T) string {
	for _, p := range []string{os.Getenv("INTELLECTUS_CORE"), "../../core/target/debug/intellectus-core", "../../core/target/release/intellectus-core"} {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Skip("intellectus-core binary not built (cd core && cargo build)")
	return ""
}

func proposal() map[string]any {
	c := func(name, in, expect, val, exc string) map[string]any {
		return map[string]any{"name": name, "input_json": in, "expect": expect, "value_json": val, "exception": exc}
	}
	return map[string]any{
		"title":       "Strict page-size parsing",
		"requirement": "parse_page_size(raw) accepts only strings of one to three ASCII digits whose value is 1..100 (leading zeros allowed) and returns the int; anything else raises ValueError.",
		"path":        "src/page_size.py",
		"function":    "parse_page_size",
		"cases": []any{
			c("ok_1", `"1"`, "returns", "1", ""),
			c("ok_100", `"100"`, "returns", "100", ""),
			c("ok_001", `"001"`, "returns", "1", ""),
			c("zero", `"0"`, "raises", "", "ValueError"),
			c("over", `"101"`, "raises", "", "ValueError"),
			c("plus", `"+1"`, "raises", "", "ValueError"),
			c("space", `" 5"`, "raises", "", "ValueError"),
			c("none", `null`, "raises", "", "ValueError"),
		},
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEndChatToPromotionAndExport(t *testing.T) {
	core := coreBinary(t)
	worker, err := sandbox.New(sandbox.Config{})
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary.txt")
	if err := os.WriteFile(canary, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("E2E_CANARY", "shh")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st, err := worker.SelfTest(ctx, sandbox.SelfTestConfig{ForbiddenPaths: []string{canary}, CanaryEnv: []string{"E2E_CANARY"}, BlockedAddrs: []string{"1.1.1.1:443"}})
	if err != nil || !st.OK {
		t.Fatalf("sandbox self-test failed: %v %+v", err, st.Probes)
	}

	// Genesis: operator key, policy trusting this sandbox, Jev LIVE.
	seed, pub, err := coreclient.GenerateOperatorSeed()
	if err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{
		"version": "P1",
		"tools": map[string]any{
			"promote_local": map[string]any{"effect": "internal", "requires_approval": true, "required_checks": []string{"acceptance_tests", "formal_context"}, "idempotent": true},
			"export_view":   map[string]any{"effect": "external", "requires_approval": true, "required_checks": []string{}, "idempotent": true},
		},
		"trusted_issuers": map[string]any{"acceptance_tests": []any{map[string]string{"principal": "runner:" + worker.Label(), "implementation_digest": worker.ImplementationDigest()}}},
		"protected_paths": []string{"tests/acceptance/"},
		"max_repairs":     3,
		"role_clearance":  map[string]string{"planner": "internal", "coder": "internal", "tester": "internal", "runner": "internal", "gateway": "internal", "intake": "internal", "scheduler": "internal", "advisor": "public"},
		"jev":             map[string]any{"mode": "LIVE", "min_confidence_bp": 8000},
	}
	pb, _ := json.Marshal(policy)
	must(t, os.WriteFile(filepath.Join(dir, "policy.json"), pb, 0o600))
	root := filepath.Join(dir, "root")
	must(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# demo\n"), 0o644))
	db := filepath.Join(dir, "intellectus.db")
	if out, err := coreclient.RunCLI(ctx, core, "init", "--db", db, "--project", "e2e", "--operator-pubkey", pub, "--policy", filepath.Join(dir, "policy.json"), "--root-dir", root); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	proc, err := coreclient.Spawn(core, []string{"serve", "--db", db}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close(5 * time.Second)
	op, err := coreclient.NewOperator(seed, "e2e")
	must(t, err)

	// Fake Claude: route by role system prompt.
	var coderCalls atomic.Int32
	anthropic := fakeapi.NewAnthropic(func(r fakeapi.AnthropicRequest) fakeapi.AnthropicReply {
		sys := r.SystemText()
		switch {
		case strings.Contains(sys, "intake assistant"):
			return fakeapi.AnthropicReply{Text: "Here is a task proposal.", ToolName: "propose_task", ToolInput: proposal()}
		case strings.Contains(sys, "You are the Planner"):
			return fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: map[string]any{"summary": "Validate syntax, then range.", "steps": []string{"regex fullmatch", "range check"}}}
		case strings.Contains(sys, "You are the Coder"):
			code := a0
			if coderCalls.Add(1) > 1 {
				code = a1
			}
			return fakeapi.AnthropicReply{ToolName: "submit_candidate", ToolInput: map[string]any{
				"files": []any{map[string]any{"path": "src/page_size.py", "content": code}}, "rationale": "implementation"}}
		case strings.Contains(sys, "You are the Tester"):
			return fakeapi.AnthropicReply{ToolName: "submit_diagnosis", ToolInput: map[string]any{
				"diagnosis": "int() accepts signs, whitespace and zero", "root_cause": "no syntax or range validation", "suggested_fix": "use a regex fullmatch and a range check"}}
		}
		return fakeapi.AnthropicReply{Status: 400, ErrorMessage: "unexpected role"}
	})
	defer anthropic.Close()
	jevFake := fakeapi.NewJev(func(r fakeapi.JevRequest) (int, any) {
		return 200, fakeapi.JevChoice("jev-1.13.0", "GATHER_CONTEXT", 0.91, map[string]float64{"GATHER_CONTEXT": 0.91, "REPAIR": 0.05, "REPLAN": 0.02, "ESCALATE": 0.01, "STOP": 0.01})
	})
	defer jevFake.Close()

	llmClient := claude.New(claude.Config{APIKey: "test-key", BaseURL: anthropic.URL(), MaxRetries: -1})
	jevClient := jev.New(jev.Config{Provider: "typesafe", APIKey: "test-key", BaseURL: jevFake.URL(), MaxRetries: -1})
	jd := jevClient.Describe()
	workdir := filepath.Join(dir, "work")
	exportDir := filepath.Join(dir, "exports")
	h, err := harness.New(harness.Options{
		Core: proc.Client, Operator: op, LLM: llmClient, Advisor: jevClient,
		AdvisorInfo: harness.AdvisorInfo{Configured: jd.Configured, Provider: jd.Provider, Model: jd.Model},
		Worker:      worker, Sandbox: harness.SandboxInfo{Available: true, Verified: true, Kind: worker.Kind(), ImplementationDigest: worker.ImplementationDigest()},
		Workdir:     workdir, ExportDir: exportDir, CostEstimator: claude.EstimateCostUSD, Logf: t.Logf,
	})
	must(t, err)
	must(t, h.Start(ctx))
	defer h.Close()

	// 1. Chat -> proposal card.
	_, err = h.SendChat("Write parse_page_size in src/page_size.py: 1-3 ASCII digits, value 1..100, else ValueError.")
	must(t, err)
	var propID string
	waitFor(t, "proposal card", 30*time.Second, func() bool {
		for _, m := range h.Chat() {
			if m.Card != nil && m.Card.Type == "proposal" && m.Card.Status == "pending" {
				propID = m.Card.ProposalID
				return true
			}
		}
		return false
	})

	// 2. Operator approves the task -> the loop runs A0 (fails in the sandbox),
	// Jev (LIVE) routes GATHER_CONTEXT, the Tester diagnoses, A1 passes.
	taskID, err := h.ApproveProposal(ctx, propID)
	must(t, err)
	var apprID string
	waitFor(t, "promotion approval card", 120*time.Second, func() bool {
		for _, m := range h.Chat() {
			if m.Card != nil && m.Card.Type == "approval" && m.Card.Status == "pending" && m.Card.ToolID == "promote_local" {
				apprID = m.Card.ApprovalID
				return true
			}
			if m.Card != nil && m.Card.Type == "report" && m.Card.Status != "COMPLETED" {
				t.Fatalf("task stopped early: %+v", m.Card.Lines)
			}
		}
		return false
	})

	// Nothing is promoted before approval.
	tree, err := h.Tree(ctx, "")
	must(t, err)
	if _, ok := tree["files"].(map[string]string)["src/page_size.py"]; ok {
		t.Fatal("candidate reached the approved tree before operator approval")
	}

	// 3. Operator approves the exact action -> promotion.
	must(t, h.ApproveAction(ctx, apprID))
	waitFor(t, "task completion", 60*time.Second, func() bool {
		ts, err := h.Tasks(ctx)
		if err != nil {
			return false
		}
		for _, x := range ts {
			if x.TaskID == taskID {
				return x.Status == "COMPLETED"
			}
		}
		return false
	})
	tree, err = h.Tree(ctx, "")
	must(t, err)
	if got := tree["files"].(map[string]string)["src/page_size.py"]; got != a1 {
		t.Fatalf("approved tree has wrong content:\n%s", got)
	}

	// The failing candidate really failed in the sandbox for the right cases.
	det, err := h.TaskDetail(ctx, taskID)
	must(t, err)
	cands := det["candidates"].([]map[string]any)
	for i, c := range cands {
		t.Logf("candidate %d: %v acceptance=%v", i, c["proposal_id"], c["acceptance"])
		if ds, ok := c["details"].([]runner.CaseDetail); ok {
			for _, d := range ds {
				if d.Status != "PASS" {
					t.Logf("   %s %s observed=%s stderr=%.200s", d.Status, d.Name, d.Observed, d.Stderr)
				}
			}
		}
	}
	t.Logf("routing: %+v", det["routing"])
	// Normally 2 candidates (A0 fails, A1 passes). Under heavy host load a
	// sandbox case can time out, which is never a PASS and costs one more
	// repair round; the last candidate must pass either way.
	if len(cands) < 2 || cands[len(cands)-1]["acceptance"] != "PASS" {
		t.Fatalf("want >=2 candidates ending in PASS, got %d", len(cands))
	}
	failed := map[string]bool{}
	for _, d := range cands[0]["details"].([]runner.CaseDetail) {
		if d.Status != "PASS" {
			failed[d.Name] = true
		}
	}
	// int(None) raises TypeError, not the required ValueError, so "none" fails too.
	for _, want := range []string{"zero", "over", "plus", "space", "none"} {
		if !failed[want] {
			t.Errorf("A0 should fail %s in the sandbox; failed=%v", want, failed)
		}
	}
	for _, ok := range []string{"ok_1", "ok_100", "ok_001"} {
		if failed[ok] {
			t.Errorf("A0 should pass %s", ok)
		}
	}

	// Jev was consulted once with the eligible options, and its LIVE choice applied.
	if n := len(jevFake.Requests()); n != len(cands)-1 {
		t.Fatalf("want %d Jev calls, got %d", len(cands)-1, n)
	}
	q := jevFake.Requests()[0].Questions["route"]
	if len(q.Criteria) != 5 || jevFake.Requests()[0].Header.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("unexpected Jev request: %+v", jevFake.Requests()[0])
	}
	assessments := det["assessments"].([]map[string]any)
	if len(assessments) != len(cands)-1 || assessments[0]["applied_choice"] != "GATHER_CONTEXT" || assessments[0]["used_advisor"] != true {
		t.Fatalf("assessment: %+v", assessments)
	}
	// Model calls: intake + planner + one coder call per candidate + one
	// tester diagnosis per failure (Jev chooses GATHER_CONTEXT each time).
	if want := 2 + len(cands) + (len(cands) - 1); len(anthropic.Requests()) != want {
		t.Fatalf("want %d model calls, got %d", want, len(anthropic.Requests()))
	}
	// Every request carried the key, adaptive thinking and a strict tool.
	for _, r := range anthropic.Requests() {
		if r.Header.Get("X-Api-Key") != "test-key" || r.Thinking["type"] != "adaptive" || len(r.Tools) != 1 || r.Tools[0]["strict"] != true {
			t.Fatalf("request shape: key=%q thinking=%v tools=%v", r.Header.Get("X-Api-Key"), r.Thinking, r.Tools)
		}
	}

	// 4. Export: operator proposes, then approves the exact export action.
	exID, err := h.ProposeExport(ctx)
	must(t, err)
	must(t, h.ApproveAction(ctx, exID))
	var exported string
	waitFor(t, "export", 30*time.Second, func() bool {
		ms, _ := filepath.Glob(filepath.Join(exportDir, "e2e-*", "src", "page_size.py"))
		if len(ms) == 1 {
			exported = ms[0]
			return true
		}
		return false
	})
	b, err := os.ReadFile(exported)
	must(t, err)
	if string(b) != a1 {
		t.Fatal("exported content differs")
	}

	// 5. The authoritative history replays to the same state.
	rv, err := proc.Client.ReplayVerify(ctx)
	must(t, err)
	if !rv.Matches {
		t.Fatalf("replay mismatch: %+v", rv)
	}
	t.Logf("replayed %d events; state matches", rv.Events)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
