// Package claude implements llm.Client on the Claude Messages API using the
// official Go SDK (github.com/anthropics/anthropic-sdk-go).
//
// Every request is streamed and accumulated (max_tokens defaults to 64000),
// uses adaptive thinking with a per-role output_config.effort, caches the
// stable system prompt, and — when a structured-output tool is offered —
// offers exactly that one tool with strict: true and eager_input_streaming,
// tool_choice auto (forced tool_choice is a 400 on newer models). Because
// eager streaming skips server-side input validation, the tool input is
// re-parsed and validated here against the tool's schema before it is
// returned. For claude-opus-5 and claude-fable-5-1 the server-side refusal
// fallback ("fallbacks": "default", beta server-side-fallback-2026-07-01)
// is enabled by default; this is why requests go through the SDK's beta
// Messages service, whose params carry the typed Fallbacks field.
//
// The API key is never logged, put in an error, or returned.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"github.com/alaindgonz-cell/intellectus/engine/llm"
)

// Provider is the llm.Result.Provider / Description.Provider value.
const Provider = "anthropic"

// Defaults.
const (
	DefaultModel      = "claude-opus-5"
	DefaultMaxTokens  = int64(64000)
	DefaultMaxRetries = 2
	defaultBaseURL    = "https://api.anthropic.com"
)

// Roles are the harness roles with configured defaults.
var Roles = []string{"intake", "planner", "coder", "tester"}

// DefaultEfforts returns the default output_config.effort per role.
func DefaultEfforts() map[string]string {
	return map[string]string{"intake": "medium", "planner": "high", "coder": "high", "tester": "medium"}
}

// fallbackModels get the server-side refusal fallback when Config.Fallbacks
// is nil.
var fallbackModels = map[string]bool{"claude-opus-5": true, "claude-fable-5-1": true}

var validEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// Config configures a Client. The zero value is usable: credentials then
// come from the environment (ANTHROPIC_API_KEY, else ANTHROPIC_AUTH_TOKEN),
// resolved by the SDK.
type Config struct {
	// APIKey, when set, is the only credential used (environment
	// credentials are then ignored entirely).
	APIKey string
	// BaseURL overrides the API base URL (default: ANTHROPIC_BASE_URL, else
	// https://api.anthropic.com).
	BaseURL string
	// Models maps role -> model id; unlisted roles use Models["default"],
	// else "claude-opus-5".
	Models map[string]string
	// Efforts maps role -> low|medium|high|xhigh|max, merged over
	// DefaultEfforts(). Call.Effort overrides it per call.
	Efforts map[string]string
	// MaxTokens caps each response (default 64000). Call.MaxTokens
	// overrides it per call.
	MaxTokens int64
	// Fallbacks: nil = server-side refusal fallback for claude-opus-5 and
	// claude-fable-5-1 only; true = request it for every model; false = never.
	Fallbacks *bool
	// MaxRetries is the SDK's retry count for 408/409/429/5xx and network
	// errors: 0 = default (2), negative = no retries.
	MaxRetries int
	// Timeout bounds each HTTP attempt (0 = none; the caller's context
	// bounds the whole call).
	Timeout time.Duration
	// HTTPClient replaces the default HTTP client.
	HTTPClient *http.Client
	// DisableEagerInputStreaming drops eager_input_streaming from the
	// offered tool, for gateways in front of the API that reject the field.
	DisableEagerInputStreaming bool
}

// Client is an llm.Client for the Claude API. Safe for concurrent use.
type Client struct {
	sdk        anthropic.Client
	configured bool
	credSource string
	host       string
	models     map[string]string
	efforts    map[string]string
	maxTokens  int64
	fallbacks  *bool
	eager      bool
	retries    int
}

var _ llm.Client = (*Client)(nil)

