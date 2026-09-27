// Package fakeapi holds in-process fakes of the model-provider HTTP APIs the
// engine talks to (the Anthropic Messages API and the TypeSafe Jev System
// One endpoint). They exist for tests and local demos: nothing here makes an
// outbound call, and nothing here checks credentials beyond recording the
// headers it received.
//
// Each fake is an http.Handler that can be mounted anywhere
// (NewAnthropicHandler / NewJevHandler) or started on a loopback httptest
// server (NewAnthropic / NewJev). Replies are produced by a caller-supplied
// script; every decoded request is logged and available via Requests().
package fakeapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// AnthropicMessage is one decoded entry of the request's "messages".
// String content is normalized to a single text block.
type AnthropicMessage struct {
	Role    string
	Content []map[string]any
}

// Text concatenates the message's text blocks.
func (m AnthropicMessage) Text() string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk["type"] == "text" {
			s, _ := blk["text"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

// Blocks returns the content blocks of the given type.
func (m AnthropicMessage) Blocks(typ string) []map[string]any {
	var out []map[string]any
	for _, blk := range m.Content {
		if blk["type"] == typ {
			out = append(out, blk)
		}
	}
	return out
}

// AnthropicRequest is a received Messages API request. Body holds the exact
// bytes; the other fields are decoded from it for convenience.
type AnthropicRequest struct {
	// Seq is 0 for the first request the fake received, 1 for the next...
	Seq      int
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
	// Fields is the whole body decoded as generic JSON.
	Fields       map[string]any
	Model        string
	MaxTokens    int64
	Stream       bool
	System       []map[string]any // string system prompts become one text block
	Messages     []AnthropicMessage
	Tools        []map[string]any
	ToolChoice   map[string]any
	Thinking     map[string]any
	OutputConfig map[string]any
	// Fallbacks is the raw "fallbacks" value (nil when absent).
	Fallbacks any
	// Betas lists the anthropic-beta header values (comma lists split).
	Betas []string
}

// SystemText concatenates the system prompt's text blocks.
func (r AnthropicRequest) SystemText() string {
	return AnthropicMessage{Content: r.System}.Text()
}

// LastMessage returns the final entry of "messages" (zero value if none).
func (r AnthropicRequest) LastMessage() AnthropicMessage {
	if len(r.Messages) == 0 {
		return AnthropicMessage{}
	}
	return r.Messages[len(r.Messages)-1]
}

// HasBeta reports whether the request carried the given anthropic-beta value.
func (r AnthropicRequest) HasBeta(beta string) bool {
	for _, b := range r.Betas {
		if b == beta {
			return true
		}
	}
	return false
}

// AnthropicUsage is the usage block reported by the fake.
type AnthropicUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// AnthropicReply scripts one response.
type AnthropicReply struct {
	// ID defaults to "msg_fake_<seq>".
	ID string
	// Model defaults to the request's model.
	Model string
	// ThinkingSignature, when set, emits a leading thinking block with empty
	// thinking text and this signature (the shape Claude Opus 5 returns with
	// display "omitted"), so tests can check it is echoed back unchanged.
	ThinkingSignature string
	// Text, when set, emits a text block.
	Text string
	// ToolName, when set, emits a tool_use block after the text.
	ToolName  string
	ToolUseID string // defaults to "toolu_fake_<seq>"
	// ToolInput is marshaled to JSON; RawToolInput, when set, is streamed
	// verbatim instead (it may be invalid JSON, like an eager-streamed input
	// cut off mid-parameter).
	ToolInput    any
	RawToolInput string
	// StopReason defaults to "refusal" when RefusalCategory is set, else
	// "tool_use" when ToolName is set, else "end_turn".
	StopReason         string
	RefusalCategory    string
	RefusalExplanation string
	// FallbackFrom, when set, emits a leading server-side "fallback" content
	// block (from FallbackFrom to the reply model) and usage.iterations with
	// a declined "message" entry and a "fallback_message" entry.
	FallbackFrom string
	Usage        AnthropicUsage

	// Status >= 400 answers with an Anthropic-shaped JSON error instead.
	Status       int
	ErrorType    string // defaults from Status (e.g. 429 -> rate_limit_error)
	ErrorMessage string
	// Header adds response headers (e.g. "retry-after-ms": "0").
	Header map[string]string
	// StreamErrorType, when set on a streaming reply, sends an SSE "error"
	// event (e.g. "overloaded_error") right after message_start.
	StreamErrorType string
	// Truncate ends a streaming reply before message_delta/message_stop, as
	// a dropped connection would.
	Truncate bool
	// Delay waits before answering (respecting the client going away).
	Delay time.Duration
}

// Anthropic is a fake Messages API. It is safe for concurrent use.
type Anthropic struct {
	script func(AnthropicRequest) AnthropicReply

	mu   sync.Mutex
	reqs []AnthropicRequest
	srv  *httptest.Server
}

// NewAnthropicHandler returns an unstarted fake, usable as an http.Handler.
// A nil script answers every request with the text "ok".
func NewAnthropicHandler(script func(AnthropicRequest) AnthropicReply) *Anthropic {
	if script == nil {
		script = func(AnthropicRequest) AnthropicReply { return AnthropicReply{Text: "ok"} }
	}
	return &Anthropic{script: script}
}

// NewAnthropic starts the fake on a loopback httptest server. Point a client
// at URL() and Close() it when done.
func NewAnthropic(script func(AnthropicRequest) AnthropicReply) *Anthropic {
	a := NewAnthropicHandler(script)
	a.srv = httptest.NewServer(a)
	return a
}

// Sequence returns a script that answers with replies in order; once they
// are used up the last one repeats.
func Sequence(replies ...AnthropicReply) func(AnthropicRequest) AnthropicReply {
	return func(r AnthropicRequest) AnthropicReply {
		if len(replies) == 0 {
			return AnthropicReply{Text: "ok"}
		}
		if r.Seq < len(replies) {
			return replies[r.Seq]
		}
		return replies[len(replies)-1]
	}
}

// URL is the base URL of the started server ("" if not started).
func (a *Anthropic) URL() string {
	if a.srv == nil {
		return ""
	}
	return a.srv.URL
}

// Close stops the started server.
func (a *Anthropic) Close() {
	if a.srv != nil {
		a.srv.Close()
	}
}

// Requests returns a copy of every request received so far.
func (a *Anthropic) Requests() []AnthropicRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AnthropicRequest(nil), a.reqs...)
}

