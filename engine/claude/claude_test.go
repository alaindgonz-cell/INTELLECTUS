package claude_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/claude"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakeapi"
	"github.com/alaindgonz-cell/intellectus/engine/llm"
)

const fakeKey = "sk-ant-test-FAKE-0000"

// TestMain makes accidental real network calls impossible: every test must
// point at an httptest fake, and the environment's base URL/credentials
// are replaced with unusable values.
func TestMain(m *testing.M) {
	os.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	os.Exit(m.Run())
}

func planTool() *llm.ToolSpec {
	return &llm.ToolSpec{
		Name:        "submit_plan",
		Description: "Submit the plan.",
		Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"summary", "steps"},
			"properties": map[string]any{
				"summary": map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
				"steps": map[string]any{"type": "array", "minItems": 1, "maxItems": 5,
					"items": map[string]any{"type": "string"}},
			},
		},
	}
}

func planCall() llm.Call {
	return llm.Call{
		Role:        "planner",
		System:      "You are the planner. Stable system prompt.",
		Messages:    []llm.Message{{Role: "user", Text: "Plan the fix for TASK-1."}},
		Tool:        planTool(),
		RequireTool: true,
	}
}

var goodPlan = map[string]any{"summary": "fix off-by-one", "steps": []any{"edit loop bound", "run tests"}}

func newClient(t *testing.T, fake *fakeapi.Anthropic, mut ...func(*claude.Config)) *claude.Client {
	t.Helper()
	cfg := claude.Config{APIKey: fakeKey, BaseURL: fake.URL()}
	for _, m := range mut {
		m(&cfg)
	}
	return claude.New(cfg)
}