// New builds a Client. It performs no network I/O.
func New(cfg Config) *Client {
	c := &Client{
		models:    map[string]string{},
		efforts:   DefaultEfforts(),
		maxTokens: cfg.MaxTokens,
		fallbacks: cfg.Fallbacks,
		eager:     !cfg.DisableEagerInputStreaming,
		retries:   cfg.MaxRetries,
	}
	for _, r := range Roles {
		c.models[r] = DefaultModel
	}
	for r, m := range cfg.Models {
		if m != "" {
			c.models[r] = m
		}
	}
	for r, e := range cfg.Efforts {
		c.efforts[r] = e
	}
	if c.maxTokens <= 0 {
		c.maxTokens = DefaultMaxTokens
	}
	switch {
	case c.retries == 0:
		c.retries = DefaultMaxRetries
	case c.retries < 0:
		c.retries = 0
	}

	base := cfg.BaseURL
	if base == "" {
		base = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if base == "" {
		base = defaultBaseURL
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		c.host = u.Host
	} else {
		c.host = "(invalid base URL)"
	}

	var opts []option.RequestOption
	switch {
	case cfg.APIKey != "":
		// Explicit key: skip the SDK's environment autoload so an
		// ANTHROPIC_AUTH_TOKEN in the environment is never sent alongside.
		c.configured, c.credSource = true, "config api key"
		opts = append(opts, option.WithoutEnvironmentDefaults(), option.WithAPIKey(cfg.APIKey), option.WithBaseURL(base))
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		c.configured, c.credSource = true, "env ANTHROPIC_API_KEY"
	case os.Getenv("ANTHROPIC_AUTH_TOKEN") != "":
		c.configured, c.credSource = true, "env ANTHROPIC_AUTH_TOKEN"
	}
	if !c.configured {
		return c
	}
	if cfg.APIKey == "" && cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = defaultHTTPClient()
	}
	opts = append(opts, option.WithHTTPClient(hc), option.WithMaxRetries(c.retries))
	if cfg.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(cfg.Timeout))
	}
	c.sdk = anthropic.NewClient(opts...)
	return c
}

// defaultHTTPClient mirrors the SDK's default: a response-header timeout
// that does not limit long streams.
func defaultHTTPClient() *http.Client {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t = t.Clone()
		t.ResponseHeaderTimeout = 10 * time.Minute
		return &http.Client{Transport: t}
	}
	return &http.Client{Transport: http.DefaultTransport}
}

// Describe implements llm.Client. It contains no secrets.
func (c *Client) Describe() llm.Description {
	models := make(map[string]string, len(c.models))
	for r, m := range c.models {
		models[r] = m
	}
	detail := "credentials: none (set ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN)"
	if c.configured {
		detail = "credentials: " + c.credSource
	}
	detail += "; host: " + c.host
	return llm.Description{Provider: Provider, Configured: c.configured, Models: models, Detail: detail}
}

func (c *Client) modelFor(role string) string {
	if m, ok := c.models[role]; ok && m != "" {
		return m
	}
	if m := c.models["default"]; m != "" {
		return m
	}
	return DefaultModel
}

// supportsAdaptive reports whether a model takes adaptive thinking and
// output_config.effort (every current Opus/Sonnet/Fable/Mythos model; not
// Haiku 4.5).
func supportsAdaptive(model string) bool {
	return !strings.HasPrefix(model, "claude-haiku-")
}

func (c *Client) fallbacksFor(model string) bool {
	if c.fallbacks != nil {
		return *c.fallbacks
	}
	return fallbackModels[model]
}

// Complete implements llm.Client.
func (c *Client) Complete(ctx context.Context, call llm.Call) (llm.Result, error) {
	if !c.configured {
		return llm.Result{}, llm.ErrNotConfigured
	}
	model := c.modelFor(call.Role)
	res := llm.Result{ModelRequested: model, Provider: Provider}
	params, err := c.buildParams(call, model)
	if err != nil {
		return res, err
	}
	start := time.Now()
	res, err = c.run(ctx, call, params, res)
	res.LatencyMS = time.Since(start).Milliseconds()
	return res, err
}