// ServeHTTP implements http.Handler.
func (a *Anthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "unreadable body")
		return
	}
	req := AnthropicRequest{
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Header:   r.Header.Clone(),
		Body:     body,
	}
	for _, v := range r.Header.Values("anthropic-beta") {
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				req.Betas = append(req.Betas, b)
			}
		}
	}
	decodeErr := decodeAnthropicBody(&req)

	a.mu.Lock()
	req.Seq = len(a.reqs)
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()

	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "fake serves only POST /v1/messages")
		return
	}
	if decodeErr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "fake could not decode body: "+decodeErr.Error())
		return
	}

	reply := a.script(req)
	if reply.Delay > 0 {
		t := time.NewTimer(reply.Delay)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	for k, v := range reply.Header {
		w.Header().Set(k, v)
	}
	if reply.Status >= 400 {
		typ := reply.ErrorType
		if typ == "" {
			typ = errorTypeForStatus(reply.Status)
		}
		msg := reply.ErrorMessage
		if msg == "" {
			msg = "scripted error"
		}
		writeAnthropicError(w, reply.Status, typ, msg)
		return
	}
	m := buildMessage(req, reply)
	if req.Stream {
		streamMessage(w, m, reply)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_fake")
	_ = json.NewEncoder(w).Encode(m.final())
}

func decodeAnthropicBody(req *AnthropicRequest) error {
	if len(bytes.TrimSpace(req.Body)) == 0 {
		return fmt.Errorf("empty body")
	}
	if err := json.Unmarshal(req.Body, &req.Fields); err != nil {
		return err
	}
	f := req.Fields
	req.Model, _ = f["model"].(string)
	if n, ok := f["max_tokens"].(float64); ok {
		req.MaxTokens = int64(n)
	}
	req.Stream, _ = f["stream"].(bool)
	switch s := f["system"].(type) {
	case string:
		req.System = []map[string]any{{"type": "text", "text": s}}
	case []any:
		req.System = objects(s)
	}
	if ms, ok := f["messages"].([]any); ok {
		for _, m := range ms {
			mm, _ := m.(map[string]any)
			msg := AnthropicMessage{}
			msg.Role, _ = mm["role"].(string)
			switch c := mm["content"].(type) {
			case string:
				msg.Content = []map[string]any{{"type": "text", "text": c}}
			case []any:
				msg.Content = objects(c)
			}
			req.Messages = append(req.Messages, msg)
		}
	}
	if ts, ok := f["tools"].([]any); ok {
		req.Tools = objects(ts)
	}
	req.ToolChoice, _ = f["tool_choice"].(map[string]any)
	req.Thinking, _ = f["thinking"].(map[string]any)
	req.OutputConfig, _ = f["output_config"].(map[string]any)
	req.Fallbacks = f["fallbacks"]
	return nil
}

func objects(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func errorTypeForStatus(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 402:
		return "billing_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_fake_error")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":       "error",
		"error":      map[string]any{"type": typ, "message": msg},
		"request_id": "req_fake_error",
	})
}