func startFake(t *testing.T, script func(fakeapi.AnthropicRequest) fakeapi.AnthropicReply) *fakeapi.Anthropic {
	t.Helper()
	f := fakeapi.NewAnthropic(script)
	t.Cleanup(f.Close)
	return f
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNotConfiguredMakesNoNetworkCall(t *testing.T) {
	fake := startFake(t, nil)
	c := claude.New(claude.Config{BaseURL: fake.URL()})
	if c.Describe().Configured {
		t.Fatal("Configured must be false without any credential")
	}
	_, err := c.Complete(context.Background(), planCall())
	if !errors.Is(err, llm.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("made %d requests while unconfigured", n)
	}
}

func TestDescribeHasNoSecretsAndDefaults(t *testing.T) {
	c := claude.New(claude.Config{APIKey: fakeKey, BaseURL: "http://127.0.0.1:9"})
	d := c.Describe()
	if !d.Configured || d.Provider != "anthropic" {
		t.Fatalf("describe: %+v", d)
	}
	for _, r := range claude.Roles {
		if d.Models[r] != "claude-opus-5" {
			t.Fatalf("role %s model %q, want claude-opus-5", r, d.Models[r])
		}
	}
	if s := mustJSON(t, d); strings.Contains(s, fakeKey) || strings.Contains(s, "FAKE") {
		t.Fatalf("description leaks the key: %s", s)
	}
	if !strings.Contains(d.Detail, "127.0.0.1:9") {
		t.Fatalf("detail should name the host: %q", d.Detail)
	}
}

func TestCredentialsFromEnvironment(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{Text: "ok"}))
	call := llm.Call{Role: "intake", Messages: []llm.Message{{Role: "user", Text: "hi"}}}

	t.Setenv("ANTHROPIC_API_KEY", "env-api-key-FAKE")
	c := claude.New(claude.Config{BaseURL: fake.URL()})
	if !c.Describe().Configured || !strings.Contains(c.Describe().Detail, "ANTHROPIC_API_KEY") {
		t.Fatalf("env key: %+v", c.Describe())
	}
	if _, err := c.Complete(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token-FAKE")
	c = claude.New(claude.Config{BaseURL: fake.URL()})
	if !c.Describe().Configured {
		t.Fatal("auth token must configure the client")
	}
	if _, err := c.Complete(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	// An explicit key wins and the env token is not sent alongside it.
	c = claude.New(claude.Config{APIKey: fakeKey, BaseURL: fake.URL()})
	if _, err := c.Complete(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests()
	if got := reqs[0].Header.Get("X-Api-Key"); got != "env-api-key-FAKE" {
		t.Fatalf("req0 x-api-key %q", got)
	}
	if got := reqs[1].Header.Get("Authorization"); got != "Bearer env-token-FAKE" {
		t.Fatalf("req1 authorization %q", got)
	}
	if got := reqs[2].Header.Get("X-Api-Key"); got != fakeKey {
		t.Fatalf("req2 x-api-key %q", got)
	}
	if got := reqs[2].Header.Get("Authorization"); got != "" {
		t.Fatalf("explicit key must not be combined with env token, got Authorization %q", got)
	}
}

// TestWireFormat proves the request shape on the wire: adaptive thinking,
// effort, cached system prompt, one strict eager-streaming tool with
// tool_choice auto, and the server-side refusal fallback.
func TestWireFormat(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{
		ThinkingSignature: "sig-1",
		Text:              "Here is the plan.",
		ToolName:          "submit_plan",
		ToolInput:         goodPlan,
		Usage:             fakeapi.AnthropicUsage{InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 700},
	}))
	res, err := newClient(t, fake).Complete(context.Background(), planCall())
	if err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != "POST" || r.Path != "/v1/messages" || r.RawQuery != "beta=true" {
		t.Fatalf("endpoint %s %s?%s", r.Method, r.Path, r.RawQuery)
	}
	if r.Header.Get("X-Api-Key") != fakeKey || r.Header.Get("Anthropic-Version") == "" {
		t.Fatalf("auth/version headers: %v", r.Header)
	}
	if !r.HasBeta("server-side-fallback-2026-07-01") {
		t.Fatalf("missing fallback beta header, got %v", r.Betas)
	}
	f := r.Fields
	if f["model"] != "claude-opus-5" || f["max_tokens"] != float64(64000) || f["stream"] != true {
		t.Fatalf("model/max_tokens/stream: %v %v %v", f["model"], f["max_tokens"], f["stream"])
	}
	if f["fallbacks"] != "default" {
		t.Fatalf(`fallbacks = %#v, want "default"`, f["fallbacks"])
	}
	if !reflect.DeepEqual(r.Thinking, map[string]any{"type": "adaptive"}) {
		t.Fatalf("thinking = %v", r.Thinking)
	}
	if !reflect.DeepEqual(r.OutputConfig, map[string]any{"effort": "high"}) {
		t.Fatalf("output_config = %v", r.OutputConfig)
	}
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if _, ok := f[k]; ok {
			t.Fatalf("%s must not be sent", k)
		}
	}
	wantSystem := []map[string]any{{"type": "text", "text": "You are the planner. Stable system prompt.", "cache_control": map[string]any{"type": "ephemeral"}}}
	if !reflect.DeepEqual(r.System, wantSystem) {
		t.Fatalf("system = %v", r.System)
	}
	if !reflect.DeepEqual(r.ToolChoice, map[string]any{"type": "auto", "disable_parallel_tool_use": true}) {
		t.Fatalf("tool_choice = %v", r.ToolChoice)
	}
	if len(r.Tools) != 1 {
		t.Fatalf("tools = %v", r.Tools)
	}
	tool := r.Tools[0]
	if tool["name"] != "submit_plan" || tool["strict"] != true || tool["eager_input_streaming"] != true || tool["description"] != "Submit the plan." {
		t.Fatalf("tool = %v", tool)
	}
	schema := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("input_schema = %v", schema)
	}
	summary := schema["properties"].(map[string]any)["summary"].(map[string]any)
	if _, ok := summary["maxLength"]; ok {
		t.Fatalf("maxLength must be stripped for strict mode: %v", summary)
	}
	if d, _ := summary["description"].(string); !strings.Contains(d, "maxLength: 200") || !strings.Contains(d, "minLength: 1") {
		t.Fatalf("stripped constraints should be described: %v", summary)
	}
	steps := schema["properties"].(map[string]any)["steps"].(map[string]any)
	if steps["minItems"] != float64(1) {
		t.Fatalf("minItems 1 is strict-compatible and must stay: %v", steps)
	}
	if strings.Count(string(r.Body), `"type":"object"`) < 1 || strings.Contains(string(r.Body), `"type":"object","type"`) {
		t.Fatalf("suspicious input_schema serialization: %s", r.Body)
	}
	if len(r.Messages) != 1 || r.Messages[0].Role != "user" {
		t.Fatalf("messages = %v", r.Messages)
	}
	if got := r.Messages[0].Text(); got != "Plan the fix for TASK-1.Call the submit_plan tool with your answer." {
		t.Fatalf("user turn text = %q", got)
	}

	if string(res.ToolInput) != mustJSON(t, goodPlan) {
		t.Fatalf("ToolInput = %s", res.ToolInput)
	}
	want := llm.Result{
		Text: "Here is the plan.", ModelRequested: "claude-opus-5", ModelReturned: "claude-opus-5",
		StopReason: "tool_use", Attempts: 1, ResponseID: "msg_fake_0", Provider: "anthropic",
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 700},
	}
	got := res
	got.ToolInput, got.RawResponse, got.LatencyMS = nil, nil, 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result\n got %+v\nwant %+v", got, want)
	}
	var raw map[string]any
	if err := json.Unmarshal(res.RawResponse, &raw); err != nil || raw["id"] != "msg_fake_0" || raw["stop_reason"] != "tool_use" {
		t.Fatalf("RawResponse = %s (%v)", res.RawResponse, err)
	}
	if len(raw["content"].([]any)) != 3 {
		t.Fatalf("RawResponse content should hold thinking, text and tool_use: %s", res.RawResponse)
	}
	if res.LatencyMS < 0 {
		t.Fatal("negative latency")
	}
}