func (c *Client) run(ctx context.Context, call llm.Call, params anthropic.BetaMessageNewParams, res llm.Result) (llm.Result, error) {
	for attempt := 1; ; attempt++ {
		res.Attempts = attempt
		msg, rawInputs, err := c.stream(ctx, params)
		if msg != nil {
			res.Usage.Add(usageOf(msg))
			res.ModelReturned = msg.Model
			res.ResponseID = msg.ID
			res.StopReason = string(msg.StopReason)
			res.RawResponse = rawOf(msg)
		}
		if err != nil {
			return res, mapError(ctx, err)
		}

		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			// Check before touching content: a refusal can leave partial
			// output (or a tool_use cut off mid-input) that must not be used.
			return res, &llm.RefusalError{Category: string(msg.StopDetails.Category), Explanation: msg.StopDetails.Explanation}
		case anthropic.BetaStopReasonMaxTokens:
			return res, llm.ErrTruncated
		case anthropic.BetaStopReasonModelContextWindowExceeded:
			return res, fmt.Errorf("%w (stop_reason model_context_window_exceeded)", llm.ErrTruncated)
		case anthropic.BetaStopReasonEndTurn, anthropic.BetaStopReasonToolUse, anthropic.BetaStopReasonStopSequence:
		default:
			return res, fmt.Errorf("claude: unexpected stop_reason %q", msg.StopReason)
		}
		res.Text = textOf(msg)
		if call.Tool == nil {
			return res, nil
		}

		idx, found := findToolUse(msg, call.Tool.Name)
		var verr error
		if found {
			input := toolInput(msg, idx, rawInputs)
			verr = validateToolInput(call.Tool.Schema, input)
			if verr == nil {
				var buf bytes.Buffer
				_ = json.Compact(&buf, input)
				res.ToolInput = json.RawMessage(buf.Bytes())
				return res, nil
			}
		}
		if !call.RequireTool {
			if found {
				return res, fmt.Errorf("%w: %v", llm.ErrInvalidToolInput, verr)
			}
			return res, nil
		}
		if attempt >= 2 {
			if found {
				return res, fmt.Errorf("%w: %v", llm.ErrInvalidToolInput, verr)
			}
			return res, llm.ErrNoToolCall
		}
		params.Messages = repairTurn(params.Messages, msg, call.Tool.Name, found, verr)
	}
}

// buildParams validates the call (no network) and builds the request.
func (c *Client) buildParams(call llm.Call, model string) (anthropic.BetaMessageNewParams, error) {
	var p anthropic.BetaMessageNewParams
	if len(call.Messages) == 0 {
		return p, errors.New("claude: call has no messages")
	}
	maxTokens := c.maxTokens
	if call.MaxTokens > 0 {
		maxTokens = call.MaxTokens
	}
	effort := call.Effort
	if effort == "" {
		effort = c.efforts[call.Role]
	}
	if effort != "" && !validEfforts[effort] {
		return p, fmt.Errorf("claude: invalid effort %q (want low|medium|high|xhigh|max)", effort)
	}

	msgs := make([]anthropic.BetaMessageParam, 0, len(call.Messages))
	for i, m := range call.Messages {
		if strings.TrimSpace(m.Text) == "" {
			return p, fmt.Errorf("claude: message %d is empty", i)
		}
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if m.Role != want {
			return p, fmt.Errorf("claude: message %d has role %q, want %q (turns must alternate, starting with user)", i, m.Role, want)
		}
		block := anthropic.NewBetaTextBlock(m.Text)
		if m.Role == "user" {
			msgs = append(msgs, anthropic.NewBetaUserMessage(block))
		} else {
			msgs = append(msgs, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{block}})
		}
	}
	if call.Messages[len(call.Messages)-1].Role != "user" {
		// Also: last-assistant-turn prefill is rejected by current models.
		return p, errors.New("claude: the last message must be from the user")
	}

	p = anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		Messages:  msgs,
	}
	if call.System != "" {
		// One stable block with the breakpoint on it caches tools + system.
		p.System = []anthropic.BetaTextBlockParam{{Text: call.System, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}}
	}
	if supportsAdaptive(model) {
		p.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{}}
		if effort != "" {
			p.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(effort)}
		}
	}
	if t := call.Tool; t != nil {
		if t.Name == "" {
			return p, errors.New("claude: tool has no name")
		}
		if typ, _ := t.Schema["type"].(string); typ != "object" {
			return p, fmt.Errorf("claude: tool %q schema must have type \"object\"", t.Name)
		}
		if err := llm.CheckSchema(t.Schema); err != nil {
			return p, fmt.Errorf("claude: tool %q: %w", t.Name, err)
		}
		tool := anthropic.BetaToolParam{
			Name:        t.Name,
			InputSchema: schemaParam(strictSchema(t.Schema)),
			Strict:      anthropic.Bool(true),
		}
		if t.Description != "" {
			tool.Description = anthropic.String(t.Description)
		}
		if c.eager {
			tool.EagerInputStreaming = anthropic.Bool(true)
		}
		p.Tools = []anthropic.BetaToolUnionParam{{OfTool: &tool}}
		p.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfAuto: &anthropic.BetaToolChoiceAutoParam{DisableParallelToolUse: anthropic.Bool(true)}}
		if call.RequireTool {
			// tool_choice cannot force the call; say it in the final turn
			// (after every cache breakpoint, so caching is unaffected).
			last := &p.Messages[len(p.Messages)-1]
			last.Content = append(last.Content, anthropic.NewBetaTextBlock(toolInstruction(t.Name)))
		}
	}
	if c.fallbacksFor(model) {
		p.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		p.Betas = append(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}
	return p, nil
}