// fakeMessage is the reply rendered into wire blocks.
type fakeMessage struct {
	id, model   string
	blocks      []fakeBlock
	stopReason  string
	stopDetails any
	usage       map[string]any
}

type fakeBlock struct {
	kind      string // thinking, text, tool_use, fallback
	signature string
	text      string
	toolID    string
	toolName  string
	toolInput string // JSON text (possibly invalid for RawToolInput)
	from, to  string
	category  string
}

func buildMessage(req AnthropicRequest, rep AnthropicReply) fakeMessage {
	m := fakeMessage{id: rep.ID, model: rep.Model}
	if m.id == "" {
		m.id = fmt.Sprintf("msg_fake_%d", req.Seq)
	}
	if m.model == "" {
		m.model = req.Model
	}
	if rep.FallbackFrom != "" {
		cat := rep.RefusalCategory
		if cat == "" {
			cat = "cyber"
		}
		m.blocks = append(m.blocks, fakeBlock{kind: "fallback", from: rep.FallbackFrom, to: m.model, category: cat})
	}
	if rep.ThinkingSignature != "" {
		m.blocks = append(m.blocks, fakeBlock{kind: "thinking", signature: rep.ThinkingSignature})
	}
	if rep.Text != "" {
		m.blocks = append(m.blocks, fakeBlock{kind: "text", text: rep.Text})
	}
	if rep.ToolName != "" {
		id := rep.ToolUseID
		if id == "" {
			id = fmt.Sprintf("toolu_fake_%d", req.Seq)
		}
		in := rep.RawToolInput
		if in == "" {
			b, err := json.Marshal(rep.ToolInput)
			if err != nil || rep.ToolInput == nil {
				b = []byte("{}")
			}
			in = string(b)
		}
		m.blocks = append(m.blocks, fakeBlock{kind: "tool_use", toolID: id, toolName: rep.ToolName, toolInput: in})
	}
	m.stopReason = rep.StopReason
	if m.stopReason == "" {
		switch {
		case rep.RefusalCategory != "" && rep.FallbackFrom == "":
			m.stopReason = "refusal"
		case rep.ToolName != "":
			m.stopReason = "tool_use"
		default:
			m.stopReason = "end_turn"
		}
	}
	if m.stopReason == "refusal" {
		var expl any
		if rep.RefusalExplanation != "" {
			expl = rep.RefusalExplanation
		}
		var cat any
		if rep.RefusalCategory != "" {
			cat = rep.RefusalCategory
		}
		m.stopDetails = map[string]any{"type": "refusal", "category": cat, "explanation": expl}
	}
	u := rep.Usage
	m.usage = map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"cache_read_input_tokens":     u.CacheReadInputTokens,
		"cache_creation_input_tokens": u.CacheCreationInputTokens,
	}
	if rep.FallbackFrom != "" {
		m.usage["iterations"] = []any{
			map[string]any{"type": "message", "model": rep.FallbackFrom, "input_tokens": u.InputTokens, "output_tokens": 0,
				"cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
			map[string]any{"type": "fallback_message", "model": m.model, "input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
				"cache_read_input_tokens": u.CacheReadInputTokens, "cache_creation_input_tokens": u.CacheCreationInputTokens},
		}
	}
	return m
}

