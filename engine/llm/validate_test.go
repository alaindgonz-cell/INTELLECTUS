package llm_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alaindgonz-cell/intellectus/engine/llm"
)

// planSchema mirrors the shape of a harness tool schema, built from Go
// literals ([]string, int) rather than decoded JSON.
func planSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"summary", "steps", "risk", "confidence", "note"},
		"properties": map[string]any{
			"summary": map[string]any{"type": "string", "minLength": 1, "maxLength": 10},
			"steps": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 3,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"id", "action"},
					"properties": map[string]any{
						"id":     map[string]any{"type": "integer", "minimum": 1},
						"action": map[string]any{"type": "string", "enum": []string{"edit", "test"}},
					},
				},
			},
			"risk":       map[string]any{"type": "string", "enum": []any{"low", "high"}},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"note":       map[string]any{"type": []string{"string", "null"}},
		},
	}
}

func TestValidateAgainstSchema(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string // substring; "" = valid
	}{
		{"valid", `{"summary":"fix bug","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":0.5,"note":null}`, ""},
		{"integer written as 2.0", `{"summary":"x","steps":[{"id":2.0,"action":"test"}],"risk":"high","confidence":1,"note":"n"}`, ""},
		{"exponent integer", `{"summary":"x","steps":[{"id":1e1,"action":"test"}],"risk":"high","confidence":0,"note":"n"}`, ""},
		{"unicode length counts code points", `{"summary":"ééééééééé","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":0,"note":null}`, ""},
		{"missing required", `{"summary":"x","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":0.1}`, `missing required property "note"`},
		{"additional property", `{"summary":"x","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":0.1,"note":null,"extra":1}`, `unexpected property "extra"`},
		{"nested additional property", `{"summary":"x","steps":[{"id":1,"action":"edit","x":true}],"risk":"low","confidence":0.1,"note":null}`, `$.steps[0]: unexpected property "x"`},
		{"non-integral integer", `{"summary":"x","steps":[{"id":1.5,"action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "$.steps[0].id: expected type integer, got number"},
		{"integer as string", `{"summary":"x","steps":[{"id":"1","action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "expected type integer, got string"},
		{"enum miss", `{"summary":"x","steps":[{"id":1,"action":"deploy"}],"risk":"low","confidence":0.1,"note":null}`, "$.steps[0].action: value \"deploy\" is not one of the allowed values"},
		{"enum from []any", `{"summary":"x","steps":[{"id":1,"action":"edit"}],"risk":"medium","confidence":0.1,"note":null}`, "$.risk: value"},
		{"minItems", `{"summary":"x","steps":[],"risk":"low","confidence":0.1,"note":null}`, "fewer than minItems 1"},
		{"maxItems", `{"summary":"x","steps":[{"id":1,"action":"edit"},{"id":2,"action":"edit"},{"id":3,"action":"edit"},{"id":4,"action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "more than maxItems 3"},
		{"minLength", `{"summary":"","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "below minLength 1"},
		{"maxLength", `{"summary":"01234567890","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "exceeds maxLength 10"},
		{"minimum", `{"summary":"x","steps":[{"id":0,"action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "violates minimum 1"},
		{"maximum", `{"summary":"x","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":1.0000000000000001,"note":null}`, "violates maximum 1"},
		{"type list miss", `{"summary":"x","steps":[{"id":1,"action":"edit"}],"risk":"low","confidence":0.1,"note":3}`, "expected type one of [string null], got integer"},
		{"wrong top-level type", `[1,2]`, "$: expected type object, got array"},
		{"null top-level", `null`, "expected type object, got null"},
		{"bool is not integer", `{"summary":"x","steps":[{"id":true,"action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "got boolean"},
		{"invalid JSON", `{"summary":"x",`, "invalid JSON"},
		{"trailing data", `{"summary":"x"} {}`, "trailing data"},
		{"empty input", ``, "invalid JSON: empty input"},
		{"huge exponent", `{"summary":"x","steps":[{"id":1e999999999,"action":"edit"}],"risk":"low","confidence":0.1,"note":null}`, "expected type integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := llm.ValidateAgainstSchema(planSchema(), json.RawMessage(tc.input))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %q", tc.wantErr, err)
			}
			var ve *llm.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("input errors must be *ValidationError, got %T", err)
			}
		})
	}
}

func TestValidateAdditionalPropertiesSchemaAndConst(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"kind": map[string]any{"const": "v1"}},
		"additionalProperties": map[string]any{"type": "integer"},
	}
	if err := llm.ValidateAgainstSchema(schema, json.RawMessage(`{"kind":"v1","a":1,"b":2}`)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := llm.ValidateAgainstSchema(schema, json.RawMessage(`{"kind":"v1","a":"x"}`)); err == nil || !strings.Contains(err.Error(), "$.a") {
		t.Fatalf("want error at $.a, got %v", err)
	}
	if err := llm.ValidateAgainstSchema(schema, json.RawMessage(`{"kind":"v2"}`)); err == nil || !strings.Contains(err.Error(), "constant") {
		t.Fatalf("want const error, got %v", err)
	}
	// Absent additionalProperties allows anything.
	open := map[string]any{"type": "object", "properties": map[string]any{}}
	if err := llm.ValidateAgainstSchema(open, json.RawMessage(`{"anything":[1,{"x":null}]}`)); err != nil {
		t.Fatalf("open object: %v", err)
	}
	// Numeric enum compares by value.
	nums := map[string]any{"enum": []any{1, 2.5}}
	if err := llm.ValidateAgainstSchema(nums, json.RawMessage(`1.0`)); err != nil {
		t.Fatalf("1.0 in enum [1, 2.5]: %v", err)
	}
	if err := llm.ValidateAgainstSchema(nums, json.RawMessage(`2`)); err == nil {
		t.Fatal("2 must not match enum [1, 2.5]")
	}
}

func TestValidateUnsupportedSchemaFailsClosed(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"anyOf":         {"anyOf": []any{map[string]any{"type": "string"}}},
		"pattern":       {"type": "string", "pattern": "^a"},
		"unknown type":  {"type": "decimal"},
		"tuple items":   {"type": "array", "items": []any{map[string]any{"type": "string"}}},
		"bad minLength": {"type": "string", "minLength": -1},
		"bad required":  {"type": "object", "required": "x"},
		"nested":        {"type": "object", "properties": map[string]any{"a": map[string]any{"oneOf": []any{}}}},
	} {
		t.Run(name, func(t *testing.T) {
			err := llm.ValidateAgainstSchema(schema, json.RawMessage(`{"a":"abc"}`))
			if !errors.Is(err, llm.ErrUnsupportedSchema) {
				t.Fatalf("want ErrUnsupportedSchema, got %v", err)
			}
		})
	}
	if err := llm.ValidateAgainstSchema(nil, json.RawMessage(`{}`)); !errors.Is(err, llm.ErrUnsupportedSchema) {
		t.Fatalf("nil schema: %v", err)
	}
	// Annotations are ignored, not rejected.
	ann := map[string]any{"type": "string", "description": "d", "title": "t", "format": "uri", "default": "x", "examples": []any{"y"}}
	if err := llm.ValidateAgainstSchema(ann, json.RawMessage(`"abc"`)); err != nil {
		t.Fatalf("annotations: %v", err)
	}
}

func TestValidateSchemaFromJSON(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","required":["n"],"properties":{"n":{"type":"integer","maxLength":3}},"additionalProperties":false}`), &schema); err != nil {
		t.Fatal(err)
	}
	// A float64-decoded schema behaves like a literal one; maxLength is
	// irrelevant for integers.
	if err := llm.ValidateAgainstSchema(schema, json.RawMessage(`{"n":123456}`)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// Exact decoding: 2^53+1 is integral even though float64 cannot hold it.
	if err := llm.ValidateAgainstSchema(schema, json.RawMessage(`{"n":9007199254740993}`)); err != nil {
		t.Fatalf("big integer: %v", err)
	}
}