func TestPerRoleModelsEffortsAndFallbackPolicy(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{Text: "ok"}))
	ctx := context.Background()
	msg := []llm.Message{{Role: "user", Text: "hi"}}
	off, on := false, true

	c := newClient(t, fake, func(c *claude.Config) {
		c.Models = map[string]string{"tester": "claude-sonnet-5", "intake": "claude-haiku-4-5", "coder": "claude-fable-5-1"}
		c.Efforts = map[string]string{"tester": "low"}
		c.MaxTokens = 32000
	})
	for _, role := range []string{"tester", "intake", "coder"} {
		if _, err := c.Complete(ctx, llm.Call{Role: role, Messages: msg}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Complete(ctx, llm.Call{Role: "coder", Messages: msg, Effort: "max", MaxTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := newClient(t, fake, func(c *claude.Config) { c.Fallbacks = &off }).Complete(ctx, llm.Call{Role: "planner", Messages: msg}); err != nil {
		t.Fatal(err)
	}
	if _, err := newClient(t, fake, func(c *claude.Config) {
		c.Fallbacks = &on
		c.Models = map[string]string{"planner": "claude-sonnet-5"}
	}).Complete(ctx, llm.Call{Role: "planner", Messages: msg}); err != nil {
		t.Fatal(err)
	}

	reqs := fake.Requests()
	type want struct {
		model     string
		effort    any
		thinking  bool
		fallbacks bool
		maxTokens float64
	}
	wants := []want{
		{"claude-sonnet-5", "low", true, false, 32000},
		{"claude-haiku-4-5", nil, false, false, 32000},
		{"claude-fable-5-1", "high", true, true, 32000},
		{"claude-fable-5-1", "max", true, true, 1000},
		{"claude-opus-5", "high", true, false, 64000},
		{"claude-sonnet-5", "high", true, true, 64000},
	}
	for i, w := range wants {
		r := reqs[i]
		var effort any
		if r.OutputConfig != nil {
			effort = r.OutputConfig["effort"]
		}
		_, hasFallbacks := r.Fields["fallbacks"]
		if r.Model != w.model || effort != w.effort || (r.Thinking != nil) != w.thinking ||
			hasFallbacks != w.fallbacks || r.HasBeta("server-side-fallback-2026-07-01") != w.fallbacks ||
			r.Fields["max_tokens"] != w.maxTokens {
			t.Errorf("req %d: model=%s effort=%v thinking=%v fallbacks=%v betas=%v max=%v; want %+v",
				i, r.Model, effort, r.Thinking, r.Fields["fallbacks"], r.Betas, r.Fields["max_tokens"], w)
		}
		if _, ok := r.Fields["tools"]; ok {
			t.Errorf("req %d: no tool requested but tools sent", i)
		}
	}
}

func TestRepairAfterInvalidToolInput(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(
		fakeapi.AnthropicReply{ThinkingSignature: "sig-A", ToolName: "submit_plan", ToolUseID: "toolu_bad",
			ToolInput: map[string]any{"summary": "x"}, Usage: fakeapi.AnthropicUsage{InputTokens: 10, OutputTokens: 5}},
		fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: goodPlan,
			Usage: fakeapi.AnthropicUsage{InputTokens: 20, OutputTokens: 7, CacheReadInputTokens: 3}},
	))
	res, err := newClient(t, fake).Complete(context.Background(), planCall())
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 || string(res.ToolInput) != mustJSON(t, goodPlan) {
		t.Fatalf("attempts=%d input=%s", res.Attempts, res.ToolInput)
	}
	if res.Usage != (llm.Usage{InputTokens: 30, OutputTokens: 12, CacheReadInputTokens: 3}) {
		t.Fatalf("usage not summed: %+v", res.Usage)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
	second := reqs[1]
	// The request prefix is unchanged; only the two repair turns are new.
	if second.SystemText() != reqs[0].SystemText() || mustJSON(t, second.Tools) != mustJSON(t, reqs[0].Tools) {
		t.Fatal("system/tools changed between attempts")
	}
	if len(second.Messages) != 3 {
		t.Fatalf("want user, assistant, user; got %d messages", len(second.Messages))
	}
	asst := second.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("messages[1] role %q", asst.Role)
	}
	thinking := asst.Blocks("thinking")
	if len(thinking) != 1 || thinking[0]["signature"] != "sig-A" || thinking[0]["thinking"] != "" {
		t.Fatalf("thinking block must be echoed unchanged: %v", asst.Content)
	}
	uses := asst.Blocks("tool_use")
	if len(uses) != 1 || uses[0]["id"] != "toolu_bad" {
		t.Fatalf("tool_use not echoed: %v", asst.Content)
	}
	results := second.Messages[2].Blocks("tool_result")
	if len(results) != 1 || results[0]["tool_use_id"] != "toolu_bad" || results[0]["is_error"] != true {
		t.Fatalf("tool_result: %v", second.Messages[2].Content)
	}
	if s := mustJSON(t, results[0]["content"]); !strings.Contains(s, `missing required property \"steps\"`) {
		t.Fatalf("tool_result should carry the validation error: %s", s)
	}
}

func TestRepairAfterNoToolCall(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(
		fakeapi.AnthropicReply{Text: "I think we should fix it."},
		fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: goodPlan},
	))
	res, err := newClient(t, fake).Complete(context.Background(), planCall())
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 || res.ToolInput == nil {
		t.Fatalf("attempts=%d input=%s", res.Attempts, res.ToolInput)
	}
	second := fake.Requests()[1]
	if len(second.Messages) != 3 || second.Messages[1].Text() != "I think we should fix it." {
		t.Fatalf("messages: %+v", second.Messages)
	}
	last := second.Messages[2]
	if last.Role != "user" || last.Text() != "Call the submit_plan tool with your answer." || len(last.Blocks("tool_result")) != 0 {
		t.Fatalf("repair turn: %+v", last)
	}
}

