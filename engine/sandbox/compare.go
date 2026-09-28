package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// expectation is a manifest case's "expect" field. Supported shapes:
//
//	{"returns": <json>}  PASS iff the call returned a JSON-equal value
//	{"raises": "Name"}   PASS iff the call raised and Name is in the MRO
type expectation struct {
	kind  string // "returns" | "raises"
	value any    // returns: decoded with UseNumber
	name  string // raises
}

// errUnsupportedExpectation marks an expectation shape this worker does
// not evaluate (the case becomes ERROR, never PASS).
var errUnsupportedExpectation = errors.New("unsupported expectation format")

func decodeStrict(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func parseExpectation(raw json.RawMessage) (expectation, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return expectation{}, fmt.Errorf("%w: missing", errUnsupportedExpectation)
	}
	v, err := decodeStrict(raw)
	if err != nil {
		return expectation{}, fmt.Errorf("%w: %v", errUnsupportedExpectation, err)
	}
	obj, ok := v.(map[string]any)
	if !ok || len(obj) != 1 {
		return expectation{}, fmt.Errorf("%w: want exactly one of {\"returns\": <json>} or {\"raises\": \"Name\"}", errUnsupportedExpectation)
	}
	if val, ok := obj["returns"]; ok {
		return expectation{kind: "returns", value: val}, nil
	}
	if val, ok := obj["raises"]; ok {
		name, ok := val.(string)
		if !ok || name == "" {
			return expectation{}, fmt.Errorf("%w: \"raises\" must be a non-empty exception class name", errUnsupportedExpectation)
		}
		return expectation{kind: "raises", name: name}, nil
	}
	return expectation{}, fmt.Errorf("%w: unknown key", errUnsupportedExpectation)
}

// observation is one parsed driver result line.
type observation struct {
	kind    string // "returns" | "raises" | "error"
	value   any
	name    string
	mro     []string
	message string
}

// parseObservation validates the driver's single result line.
func parseObservation(line []byte) (observation, error) {
	v, err := decodeStrict(line)
	if err != nil {
		return observation{}, fmt.Errorf("result line is not JSON: %v", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return observation{}, errors.New("result line is not a JSON object")
	}
	has := func(keys ...string) bool {
		if len(obj) != len(keys) {
			return false
		}
		for _, k := range keys {
			if _, ok := obj[k]; !ok {
				return false
			}
		}
		return true
	}
	switch {
	case has("returns"):
		return observation{kind: "returns", value: obj["returns"]}, nil
	case has("raises", "mro", "message"):
		name, ok1 := obj["raises"].(string)
		msg, ok2 := obj["message"].(string)
		list, ok3 := obj["mro"].([]any)
		if !ok1 || !ok2 || !ok3 || name == "" || len(list) == 0 {
			return observation{}, errors.New("malformed raises result")
		}
		mro := make([]string, 0, len(list))
		for _, x := range list {
			s, ok := x.(string)
			if !ok {
				return observation{}, errors.New("malformed raises result: mro entry is not a string")
			}
			mro = append(mro, s)
		}
		if mro[0] != name {
			return observation{}, errors.New("malformed raises result: mro does not start with the raised class")
		}
		return observation{kind: "raises", name: name, mro: mro, message: msg}, nil
	case has("error"):
		msg, ok := obj["error"].(string)
		if !ok {
			return observation{}, errors.New("malformed error result")
		}
		return observation{kind: "error", message: msg}, nil
	}
	return observation{}, errors.New("result line has an unknown shape")
}

// candidateFaultPrefixes are driver errors caused by the candidate's own
// behaviour (a mismatch: FAIL). Any other driver error is ERROR.
var candidateFaultPrefixes = []string{
	"import failed:",
	"entrypoint not found:",
	"entrypoint not callable:",
	"result not JSON-serializable:",
}

// judge decides one executed case from a well-formed observation.
func judge(exp expectation, obs observation) (status, reason string) {
	switch obs.kind {
	case "error":
		for _, p := range candidateFaultPrefixes {
			if strings.HasPrefix(obs.message, p) {
				return statusFail, obs.message
			}
		}
		return statusError, obs.message
	case "returns":
		if exp.kind != "returns" {
			return statusFail, fmt.Sprintf("returned a value; expected %s to be raised", exp.name)
		}
		if jsonEqual(exp.value, obs.value) {
			return statusPass, ""
		}
		return statusFail, "returned value differs from expected"
	case "raises":
		if exp.kind != "raises" {
			return statusFail, fmt.Sprintf("raised %s; expected a return value", obs.name)
		}
		for _, c := range obs.mro {
			if c == exp.name {
				return statusPass, ""
			}
		}
		return statusFail, fmt.Sprintf("raised %s, which is not a %s", obs.name, exp.name)
	}
	return statusError, "unknown observation"
}

// jsonEqual compares two values decoded with UseNumber: numbers by exact
// numeric value (1 == 1.0 == 1e0), everything else structurally (object key
// order irrelevant, array order significant, bool/null/string/number never
// equal to each other).
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numbersEqual(string(x), string(y))
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	return false
}

// decimal is an exact canonical form of a JSON number: value =
// (-1)^neg * digits * 10^exp, digits without leading or trailing zeros
// (zero is digits "" with exp 0 and neg false).
type decimal struct {
	neg    bool
	digits string
	exp    int64
}

const maxExponentDigits = 15 // keeps exponent arithmetic far from overflow

// canonicalNumber parses a JSON number exactly, in time linear in its
// length (no big-number expansion, so a forged "1e999999999" is cheap).
func canonicalNumber(s string) (decimal, bool) {
	var d decimal
	i := 0
	if i < len(s) && s[i] == '-' {
		d.neg = true
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intPart := s[start:i]
	if intPart == "" {
		return d, false
	}
	frac := ""
	if i < len(s) && s[i] == '.' {
		i++
		fs := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		frac = s[fs:i]
		if frac == "" {
			return d, false
		}
	}
	var exp int64
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		es := i
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		ds := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		digits := strings.TrimLeft(s[ds:i], "0")
		if i == ds || len(digits) > maxExponentDigits {
			return d, false
		}
		e, err := strconv.ParseInt(s[es:i], 10, 64)
		if err != nil {
			return d, false
		}
		exp = e
	}
	if i != len(s) {
		return d, false
	}
	all := intPart + frac
	exp -= int64(len(frac))
	trimmed := strings.TrimLeft(all, "0")
	if trimmed == "" {
		return decimal{}, true // zero, sign ignored (-0 == 0)
	}
	end := len(trimmed)
	for end > 0 && trimmed[end-1] == '0' {
		end--
	}
	exp += int64(len(trimmed) - end)
	d.digits = trimmed[:end]
	d.exp = exp
	return d, true
}

func numbersEqual(a, b string) bool {
	x, ok1 := canonicalNumber(a)
	y, ok2 := canonicalNumber(b)
	return ok1 && ok2 && x == y
}
