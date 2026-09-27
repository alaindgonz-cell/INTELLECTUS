// Package harness is INTELLECTUS's orchestration layer: it turns operator
// chat into approved tasks, runs the Planner / Coder / Tester roles against
// a model provider, tests candidates in the sandbox, routes failures
// (deterministic rules first, Jev optionally), asks the operator to approve
// exact changes, and reports back.
//
// Trust boundaries:
//   - Model output is untrusted data. It reaches the core only through
//     `submit` / `record_input`, where it is recorded and validated. The
//     harness decodes tool inputs strictly and never lets model text choose
//     an authorization, a policy, a principal or a verification result.
//   - Operator commands (task approval, action approval, policy changes) are
//     signed only in response to an explicit operator action in the UI.
//   - The core decides; the harness only proposes and executes what the core
//     has authorized.
package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/gateway"
	"github.com/alaindgonz-cell/intellectus/engine/llm"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

// Probe is one sandbox isolation self-test result.
type Probe struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// SandboxInfo describes the code-execution backend (from its self-test).
type SandboxInfo struct {
	Available            bool    `json:"available"`
	Verified             bool    `json:"verified"`
	Kind                 string  `json:"kind"`
	ImplementationDigest string  `json:"implementation_digest"`
	Probes               []Probe `json:"probes"`
	VerifiedAt           int64   `json:"verified_at"`
	Detail               string  `json:"detail"`
}

// AdvisorInfo describes the Jev advisor configuration (no secrets).
type AdvisorInfo struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Detail     string `json:"detail"`
}

// Options configure a Harness.
type Options struct {
	Core     *coreclient.Client
	Operator *coreclient.Operator
	LLM      llm.Client
	// Advisor is the Jev client; nil disables advisory routing.
	Advisor     advisor.DecisionAdvisor
	AdvisorInfo AdvisorInfo
	// Worker executes acceptance tests. Nil (or an unverified sandbox) means
	// no code is ever executed and tasks cannot pass their checks.
	Worker  runner.Worker
	Sandbox SandboxInfo
	// Workdir holds harness-local state (chat log). ExportDir receives
	// approved-tree exports.
	Workdir   string
	ExportDir string
	// CostEstimator returns an estimated USD cost for model usage.
	CostEstimator func(model string, u llm.Usage) (string, bool)
	// Limits.
	MaxModelCallsPerTask int
	ApprovalTTL          time.Duration
	AdvisorTimeout       time.Duration
	Now                  func() time.Time
	Logf                 func(format string, args ...any)
}

type sessions struct {
	intake, planner, coder, tester, runner, gateway, advisor, scheduler coreclient.ID
}

// Harness is the running orchestration layer.
type Harness struct {
	opt      Options
	bus      *Bus
	activity *activityLog
	chat     *chatStore
	sess     sessions
	env      string // registered environment id for the sandbox worker

	mu        sync.Mutex
	proposals map[string]*proposal
	approvals map[string]*approval
	runs      map[string]*taskRun
	nextProp  int
	nextAppr  int
	usage     usageTotals
	chatBusy  bool
	rootCtx   context.Context
	cancelAll context.CancelFunc
	wg        sync.WaitGroup
}

type usageTotals struct {
	ModelCalls int `json:"model_calls"`
	Usage      llm.Usage
	CostUSD    float64 `json:"-"`
	CostKnown  bool    `json:"-"`
	JevCalls   int     `json:"jev_calls"`
	JevCostUSD float64 `json:"-"`
}