func TestRepairIsBounded(t *testing.T) {
	t.Run("no tool call twice", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{Text: "no"}))
		res, err := newClient(t, fake).Complete(context.Background(), planCall())
		if !errors.Is(err, llm.ErrNoToolCall) || res.Attempts != 2 || len(fake.Requests()) != 2 {
			t.Fatalf("err=%v attempts=%d reqs=%d", err, res.Attempts, len(fake.Requests()))
		}
	})
	t.Run("invalid input twice", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: map[string]any{"summary": ""}}))
		res, err := newClient(t, fake).Complete(context.Background(), planCall())
		if !errors.Is(err, llm.ErrInvalidToolInput) || res.Attempts != 2 || res.ToolInput != nil {
			t.Fatalf("err=%v attempts=%d input=%s", err, res.Attempts, res.ToolInput)
		}
		if !strings.Contains(err.Error(), "maxLength") && !strings.Contains(err.Error(), "missing required") && !strings.Contains(err.Error(), "minLength") {
			t.Fatalf("error should explain the violation: %v", err)
		}
	})
	t.Run("not required", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{Text: "plain answer"}))
		call := planCall()
		call.RequireTool = false
		res, err := newClient(t, fake).Complete(context.Background(), call)
		if err != nil || res.ToolInput != nil || res.Attempts != 1 || res.Text != "plain answer" {
			t.Fatalf("err=%v res=%+v", err, res)
		}
	})
	t.Run("not required but invalid", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: map[string]any{"bogus": 1}}))
		call := planCall()
		call.RequireTool = false
		res, err := newClient(t, fake).Complete(context.Background(), call)
		if !errors.Is(err, llm.ErrInvalidToolInput) || res.ToolInput != nil || len(fake.Requests()) != 1 {
			t.Fatalf("err=%v res=%+v", err, res)
		}
	})
}