// schemaParam sends schema as pre-serialized JSON. encoding/json sorts map
// keys, so the bytes are identical on every request; the SDK's ExtraFields
// path appends keys in Go map order, which would randomly change the tools
// prefix and silently defeat prompt caching.
func schemaParam(schema map[string]any) anthropic.BetaToolInputSchemaParam {
	b, err := json.Marshal(schema)
	if err != nil {
		return anthropic.BetaToolInputSchemaParam{ExtraFields: schema}
	}
	return param.Override[anthropic.BetaToolInputSchemaParam](json.RawMessage(b))
}

func toolInstruction(name string) string {
	return fmt.Sprintf("Call the %s tool with your answer.", name)
}

var errIncompleteStream = errors.New("stream ended before message_stop")

// stream runs one streamed request and accumulates it. rawInputs holds the
// exact input_json_delta text per content-block index (the accumulator
// silently replaces unparseable input with {}; validation must see it).
func (c *Client) stream(ctx context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, map[int64][]byte, error) {
	s := c.sdk.Beta.Messages.NewStreaming(ctx, params)
	defer s.Close()
	var msg *anthropic.BetaMessage
	acc := anthropic.BetaMessage{}
	raw := map[int64][]byte{}
	stopped := false
	for s.Next() {
		ev := s.Current()
		if err := acc.Accumulate(ev); err != nil {
			return msg, raw, fmt.Errorf("accumulate stream: %w", err)
		}
		switch ev.Type {
		case "message_start":
			msg = &acc
		case "content_block_delta":
			if ev.Delta.Type == "input_json_delta" {
				raw[ev.Index] = append(raw[ev.Index], ev.Delta.PartialJSON...)
			}
		case "message_stop":
			stopped = true
		}
	}
	if err := s.Err(); err != nil {
		return msg, raw, err
	}
	if msg == nil || !stopped {
		return msg, raw, errIncompleteStream
	}
	return msg, raw, nil
}

// lastFallback is the index of the last server-side fallback block (-1 if
// none). Content before it belongs to a model that declined.
func lastFallback(msg *anthropic.BetaMessage) int {
	last := -1
	for i, b := range msg.Content {
		if b.Type == "fallback" {
			last = i
		}
	}
	return last
}

