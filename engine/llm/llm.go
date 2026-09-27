// Package llm is the provider-neutral seam between the harness and a model
// provider. Model output that crosses it is UNTRUSTED DATA: the harness
// decodes tool inputs strictly and submits them to the core, which decides.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Message is one plain-text conversation turn ("user" or "assistant").
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ToolSpec is a structured-output tool the model is asked to call. Schema is
// a JSON Schema object (type "object", "additionalProperties": false, every
// property listed in "required").
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

// Call is one model request.
type Call struct {
	// Role is the harness role (intake, planner, coder, tester); used for
	// model/effort selection, metering and provenance.
	Role string
	// System is the stable system prompt (cached by providers that can).
	System string
	// Messages are the conversation turns; the last must be "user".
	Messages []Message
	// Tool, when set, is the structured-output tool offered to the model.
	Tool *ToolSpec
	// RequireTool: a response without a valid call to Tool gets one bounded
	// repair attempt, then fails with ErrNoToolCall / ErrInvalidToolInput.
	RequireTool bool
	// MaxTokens caps the response (0 = provider default).
	MaxTokens int64
	// Effort is low|medium|high|xhigh|max ("" = provider default for Role).
	Effort string
}

// Usage is metered provider usage for one call (all attempts summed).
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.OutputTokens += u2.OutputTokens
	u.CacheReadInputTokens += u2.CacheReadInputTokens
	u.CacheCreationInputTokens += u2.CacheCreationInputTokens
}

// Result is a completed call.
type Result struct {
	// Text is the concatenation of the final response's text blocks.
	Text string
	// ToolInput is the schema-validated input of the call to Call.Tool
	// (nil when the model did not call it and RequireTool was false).
	ToolInput      json.RawMessage
	ModelRequested string
	ModelReturned  string
	StopReason     string
	Usage          Usage
	// Attempts is 1, or 2 when a malformed-output repair was needed.
	Attempts int
	// ResponseID is the provider's id of the final response.
	ResponseID string
	// RawResponse is the final response as JSON, kept for provenance.
	RawResponse json.RawMessage
	LatencyMS   int64
	// Provider names the backend, e.g. "anthropic".
	Provider string
}

// Client is a model backend.
type Client interface {
	Complete(ctx context.Context, call Call) (Result, error)
	// Describe reports configuration for status displays (no secrets).
	Describe() Description
}

// Description is non-secret provider configuration.
type Description struct {
	Provider   string            `json:"provider"`
	Configured bool              `json:"configured"`
	Models     map[string]string `json:"models"` // role -> model id
	Detail     string            `json:"detail,omitempty"`
}

var (
	// ErrNotConfigured: no credentials are available.
	ErrNotConfigured = errors.New("llm: provider not configured")
	// ErrNoToolCall: the model did not call the required tool.
	ErrNoToolCall = errors.New("llm: model did not call the required tool")
	// ErrInvalidToolInput: the tool input failed schema validation.
	ErrInvalidToolInput = errors.New("llm: tool input failed schema validation")
	// ErrTruncated: the response hit max_tokens.
	ErrTruncated = errors.New("llm: response truncated at max_tokens")
)

// RefusalError: the provider's safety systems declined the request.
type RefusalError struct {
	Category    string
	Explanation string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("llm: request refused (category %q): %s", e.Category, e.Explanation)
}

// APIError is a provider HTTP error after the SDK's own retries.
type APIError struct {
	Status    int
	Type      string
	Message   string
	Retryable bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: provider error %d %s: %s", e.Status, e.Type, e.Message)
}
