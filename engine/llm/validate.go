package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValidationError reports the first place where an input does not satisfy
// its schema. Path is a JSONPath-like location ("$", "$.steps[2].name").
type ValidationError struct {
	Path   string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// ErrUnsupportedSchema is wrapped by errors about the schema itself (a
// keyword outside the supported subset, or a malformed keyword value). Such
// schemas fail closed: no input validates against them.
var ErrUnsupportedSchema = errors.New("llm: unsupported schema")

// ValidateAgainstSchema checks input (one JSON value) against a JSON Schema.
//
// Supported subset: type (object, array, string, integer, number, boolean,
// null, or a list of those), properties, required, additionalProperties
// (false, true, or a schema), items (a single schema), enum, const,
// minItems/maxItems, minLength/maxLength (in Unicode code points), and
// minimum/maximum/exclusiveMinimum/exclusiveMaximum (numeric forms).
// Annotations (description, title, default, examples, format, $schema, $id,
// $comment) are ignored. Any other keyword makes the schema unsupported and
// the call fails closed with an error wrapping ErrUnsupportedSchema.
//
// Numbers are decoded exactly (no float rounding); "integer" accepts any
// number with an integral value (so 3 and 3.0 are integers, 3.5 is not).
// Input violations are returned as *ValidationError.
func ValidateAgainstSchema(schema map[string]any, input json.RawMessage) error {
	if err := CheckSchema(schema); err != nil {
		return err
	}
	s, _ := normalize(schema)
	sm := s.(map[string]any)
	v, err := decodeStrict(input)
	if err != nil {
		return &ValidationError{Path: "$", Reason: "invalid JSON: " + err.Error()}
	}
	return validateValue(sm, v, "$")
}

// CheckSchema reports whether schema is within the subset
// ValidateAgainstSchema supports (nil error) without validating any input.
// Errors wrap ErrUnsupportedSchema.
func CheckSchema(schema map[string]any) error {
	if schema == nil {
		return fmt.Errorf("%w: nil schema", ErrUnsupportedSchema)
	}
	s, err := normalize(schema)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
	}
	sm, ok := s.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: schema is not an object", ErrUnsupportedSchema)
	}
	return checkSchema(sm, "$")
}

// normalize round-trips a Go value through JSON so that schemas built from
// Go literals ([]string, int, float64, nested maps) and schemas decoded from
// JSON look the same: map[string]any, []any, json.Number, string, bool, nil.
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decodeStrict(b)
}

// decodeStrict decodes exactly one JSON value with UseNumber.
func decodeStrict(b []byte) (any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, errors.New("empty input")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

var annotationKeywords = map[string]bool{
	"description": true, "title": true, "default": true, "examples": true,
	"format": true, "$schema": true, "$id": true, "$comment": true,
}

var supportedKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "enum": true, "const": true,
	"minItems": true, "maxItems": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
}

// checkSchema verifies the whole schema once, so that an unsupported or
// malformed keyword fails closed regardless of the input's shape.
func checkSchema(s map[string]any, path string) error {
	for _, k := range sortedKeys(s) {
		if !supportedKeywords[k] && !annotationKeywords[k] {
			return schemaErr(path, "keyword %q is not supported", k)
		}
	}
	if t, ok := s["type"]; ok {
		if _, err := typeList(t, path); err != nil {
			return err
		}
	}
	if e, ok := s["enum"]; ok {
		if _, ok := e.([]any); !ok {
			return schemaErr(path, "enum must be an array")
		}
	}
	for _, k := range []string{"minItems", "maxItems", "minLength", "maxLength"} {
		if _, _, err := intKeyword(s, k, path); err != nil {
			return err
		}
	}
	for _, k := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		if raw, ok := s[k]; ok {
			n, ok := raw.(json.Number)
			if !ok {
				return schemaErr(path, "%s must be a number", k)
			}
			if _, ok := parseRat(n.String()); !ok {
				return schemaErr(path, "%s is not a valid number", k)
			}
		}
	}
	if p, ok := s["properties"]; ok {
		props, ok := p.(map[string]any)
		if !ok {
			return schemaErr(path, "properties must be an object")
		}
		for _, name := range sortedKeys(props) {
			sub, ok := props[name].(map[string]any)
			if !ok {
				return schemaErr(path+"."+name, "property schema must be an object")
			}
			if err := checkSchema(sub, path+"."+name); err != nil {
				return err
			}
		}
	}
	if r, ok := s["required"]; ok {
		list, ok := r.([]any)
		if !ok {
			return schemaErr(path, "required must be an array")
		}
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return schemaErr(path, "required entries must be strings")
			}
		}
	}
	if ap, ok := s["additionalProperties"]; ok {
		switch a := ap.(type) {
		case bool:
		case map[string]any:
			if err := checkSchema(a, path+".<additional>"); err != nil {
				return err
			}
		default:
			return schemaErr(path, "additionalProperties must be a boolean or a schema")
		}
	}
	if it, ok := s["items"]; ok {
		sub, ok := it.(map[string]any)
		if !ok {
			return schemaErr(path, "items must be a single schema object")
		}
		if err := checkSchema(sub, path+"[]"); err != nil {
			return err
		}
	}
	return nil
}