// With eager input streaming the API does not validate tool input; a
// cut-off parameter must be caught client-side, not passed through.
func TestEagerStreamedInvalidJSONIsRepaired(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(
		fakeapi.AnthropicReply{ToolName: "submit_plan", ToolUseID: "toolu_cut", RawToolInput: `{"summary":"fix","steps":["a`},
		fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: goodPlan},
	))
	res, err := newClient(t, fake).Complete(context.Background(), planCall())
	if err != nil || res.Attempts != 2 {
		t.Fatalf("err=%v attempts=%d", err, res.Attempts)
	}
	second := fake.Requests()[1]
	tr := second.Messages[2].Blocks("tool_result")
	if len(tr) != 1 || tr[0]["tool_use_id"] != "toolu_cut" || !strings.Contains(mustJSON(t, tr[0]["content"]), "not valid JSON") {
		t.Fatalf("tool_result: %v", second.Messages[2].Content)
	}
	// The echoed tool_use carries a valid (emptied) input object.
	if in := second.Messages[1].Blocks("tool_use")[0]["input"]; !reflect.DeepEqual(in, map[string]any{}) {
		t.Fatalf("echoed input = %v", in)
	}
}

func TestRefusalAndTruncation(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{
			Text: "partial", ToolName: "submit_plan", RawToolInput: `{"summ`,
			RefusalCategory: "cyber", RefusalExplanation: "declined by policy",
		}))
		res, err := newClient(t, fake).Complete(context.Background(), planCall())
		var re *llm.RefusalError
		if !errors.As(err, &re) || re.Category != "cyber" || re.Explanation != "declined by policy" {
			t.Fatalf("want RefusalError, got %v", err)
		}
		if res.Attempts != 1 || res.StopReason != "refusal" || res.ToolInput != nil || res.Text != "" || len(fake.Requests()) != 1 {
			t.Fatalf("refusal must not be repaired or used: %+v", res)
		}
	})
	t.Run("refusal without details", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{StopReason: "refusal"}))
		_, err := newClient(t, fake).Complete(context.Background(), planCall())
		var re *llm.RefusalError
		if !errors.As(err, &re) || re.Category != "" {
			t.Fatalf("want RefusalError, got %v", err)
		}
	})
	t.Run("max_tokens", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: goodPlan, StopReason: "max_tokens"}))
		res, err := newClient(t, fake).Complete(context.Background(), planCall())
		if !errors.Is(err, llm.ErrTruncated) || res.ToolInput != nil || len(fake.Requests()) != 1 {
			t.Fatalf("err=%v res=%+v", err, res)
		}
	})
}

func TestServerSideFallbackResponse(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{
		FallbackFrom: "claude-opus-5", Model: "claude-opus-4-8", RefusalCategory: "cyber",
		Text: "served by fallback", ToolName: "submit_plan", ToolInput: goodPlan,
		Usage: fakeapi.AnthropicUsage{InputTokens: 40, OutputTokens: 9},
	}))
	res, err := newClient(t, fake).Complete(context.Background(), planCall())
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelRequested != "claude-opus-5" || res.ModelReturned != "claude-opus-4-8" || res.Text != "served by fallback" {
		t.Fatalf("result: %+v", res)
	}
	// Declined attempt (40 in) + fallback attempt (40 in, 9 out).
	if res.Usage != (llm.Usage{InputTokens: 80, OutputTokens: 9}) {
		t.Fatalf("usage should sum usage.iterations: %+v", res.Usage)
	}
}