func textOf(msg *anthropic.BetaMessage) string {
	var b strings.Builder
	for _, blk := range msg.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// findToolUse returns the first call to name made by the serving model.
func findToolUse(msg *anthropic.BetaMessage, name string) (int, bool) {
	for i := lastFallback(msg) + 1; i < len(msg.Content); i++ {
		if b := msg.Content[i]; b.Type == "tool_use" && b.Name == name {
			return i, true
		}
	}
	return 0, false
}

func toolInput(msg *anthropic.BetaMessage, idx int, rawInputs map[int64][]byte) []byte {
	if r, ok := rawInputs[int64(idx)]; ok {
		return r
	}
	if in := msg.Content[idx].Input; len(in) > 0 {
		return in
	}
	return []byte("{}")
}

func validateToolInput(schema map[string]any, input []byte) error {
	if !json.Valid(input) {
		return &llm.ValidationError{Path: "$", Reason: "tool input is not valid JSON (possibly cut off)"}
	}
	return llm.ValidateAgainstSchema(schema, input)
}

// repairTurn appends the model's turn and one corrective user turn.
func repairTurn(history []anthropic.BetaMessageParam, msg *anthropic.BetaMessage, tool string, called bool, verr error) []anthropic.BetaMessageParam {
	assistant := echoParam(msg)
	var feedback []anthropic.BetaContentBlockParamUnion
	for _, blk := range assistant.Content {
		if blk.OfToolUse == nil {
			continue
		}
		// Every tool_use needs a tool_result in the next user turn.
		text := "Not executed: only one call is accepted."
		if blk.OfToolUse.Name == tool && called {
			text = fmt.Sprintf("Tool input rejected: %v. Call the %s tool again with input that satisfies its schema.", verr, tool)
		}
		feedback = append(feedback, anthropic.NewBetaToolResultBlock(blk.OfToolUse.ID, text, true))
	}
	if !called {
		feedback = append(feedback, anthropic.NewBetaTextBlock(toolInstruction(tool)))
	}
	if len(assistant.Content) == 0 {
		// Nothing to echo (an empty turn cannot be sent): repeat the request
		// with the instruction added to the final user turn.
		out := append([]anthropic.BetaMessageParam(nil), history...)
		last := out[len(out)-1]
		last.Content = append(append([]anthropic.BetaContentBlockParamUnion(nil), last.Content...), anthropic.NewBetaTextBlock(toolInstruction(tool)))
		out[len(out)-1] = last
		return out
	}
	return append(history, assistant, anthropic.NewBetaUserMessage(feedback...))
}

// echoParam converts the response to a history entry. ToParam keeps
// thinking blocks (and their signatures) unchanged. After a server-side
// fallback, thinking/redacted_thinking/tool_use blocks that precede the
// last fallback block came from a model that declined and are omitted; the
// fallback block itself stays in place.
func echoParam(msg *anthropic.BetaMessage) anthropic.BetaMessageParam {
	p := msg.ToParam()
	cut := lastFallback(msg)
	if cut < 0 {
		return p
	}
	kept := p.Content[:0:0]
	for i, blk := range msg.Content {
		if i < cut && (blk.Type == "thinking" || blk.Type == "redacted_thinking" || blk.Type == "tool_use") {
			continue
		}
		kept = append(kept, p.Content[i])
	}
	p.Content = kept
	return p
}

// usageOf sums per-attempt usage when a server-side fallback ran (the
// top-level usage then covers only the serving attempt).
func usageOf(msg *anthropic.BetaMessage) llm.Usage {
	u := msg.Usage
	fellBack := false
	for _, it := range u.Iterations {
		if it.Type == "fallback_message" {
			fellBack = true
		}
	}
	if fellBack {
		var sum llm.Usage
		for _, it := range u.Iterations {
			if it.Type == "message" || it.Type == "fallback_message" {
				sum.Add(llm.Usage{InputTokens: it.InputTokens, OutputTokens: it.OutputTokens,
					CacheReadInputTokens: it.CacheReadInputTokens, CacheCreationInputTokens: it.CacheCreationInputTokens})
			}
		}
		return sum
	}
	return llm.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadInputTokens: u.CacheReadInputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens}
}

func rawOf(msg *anthropic.BetaMessage) json.RawMessage {
	if r := msg.RawJSON(); r != "" && json.Valid([]byte(r)) {
		return json.RawMessage(r)
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return b
}

// mapError converts SDK/transport errors to *llm.APIError; caller
// cancellation is returned unchanged.
func mapError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		typ := string(apierr.Type())
		status := apierr.StatusCode
		if status < 400 {
			// An SSE "error" event arrives on a 200 stream.
			status = statusForType(typ)
		}
		return &llm.APIError{Status: status, Type: typ, Message: apiMessage(apierr), Retryable: retryableStatus(status)}
	}
	if errors.Is(err, errIncompleteStream) {
		return &llm.APIError{Type: "incomplete_stream", Message: err.Error(), Retryable: true}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// Per-attempt Config.Timeout expired (the caller's context is alive).
		return &llm.APIError{Type: "timeout", Message: err.Error(), Retryable: true}
	}
	var nerr net.Error
	var uerr *url.Error
	if errors.As(err, &nerr) || errors.As(err, &uerr) {
		return &llm.APIError{Type: "network_error", Message: err.Error(), Retryable: true}
	}
	return fmt.Errorf("claude: %w", err)
}

func retryableStatus(s int) bool {
	return s == http.StatusTooManyRequests || s >= 500
}

func statusForType(typ string) int {
	switch typ {
	case "overloaded_error":
		return 529
	case "rate_limit_error":
		return 429
	case "api_error":
		return 500
	case "invalid_request_error":
		return 400
	case "authentication_error":
		return 401
	case "permission_error":
		return 403
	case "not_found_error":
		return 404
	}
	return 500
}

// apiMessage extracts error.message from the response body.
func apiMessage(e *anthropic.Error) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if raw := e.RawJSON(); raw != "" && json.Unmarshal([]byte(raw), &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return http.StatusText(e.StatusCode)
}

// sortedStrings is a small helper for deterministic output.
func sortedStrings(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