// New builds a harness; call Start before use.
func New(opt Options) (*Harness, error) {
	if opt.Core == nil || opt.Operator == nil || opt.LLM == nil {
		return nil, errors.New("harness: Core, Operator and LLM are required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	if opt.MaxModelCallsPerTask <= 0 {
		opt.MaxModelCallsPerTask = 14
	}
	if opt.ApprovalTTL <= 0 {
		opt.ApprovalTTL = time.Hour
	}
	if opt.AdvisorTimeout <= 0 {
		opt.AdvisorTimeout = 10 * time.Second
	}
	chat, err := openChatStore(filepath.Join(opt.Workdir, "chat.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("harness: chat store: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Harness{
		opt: opt, bus: NewBus(), activity: newActivityLog(2000), chat: chat,
		proposals: map[string]*proposal{}, approvals: map[string]*approval{}, runs: map[string]*taskRun{},
		rootCtx: ctx, cancelAll: cancel,
	}, nil
}

// Bus exposes the UI event bus.
func (h *Harness) Bus() *Bus { return h.bus }

// Start opens the core sessions, registers the sandbox environment and
// restores chat state after a restart.
func (h *Harness) Start(ctx context.Context) error {
	open := func(role, label string) (coreclient.ID, error) {
		s, err := h.opt.Core.OpenSession(ctx, role, label)
		if err != nil {
			return "", fmt.Errorf("open_session %s: %w", role, err)
		}
		return s.SessionID, nil
	}
	var err error
	steps := []struct {
		dst         *coreclient.ID
		role, label string
	}{
		{&h.sess.intake, coreclient.RoleIntake, "claude"},
		{&h.sess.planner, coreclient.RolePlanner, "claude"},
		{&h.sess.coder, coreclient.RoleCoder, "claude"},
		{&h.sess.tester, coreclient.RoleTester, "claude"},
		{&h.sess.gateway, coreclient.RoleGateway, "fs"},
		{&h.sess.advisor, coreclient.RoleAdvisor, "jev"},
		{&h.sess.scheduler, coreclient.RoleScheduler, "engine"},
	}
	if h.opt.Worker != nil {
		steps = append(steps, struct {
			dst         *coreclient.ID
			role, label string
		}{&h.sess.runner, coreclient.RoleRunner, h.opt.Worker.Label()})
	}
	for _, s := range steps {
		if *s.dst, err = open(s.role, s.label); err != nil {
			return err
		}
	}
	if h.opt.Worker != nil && h.opt.Sandbox.Verified {
		if err := h.ensureEnvironment(ctx); err != nil {
			return err
		}
	}
	h.restore(ctx)
	h.systemActivity("system", "info", "INTELLECTUS started", h.startupDetail())
	return nil
}

// Close cancels running tasks and waits for them.
func (h *Harness) Close() {
	h.cancelAll()
	h.wg.Wait()
}

func (h *Harness) startupDetail() string {
	d := h.opt.LLM.Describe()
	s := fmt.Sprintf("Claude: configured=%v; Jev: configured=%v (%s); sandbox: available=%v verified=%v",
		d.Configured, h.opt.AdvisorInfo.Configured, h.opt.AdvisorInfo.Model, h.opt.Sandbox.Available, h.opt.Sandbox.Verified)
	return s
}

// ensureEnvironment registers the sandbox environment once per worker
// implementation (registration is idempotent in content).
func (h *Harness) ensureEnvironment(ctx context.Context) error {
	sum := sha256.Sum256([]byte(h.opt.Worker.ImplementationDigest()))
	h.env = "ENV-" + h.opt.Worker.Kind() + "-" + hex.EncodeToString(sum[:])[:12]
	st, err := h.opt.Core.Status(ctx)
	if err != nil {
		return err
	}
	for _, e := range st.Environments {
		if e.ID == h.env {
			return nil
		}
	}
	res, err := h.opt.Core.OperatorOp(ctx, h.opt.Operator, "register_environment", map[string]any{
		"environment_id": h.env,
		"description":    fmt.Sprintf("%s sandbox, worker %s", h.opt.Worker.Kind(), h.opt.Worker.ImplementationDigest()),
		"worker_kind":    h.opt.Worker.Kind(),
	})
	if err != nil {
		return err
	}
	if res.Status != "APPLIED" {
		return fmt.Errorf("register_environment: %s", res.Reasons.Codes())
	}
	return nil
}

// ---- status -----------------------------------------------------------------

// Status is the UI status object (docs/UI_API.md).
func (h *Harness) Status(ctx context.Context) (map[string]any, error) {
	st, err := h.opt.Core.Status(ctx)
	if err != nil {
		return nil, err
	}
	pol := decodePolicy(st.Policy)
	jevMode, _ := dig(pol, "jev", "mode").(string)
	sandbox := h.opt.Sandbox
	trusted := h.sandboxTrusted(pol)
	d := h.opt.LLM.Describe()
	h.mu.Lock()
	u := h.usage
	busy := h.chatBusy
	for _, r := range h.runs {
		if r.active {
			busy = true
		}
	}
	h.mu.Unlock()
	cost := "unknown"
	if u.CostKnown || u.ModelCalls == 0 {
		cost = fmt.Sprintf("%.4f", u.CostUSD)
	}
	return map[string]any{
		"project_id":    st.ProjectID,
		"approved_root": st.ApprovedRoot,
		"head_sequence": st.HeadSequence,
		"core": map[string]any{
			"reducer_version": st.ReducerVersion, "deductor": st.Deductor.Kind,
			"deductor_digest": st.Deductor.ImplementationDigest, "policy_version": st.PolicyVersion,
		},
		"claude": map[string]any{"configured": d.Configured, "provider": d.Provider, "models": d.Models, "detail": d.Detail},
		"jev": map[string]any{
			"configured": h.opt.AdvisorInfo.Configured && h.opt.Advisor != nil, "policy_mode": jevMode,
			"model": h.opt.AdvisorInfo.Model, "provider": h.opt.AdvisorInfo.Provider, "detail": h.opt.AdvisorInfo.Detail,
		},
		"sandbox": map[string]any{
			"available": sandbox.Available, "verified": sandbox.Verified, "trusted": trusted, "kind": sandbox.Kind,
			"implementation_digest": sandbox.ImplementationDigest, "probes": sandbox.Probes,
			"verified_at": sandbox.VerifiedAt, "detail": sandbox.Detail,
		},
		"usage": map[string]any{
			"model_calls": u.ModelCalls, "input_tokens": u.Usage.InputTokens, "output_tokens": u.Usage.OutputTokens,
			"cache_read_input_tokens": u.Usage.CacheReadInputTokens, "cache_creation_input_tokens": u.Usage.CacheCreationInputTokens,
			"estimated_cost_usd": cost, "jev_calls": u.JevCalls, "jev_cost_usd": fmt.Sprintf("%.6f", u.JevCostUSD),
		},
		"busy": busy,
	}, nil
}

func (h *Harness) publishStatus() {
	st, err := h.Status(h.rootCtx)
	if err == nil {
		h.bus.Publish("status", st)
	}
}

// ---- policy (operator-initiated only) -----------------------------------------

func decodePolicy(raw json.RawMessage) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return map[string]any{}
	}
	return m
}

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func (h *Harness) sandboxTrusted(pol map[string]any) bool {
	if h.opt.Worker == nil {
		return false
	}
	issuers, _ := dig(pol, "trusted_issuers", "acceptance_tests").([]any)
	for _, i := range issuers {
		m, _ := i.(map[string]any)
		if m["principal"] == "runner:"+h.opt.Worker.Label() && m["implementation_digest"] == h.opt.Worker.ImplementationDigest() {
			return true
		}
	}
	return false
}

func (h *Harness) setPolicy(ctx context.Context, mutate func(pol map[string]any) error) error {
	st, err := h.opt.Core.Status(ctx)
	if err != nil {
		return err
	}
	pol := decodePolicy(st.Policy)
	if err := mutate(pol); err != nil {
		return err
	}
	res, err := h.opt.Core.OperatorOp(ctx, h.opt.Operator, "set_policy", map[string]any{"policy": pol})
	if err != nil {
		return err
	}
	if res.Status != "APPLIED" {
		return fmt.Errorf("set_policy rejected: %s", res.Reasons.Codes())
	}
	h.publishStatus()
	return nil
}

// SetJevMode changes policy.jev.mode (operator action).
func (h *Harness) SetJevMode(ctx context.Context, mode string) error {
	if mode != "SHADOW" && mode != "LIVE" && mode != "OFF" {
		return fmt.Errorf("invalid jev mode %q", mode)
	}
	err := h.setPolicy(ctx, func(pol map[string]any) error {
		j, _ := pol["jev"].(map[string]any)
		if j == nil {
			return errors.New("policy has no jev section")
		}
		j["mode"] = mode
		return nil
	})
	if err == nil {
		h.systemActivity("approval", "ok", "Operator set Jev routing mode to "+mode, "")
	}
	return err
}

// TrustSandbox adds the current sandbox worker to the acceptance-test
// trusted issuers (operator action; the digest changes whenever the worker,
// bubblewrap or Python changes).
func (h *Harness) TrustSandbox(ctx context.Context) error {
	if h.opt.Worker == nil || !h.opt.Sandbox.Verified {
		return errors.New("no verified sandbox to trust")
	}
	err := h.setPolicy(ctx, func(pol map[string]any) error {
		ti, _ := pol["trusted_issuers"].(map[string]any)
		if ti == nil {
			ti = map[string]any{}
			pol["trusted_issuers"] = ti
		}
		list, _ := ti["acceptance_tests"].([]any)
		list = append(list, map[string]any{
			"principal": "runner:" + h.opt.Worker.Label(), "implementation_digest": h.opt.Worker.ImplementationDigest(),
		})
		ti["acceptance_tests"] = list
		return nil
	})
	if err == nil {
		h.systemActivity("approval", "ok", "Operator trusted sandbox "+h.opt.Worker.ImplementationDigest(), "")
		if h.env == "" {
			err = h.ensureEnvironment(ctx)
		}
	}
	return err
}

// ---- activity & chat helpers ----------------------------------------------------

func (h *Harness) now() int64 { return nowMS(h.opt.Now()) }

// act publishes a new activity item and returns its id.
func (h *Harness) act(taskID, kind, status, title, detail string, data map[string]any) string {
	it := ActivityItem{ID: h.activity.newID(), TS: h.now(), TaskID: taskID, Kind: kind, Status: status, Title: title, Detail: detail, Data: data}
	h.activity.put(it)
	h.bus.Publish("activity", it)
	return it.ID
}

// update replaces an activity item's status/title/detail (empty = keep).
func (h *Harness) update(id, status, title, detail string, data map[string]any) {
	it, ok := h.activity.get(id)
	if !ok {
		return
	}
	it.TS = h.now()
	if status != "" {
		it.Status = status
	}
	if title != "" {
		it.Title = title
	}
	if detail != "" {
		it.Detail = detail
	}
	if data != nil {
		it.Data = data
	}
	h.activity.put(it)
	h.bus.Publish("activity", it)
}

func (h *Harness) systemActivity(kind, status, title, detail string) {
	h.act("", kind, status, title, detail, nil)
}

// Activity lists recent items.
func (h *Harness) Activity(limit int) []ActivityItem { return h.activity.list(limit) }

// Chat lists chat messages.
func (h *Harness) Chat() []Message { return h.chat.list() }

func (h *Harness) post(role, text, taskID string, card *Card) Message {
	m := Message{ID: h.chat.newID(), Role: role, TS: h.now(), Text: text, TaskID: taskID, Card: card}
	if err := h.chat.put(m); err != nil {
		h.opt.Logf("chat store: %v", err)
	}
	h.bus.Publish("chat", m)
	return m
}

func (h *Harness) repost(m Message) {
	if err := h.chat.put(m); err != nil {
		h.opt.Logf("chat store: %v", err)
	}
	h.bus.Publish("chat", m)
}

// ---- usage ------------------------------------------------------------------------

func (h *Harness) meter(res llm.Result) {
	h.mu.Lock()
	h.usage.ModelCalls++
	h.usage.Usage.Add(res.Usage)
	if h.opt.CostEstimator != nil {
		if c, ok := h.opt.CostEstimator(res.ModelRequested, res.Usage); ok {
			var f float64
			if _, err := fmt.Sscanf(c, "%g", &f); err == nil {
				h.usage.CostUSD += f
				h.usage.CostKnown = true
			}
		}
	}
	h.mu.Unlock()
	h.publishStatus()
}

func (h *Harness) meterJev(a *advisor.Advice) {
	if a == nil {
		return
	}
	h.mu.Lock()
	h.usage.JevCalls++
	var f float64
	if a.CostUSD != "" {
		if _, err := fmt.Sscanf(a.CostUSD, "%g", &f); err == nil {
			h.usage.JevCostUSD += f
		}
	}
	h.mu.Unlock()
}

// providerRecord converts a model result into core provenance.
func providerRecord(res llm.Result) *coreclient.ProviderRecord {
	raw := string(res.RawResponse)
	usage, _ := json.Marshal(res.Usage)
	return &coreclient.ProviderRecord{
		Provider: res.Provider, ModelRequested: res.ModelRequested, ModelReturned: res.ModelReturned,
		ResponseID: res.ResponseID, RawResponse: &raw, Usage: usage, LatencyMS: res.LatencyMS, Attempts: res.Attempts,
	}
}

// dispatcher builds a gateway dispatcher for external effects.
func (h *Harness) dispatcher() *gateway.Dispatcher {
	return &gateway.Dispatcher{
		Core: h.opt.Core, Session: h.sess.gateway,
		Adapters: map[string]gateway.Adapter{"export_view": &gateway.FSExportAdapter{Core: h.opt.Core, Dir: h.opt.ExportDir}},
	}
}