func schemaErr(path, format string, args ...any) error {
	return fmt.Errorf("%w at %s: %s", ErrUnsupportedSchema, path, fmt.Sprintf(format, args...))
}

func invalid(path, format string, args ...any) error {
	return &ValidationError{Path: path, Reason: fmt.Sprintf(format, args...)}
}

// validateValue assumes checkSchema accepted s.
func validateValue(s map[string]any, v any, path string) error {
	if t, ok := s["type"]; ok {
		types, err := typeList(t, path)
		if err != nil {
			return err
		}
		matched := false
		for _, name := range types {
			if typeMatches(name, v) {
				matched = true
				break
			}
		}
		if !matched {
			return invalid(path, "expected type %s, got %s", describeTypes(types), jsonTypeOf(v))
		}
	}

	if e, ok := s["enum"]; ok {
		list, ok := e.([]any)
		if !ok {
			return schemaErr(path, "enum must be an array")
		}
		found := false
		for _, item := range list {
			if jsonEqual(item, v) {
				found = true
				break
			}
		}
		if !found {
			return invalid(path, "value %s is not one of the allowed values", preview(v))
		}
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, v) {
		return invalid(path, "value %s does not equal the required constant", preview(v))
	}

	switch val := v.(type) {
	case map[string]any:
		return validateObject(s, val, path)
	case []any:
		return validateArray(s, val, path)
	case string:
		n := int64(utf8.RuneCountInString(val))
		if min, ok, err := intKeyword(s, "minLength", path); err != nil {
			return err
		} else if ok && n < min {
			return invalid(path, "string length %d is below minLength %d", n, min)
		}
		if max, ok, err := intKeyword(s, "maxLength", path); err != nil {
			return err
		} else if ok && n > max {
			return invalid(path, "string length %d exceeds maxLength %d", n, max)
		}
	case json.Number:
		return validateNumber(s, val, path)
	}
	return nil
}

func validateObject(s map[string]any, obj map[string]any, path string) error {
	var props map[string]any
	if p, ok := s["properties"]; ok {
		props, ok = p.(map[string]any)
		if !ok {
			return schemaErr(path, "properties must be an object")
		}
	}
	if r, ok := s["required"]; ok {
		list, ok := r.([]any)
		if !ok {
			return schemaErr(path, "required must be an array")
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return schemaErr(path, "required entries must be strings")
			}
			if _, present := obj[name]; !present {
				return invalid(path, "missing required property %q", name)
			}
		}
	}
	var extra map[string]any
	allowExtra := true
	if ap, ok := s["additionalProperties"]; ok {
		switch a := ap.(type) {
		case bool:
			allowExtra = a
		case map[string]any:
			extra = a
		default:
			return schemaErr(path, "additionalProperties must be a boolean or a schema")
		}
	}
	for _, k := range sortedKeys(obj) {
		child := path + "." + k
		if !isPlainKey(k) {
			child = path + "[" + strconv.Quote(k) + "]"
		}
		if ps, ok := props[k]; ok {
			sub, ok := ps.(map[string]any)
			if !ok {
				return schemaErr(child, "property schema must be an object")
			}
			if err := validateValue(sub, obj[k], child); err != nil {
				return err
			}
			continue
		}
		if extra != nil {
			if err := validateValue(extra, obj[k], child); err != nil {
				return err
			}
			continue
		}
		if !allowExtra {
			return invalid(path, "unexpected property %q", k)
		}
	}
	return nil
}