func (b fakeBlock) wire(final bool) map[string]any {
	switch b.kind {
	case "thinking":
		sig := ""
		if final {
			sig = b.signature
		}
		return map[string]any{"type": "thinking", "thinking": "", "signature": sig}
	case "text":
		t := ""
		if final {
			t = b.text
		}
		return map[string]any{"type": "text", "text": t}
	case "tool_use":
		var in any = map[string]any{}
		if final {
			if json.Valid([]byte(b.toolInput)) {
				in = json.RawMessage(b.toolInput)
			}
		}
		return map[string]any{"type": "tool_use", "id": b.toolID, "name": b.toolName, "input": in}
	case "fallback":
		return map[string]any{"type": "fallback", "from": map[string]any{"model": b.from}, "to": map[string]any{"model": b.to},
			"trigger": map[string]any{"type": "refusal", "category": b.category}}
	}
	return map[string]any{"type": b.kind}
}

func (m fakeMessage) final() map[string]any {
	content := make([]any, 0, len(m.blocks))
	for _, b := range m.blocks {
		content = append(content, b.wire(true))
	}
	return map[string]any{
		"id": m.id, "type": "message", "role": "assistant", "model": m.model,
		"content": content, "stop_reason": m.stopReason, "stop_sequence": nil,
		"stop_details": m.stopDetails, "usage": m.usage,
	}
}

// streamMessage writes the reply as Messages API server-sent events.
func streamMessage(w http.ResponseWriter, m fakeMessage, rep AnthropicReply) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("request-id", "req_fake")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	startUsage := map[string]any{
		"input_tokens":                m.usage["input_tokens"],
		"output_tokens":               1,
		"cache_read_input_tokens":     m.usage["cache_read_input_tokens"],
		"cache_creation_input_tokens": m.usage["cache_creation_input_tokens"],
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": m.id, "type": "message", "role": "assistant", "model": m.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "stop_details": nil, "usage": startUsage,
	}})
	if rep.StreamErrorType != "" {
		send("error", map[string]any{"type": "error", "error": map[string]any{"type": rep.StreamErrorType, "message": "scripted stream error"}})
		return
	}
	send("ping", map[string]any{"type": "ping"})

	for i, b := range m.blocks {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": b.wire(false)})
		switch b.kind {
		case "thinking":
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "signature_delta", "signature": b.signature}})
		case "text":
			for _, part := range chunks(b.text, 7) {
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "text_delta", "text": part}})
			}
		case "tool_use":
			for _, part := range chunks(b.toolInput, 5) {
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
			}
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	if rep.Truncate {
		return
	}
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": m.stopReason, "stop_sequence": nil, "stop_details": m.stopDetails},
		"usage": m.usage})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// chunks splits s into pieces of at most n runes (at least one piece).
func chunks(s string, n int) []string {
	r := []rune(s)
	if len(r) == 0 {
		return []string{""}
	}
	var out []string
	for len(r) > 0 {
		k := min(n, len(r))
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	return out
}