func TestAPIErrorMapping(t *testing.T) {
	fast := map[string]string{"retry-after-ms": "0"}
	cases := []struct {
		name      string
		reply     fakeapi.AnthropicReply
		status    int
		typ       string
		retryable bool
		requests  int
	}{
		{"429 retried by SDK", fakeapi.AnthropicReply{Status: 429, ErrorMessage: "slow down", Header: fast}, 429, "rate_limit_error", true, 3},
		{"529 overloaded", fakeapi.AnthropicReply{Status: 529, Header: fast}, 529, "overloaded_error", true, 3},
		{"500", fakeapi.AnthropicReply{Status: 500, Header: fast}, 500, "api_error", true, 3},
		{"400 not retried", fakeapi.AnthropicReply{Status: 400, ErrorMessage: "bad field"}, 400, "invalid_request_error", false, 1},
		{"401", fakeapi.AnthropicReply{Status: 401}, 401, "authentication_error", false, 1},
		{"stream error event", fakeapi.AnthropicReply{StreamErrorType: "overloaded_error"}, 529, "overloaded_error", true, 1},
		{"truncated stream", fakeapi.AnthropicReply{Text: "abc", Truncate: true}, 0, "incomplete_stream", true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := startFake(t, fakeapi.Sequence(tc.reply))
			res, err := newClient(t, fake).Complete(context.Background(), planCall())
			var ae *llm.APIError
			if !errors.As(err, &ae) {
				t.Fatalf("want *llm.APIError, got %T %v", err, err)
			}
			if ae.Status != tc.status || ae.Type != tc.typ || ae.Retryable != tc.retryable {
				t.Fatalf("got %+v", ae)
			}
			if tc.reply.ErrorMessage != "" && ae.Message != tc.reply.ErrorMessage {
				t.Fatalf("message %q", ae.Message)
			}
			if n := len(fake.Requests()); n != tc.requests {
				t.Fatalf("requests = %d, want %d", n, tc.requests)
			}
			if strings.Contains(err.Error(), fakeKey) || strings.Contains(mustJSON(t, res), fakeKey) {
				t.Fatal("key leaked into error/result")
			}
		})
	}
	t.Run("no retries configured", func(t *testing.T) {
		fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{Status: 529, Header: fast}))
		_, err := newClient(t, fake, func(c *claude.Config) { c.MaxRetries = -1 }).Complete(context.Background(), planCall())
		var ae *llm.APIError
		if !errors.As(err, &ae) || len(fake.Requests()) != 1 {
			t.Fatalf("err=%v reqs=%d", err, len(fake.Requests()))
		}
	})
	t.Run("network error", func(t *testing.T) {
		fake := fakeapi.NewAnthropic(nil)
		url := fake.URL()
		fake.Close()
		c := claude.New(claude.Config{APIKey: fakeKey, BaseURL: url, MaxRetries: -1})
		_, err := c.Complete(context.Background(), planCall())
		var ae *llm.APIError
		if !errors.As(err, &ae) || ae.Type != "network_error" || !ae.Retryable || ae.Status != 0 {
			t.Fatalf("got %v", err)
		}
	})
}

func TestContextCancellation(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{Text: "late", Delay: 5 * time.Second}))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := newClient(t, fake).Complete(ctx, planCall())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation not honored promptly")
	}
}

func TestInvalidCallsFailWithoutNetwork(t *testing.T) {
	fake := startFake(t, nil)
	c := newClient(t, fake)
	badSchema := planTool()
	badSchema.Schema = map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"pattern": "^x"}}}
	notObject := planTool()
	notObject.Schema = map[string]any{"type": "string"}
	cases := map[string]llm.Call{
		"no messages":       {Role: "coder"},
		"assistant last":    {Role: "coder", Messages: []llm.Message{{Role: "user", Text: "a"}, {Role: "assistant", Text: "prefill"}}},
		"not alternating":   {Role: "coder", Messages: []llm.Message{{Role: "user", Text: "a"}, {Role: "user", Text: "b"}}},
		"empty text":        {Role: "coder", Messages: []llm.Message{{Role: "user", Text: "  "}}},
		"bad effort":        {Role: "coder", Effort: "extreme", Messages: []llm.Message{{Role: "user", Text: "a"}}},
		"unsupported":       {Role: "coder", Tool: badSchema, Messages: []llm.Message{{Role: "user", Text: "a"}}},
		"non-object schema": {Role: "coder", Tool: notObject, Messages: []llm.Message{{Role: "user", Text: "a"}}},
	}
	for name, call := range cases {
		if _, err := c.Complete(context.Background(), call); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("invalid calls made %d requests", n)
	}
}

