package claude

import (
	"encoding/json"
	"fmt"
	"strings"
)

// strictKeys are the JSON Schema keywords strict tool use accepts. Others
// (minLength, maxLength, maxItems, minimum, ...) are removed from the schema
// sent to the API, described in the description instead, and enforced
// client-side by llm.ValidateAgainstSchema against the caller's original
// schema.
var strictKeys = map[string]bool{
	"$ref": true, "$defs": true, "type": true, "anyOf": true, "oneOf": true, "allOf": true,
	"description": true, "title": true, "enum": true, "const": true,
	"properties": true, "additionalProperties": true, "required": true,
	"items": true, "minItems": true, "format": true, "pattern": true,
}

var strictFormats = map[string]bool{
	"date-time": true, "time": true, "date": true, "duration": true, "email": true,
	"hostname": true, "uri": true, "ipv4": true, "ipv6": true, "uuid": true,
}

// strictSchema returns a copy of schema shaped for strict: true. Objects
// without an explicit additionalProperties get false (strict mode requires
// it). The result is plain JSON data, so it marshals deterministically
// (sorted keys) and keeps the tools prefix byte-stable for caching.
func strictSchema(schema map[string]any) map[string]any {
	b, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		return schema
	}
	return strictNode(generic)
}

func strictNode(s map[string]any) map[string]any {
	out := map[string]any{}
	extras := map[string]any{}
	for k, v := range s {
		if !strictKeys[k] {
			extras[k] = v
			continue
		}
		out[k] = v
	}
	if f, ok := out["format"].(string); ok && !strictFormats[f] {
		extras["format"] = f
		delete(out, "format")
	}
	if n, ok := out["minItems"].(float64); ok && n != 0 && n != 1 {
		extras["minItems"] = n
		delete(out, "minItems")
	}
	if props, ok := out["properties"].(map[string]any); ok {
		np := make(map[string]any, len(props))
		for name, sub := range props {
			if m, ok := sub.(map[string]any); ok {
				np[name] = strictNode(m)
			} else {
				np[name] = sub
			}
		}
		out["properties"] = np
	}
	if m, ok := out["items"].(map[string]any); ok {
		out["items"] = strictNode(m)
	}
	if m, ok := out["additionalProperties"].(map[string]any); ok {
		out["additionalProperties"] = strictNode(m)
	}
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := out[k].([]any); ok {
			nl := make([]any, len(list))
			for i, x := range list {
				if m, ok := x.(map[string]any); ok {
					nl[i] = strictNode(m)
				} else {
					nl[i] = x
				}
			}
			out[k] = nl
		}
	}
	if defs, ok := out["$defs"].(map[string]any); ok {
		nd := make(map[string]any, len(defs))
		for name, sub := range defs {
			if m, ok := sub.(map[string]any); ok {
				nd[name] = strictNode(m)
			} else {
				nd[name] = sub
			}
		}
		out["$defs"] = nd
	}
	if isObjectSchema(out) {
		if _, ok := out["additionalProperties"]; !ok {
			out["additionalProperties"] = false
		}
	}
	if len(extras) > 0 {
		parts := make([]string, 0, len(extras))
		for _, k := range sortedStrings(extras) {
			v, _ := json.Marshal(extras[k])
			parts = append(parts, fmt.Sprintf("%s: %s", k, v))
		}
		note := "{" + strings.Join(parts, ", ") + "}"
		if d, _ := out["description"].(string); d != "" {
			out["description"] = d + "\n\n" + note
		} else {
			out["description"] = note
		}
	}
	return out
}

func isObjectSchema(s map[string]any) bool {
	if _, ok := s["properties"]; ok {
		return true
	}
	switch t := s["type"].(type) {
	case string:
		return t == "object"
	case []any:
		for _, x := range t {
			if x == "object" {
				return true
			}
		}
	}
	return false
}