func validateArray(s map[string]any, arr []any, path string) error {
	n := int64(len(arr))
	if min, ok, err := intKeyword(s, "minItems", path); err != nil {
		return err
	} else if ok && n < min {
		return invalid(path, "array has %d items, fewer than minItems %d", n, min)
	}
	if max, ok, err := intKeyword(s, "maxItems", path); err != nil {
		return err
	} else if ok && n > max {
		return invalid(path, "array has %d items, more than maxItems %d", n, max)
	}
	if it, ok := s["items"]; ok {
		sub, ok := it.(map[string]any)
		if !ok {
			return schemaErr(path, "items must be a single schema object")
		}
		for i, elem := range arr {
			if err := validateValue(sub, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateNumber(s map[string]any, num json.Number, path string) error {
	x, ok := parseRat(num.String())
	if !ok {
		return invalid(path, "malformed number %s", num)
	}
	bounds := []struct {
		key       string
		exclusive bool
		lower     bool
	}{
		{"minimum", false, true}, {"exclusiveMinimum", true, true},
		{"maximum", false, false}, {"exclusiveMaximum", true, false},
	}
	for _, b := range bounds {
		raw, ok := s[b.key]
		if !ok {
			continue
		}
		bn, ok := raw.(json.Number)
		if !ok {
			return schemaErr(path, "%s must be a number", b.key)
		}
		limit, ok := parseRat(bn.String())
		if !ok {
			return schemaErr(path, "%s is not a valid number", b.key)
		}
		c := x.Cmp(limit)
		var bad bool
		switch {
		case b.lower && b.exclusive:
			bad = c <= 0
		case b.lower:
			bad = c < 0
		case b.exclusive:
			bad = c >= 0
		default:
			bad = c > 0
		}
		if bad {
			return invalid(path, "value %s violates %s %s", num, b.key, bn)
		}
	}
	return nil
}

func typeList(t any, path string) ([]string, error) {
	var out []string
	switch tv := t.(type) {
	case string:
		out = []string{tv}
	case []any:
		for _, item := range tv {
			name, ok := item.(string)
			if !ok {
				return nil, schemaErr(path, "type list entries must be strings")
			}
			out = append(out, name)
		}
	default:
		return nil, schemaErr(path, "type must be a string or a list of strings")
	}
	if len(out) == 0 {
		return nil, schemaErr(path, "type list is empty")
	}
	for _, name := range out {
		switch name {
		case "object", "array", "string", "integer", "number", "boolean", "null":
		default:
			return nil, schemaErr(path, "unknown type %q", name)
		}
	}
	return out, nil
}

func typeMatches(name string, v any) bool {
	switch name {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		r, ok := parseRat(n.String())
		return ok && r.IsInt()
	}
	return false
}

func jsonTypeOf(v any) string {
	switch val := v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case nil:
		return "null"
	case json.Number:
		if typeMatches("integer", val) {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func describeTypes(ts []string) string {
	if len(ts) == 1 {
		return ts[0]
	}
	return fmt.Sprintf("one of %v", ts)
}

// intKeyword reads a non-negative integer keyword (minItems, maxLength, ...).
func intKeyword(s map[string]any, key, path string) (int64, bool, error) {
	raw, ok := s[key]
	if !ok {
		return 0, false, nil
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0, false, schemaErr(path, "%s must be a number", key)
	}
	r, ok := parseRat(n.String())
	if !ok || !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() {
		return 0, false, schemaErr(path, "%s must be a non-negative integer", key)
	}
	return r.Num().Int64(), true, nil
}

// jsonEqual compares two normalized JSON values; numbers compare by value.
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			return false
		}
		ar, ok1 := parseRat(av.String())
		br, ok2 := parseRat(bv.String())
		return ok1 && ok2 && ar.Cmp(br) == 0
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok || !jsonEqual(x, y) {
				return false
			}
		}
		return true
	}
	return false
}

func preview(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unprintable>"
	}
	const max = 80
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// parseRat parses a JSON number exactly. Exponents beyond +-1000 are
// rejected: they are outside any range we validate and would otherwise let
// untrusted input force huge big.Int allocations (e.g. 1e999999999).
func parseRat(s string) (*big.Rat, bool) {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(strings.TrimPrefix(s[i+1:], "+"))
		if err != nil || exp > 1000 || exp < -1000 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(s)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isPlainKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