func TestMultiTurnAndConcurrency(t *testing.T) {
	fake := startFake(t, func(r fakeapi.AnthropicRequest) fakeapi.AnthropicReply {
		return fakeapi.AnthropicReply{Text: "echo:" + r.LastMessage().Text()}
	})
	c := newClient(t, fake, func(c *claude.Config) { c.DisableEagerInputStreaming = true })
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Complete(context.Background(), llm.Call{Role: "tester", Messages: []llm.Message{
				{Role: "user", Text: "one"}, {Role: "assistant", Text: "two"}, {Role: "user", Text: "three"},
			}})
			if err == nil && res.Text != "echo:three" {
				err = errors.New("unexpected text " + res.Text)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	r := fake.Requests()[0]
	if len(r.Messages) != 3 || r.Messages[1].Role != "assistant" || r.Messages[1].Text() != "two" {
		t.Fatalf("messages: %+v", r.Messages)
	}
	if _, ok := r.Fields["system"]; ok {
		t.Fatal("empty system prompt must be omitted")
	}
}

// The cached prefix (tools, then system) must be byte-identical across
// requests; a schema with several top-level keywords used to serialize in
// Go map order through the SDK's ExtraFields.
func TestCachedPrefixIsByteStable(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: goodPlan}))
	c := newClient(t, fake)
	call := planCall()
	call.Tool.Schema["description"] = "A plan."
	call.Tool.Schema["title"] = "Plan"
	call.Tool.Schema["$comment"] = "moved into the description"
	for i := 0; i < 25; i++ {
		if _, err := c.Complete(context.Background(), call); err != nil {
			t.Fatal(err)
		}
	}
	var first map[string]json.RawMessage
	for i, r := range fake.Requests() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r.Body, &m); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = m
			continue
		}
		for _, k := range []string{"tools", "system", "model", "thinking", "output_config"} {
			if string(m[k]) != string(first[k]) {
				t.Fatalf("request %d: %s bytes differ:\n%s\n%s", i, k, first[k], m[k])
			}
		}
	}
}

func TestEagerStreamingCanBeDisabled(t *testing.T) {
	fake := startFake(t, fakeapi.Sequence(fakeapi.AnthropicReply{ToolName: "submit_plan", ToolInput: goodPlan}))
	c := newClient(t, fake, func(c *claude.Config) { c.DisableEagerInputStreaming = true })
	if _, err := c.Complete(context.Background(), planCall()); err != nil {
		t.Fatal(err)
	}
	tool := fake.Requests()[0].Tools[0]
	if _, ok := tool["eager_input_streaming"]; ok || tool["strict"] != true {
		t.Fatalf("tool = %v", tool)
	}
}

func TestEstimateCostUSD(t *testing.T) {
	u := llm.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadInputTokens: 2_000_000, CacheCreationInputTokens: 400_000}
	cases := map[string]string{
		// 5 + 2.5 + 2*0.5 + 0.4*6.25
		"claude-opus-5": "11",
		// 4 + 2 + 2*0.2 + 0.4*5
		"claude-opus-5-5": "8.4",
		// 2 + 1 + 2*0.2 + 0.4*2.5
		"claude-sonnet-5": "4.4",
		// 10 + 5 + 2*0.25 + 0.4*12.5
		"claude-fable-5-1": "20.5",
		// 1 + 0.5 + 2*0.1 + 0.4*1.25
		"claude-haiku-4-5": "2.2",
	}
	for model, want := range cases {
		got, ok := claude.EstimateCostUSD(model, u)
		if !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", model, got, ok, want)
		}
	}
	if got, ok := claude.EstimateCostUSD("claude-opus-5", llm.Usage{InputTokens: 1, OutputTokens: 1}); !ok || got != "0.00003" {
		t.Errorf("small usage: %q", got)
	}
	if got, ok := claude.EstimateCostUSD("claude-opus-5", llm.Usage{}); !ok || got != "0" {
		t.Errorf("zero usage: %q", got)
	}
	if _, ok := claude.EstimateCostUSD("gpt-9", u); ok {
		t.Error("unknown model must return false")
	}
}
