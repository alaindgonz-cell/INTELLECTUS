package claude

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// After a mid-output fallback, blocks from the declining model that must not
// be echoed (thinking, redacted_thinking, tool_use) are dropped; the
// fallback block and everything after it are kept in order.
func TestEchoParamAfterFallback(t *testing.T) {
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","stop_reason":"tool_use",
	 "content":[
	  {"type":"thinking","thinking":"","signature":"sig-declined"},
	  {"type":"text","text":"partial "},
	  {"type":"tool_use","id":"toolu_declined","name":"submit_plan","input":{}},
	  {"type":"fallback","from":{"model":"claude-opus-5"},"to":{"model":"claude-opus-4-8"},"trigger":{"type":"refusal","category":"cyber"}},
	  {"type":"thinking","thinking":"","signature":"sig-served"},
	  {"type":"text","text":"continued"},
	  {"type":"tool_use","id":"toolu_served","name":"submit_plan","input":{"summary":"s"}}
	 ],
	 "usage":{"input_tokens":1,"output_tokens":1}}`
	var msg anthropic.BetaMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	p := echoParam(&msg)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, c := range got.Content {
		types = append(types, c["type"].(string))
	}
	if want := []string{"text", "fallback", "thinking", "text", "tool_use"}; !reflect.DeepEqual(types, want) || got.Role != "assistant" {
		t.Fatalf("echoed %v (role %s), want %v", types, got.Role, want)
	}
	if got.Content[2]["signature"] != "sig-served" || got.Content[4]["id"] != "toolu_served" {
		t.Fatalf("echo %s", b)
	}
	if idx, ok := findToolUse(&msg, "submit_plan"); !ok || idx != 6 {
		t.Fatalf("findToolUse = %d %v; the declined call must be ignored", idx, ok)
	}
	if textOf(&msg) != "partial continued" {
		t.Fatalf("text %q", textOf(&msg))
	}
}

func TestStrictSchema(t *testing.T) {
	in := map[string]any{
		"type":     "object",
		"required": []string{"note", "tags"},
		"properties": map[string]any{
			"note": map[string]any{"type": []string{"string", "null"}, "maxLength": 20, "format": "uri", "description": "A link."},
			"tags": map[string]any{"type": "array", "minItems": 2, "maxItems": 4,
				"items": map[string]any{"type": "object", "properties": map[string]any{"k": map[string]any{"type": "string", "format": "email"}}}},
			"n": map[string]any{"type": "integer", "minimum": 0},
		},
	}
	got := strictSchema(in)
	want := map[string]any{
		"type":                 "object",
		"required":             []any{"note", "tags"},
		"additionalProperties": false,
		"properties": map[string]any{
			"note": map[string]any{"type": []any{"string", "null"}, "format": "uri", "description": "A link.\n\n{maxLength: 20}"},
			"tags": map[string]any{"type": "array", "description": "{maxItems: 4, minItems: 2}",
				"items": map[string]any{"type": "object", "additionalProperties": false,
					"properties": map[string]any{"k": map[string]any{"type": "string", "format": "email"}}}},
			"n": map[string]any{"type": "integer", "description": "{minimum: 0}"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("strict schema:\n%s", g)
	}
	// The caller's schema is not mutated.
	if _, ok := in["additionalProperties"]; ok {
		t.Fatal("input schema was mutated")
	}
}

func TestUsageOfSumsFallbackIterations(t *testing.T) {
	var msg anthropic.BetaMessage
	raw := `{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":"end_turn",
	 "usage":{"input_tokens":7,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,
	  "iterations":[{"type":"message","model":"a","input_tokens":7,"output_tokens":2,"cache_read_input_tokens":1,"cache_creation_input_tokens":0},
	                {"type":"fallback_message","model":"x","input_tokens":7,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":3}]}}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	u := usageOf(&msg)
	if u.InputTokens != 14 || u.OutputTokens != 7 || u.CacheReadInputTokens != 1 || u.CacheCreationInputTokens != 3 {
		t.Fatalf("usage %+v", u)
	}
}
