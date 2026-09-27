package sandbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeStrict([]byte(s))
	if err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

func TestJSONEqual(t *testing.T) {
	equal := [][2]string{
		{`1`, `1.0`},
		{`1`, `1e0`},
		{`100`, `1E2`},
		{`0.5`, `5e-1`},
		{`0`, `-0`},
		{`0`, `0.000e10`},
		{`-12.50`, `-1.25e1`},
		{`12345678901234567890123`, `12345678901234567890123.000`},
		{`null`, `null`},
		{`true`, `true`},
		{`"a"`, `"a"`},
		{`[1,2,[3]]`, `[1.0,2,[3e0]]`},
		{`{"a":1,"b":{"c":[true,null]}}`, `{"b":{"c":[true,null]},"a":1.0}`},
	}
	for _, c := range equal {
		if !jsonEqual(mustDecode(t, c[0]), mustDecode(t, c[1])) {
			t.Errorf("%s should equal %s", c[0], c[1])
		}
	}
	different := [][2]string{
		{`1`, `"1"`},
		{`1`, `true`},
		{`0`, `false`},
		{`0`, `null`},
		{`""`, `null`},
		{`12345678901234567890`, `12345678901234567891`},
		{`0.1`, `0.10000000000000001`},
		{`1`, `-1`},
		{`[1,2]`, `[2,1]`},
		{`[1]`, `[1,1]`},
		{`{"a":1}`, `{"a":1,"b":2}`},
		{`{"a":1}`, `{"b":1}`},
		{`{}`, `[]`},
		{`1e999999999999999999`, `1e999999999999999999`}, // exponent too large: never equal
	}
	for _, c := range different {
		if jsonEqual(mustDecode(t, c[0]), mustDecode(t, c[1])) {
			t.Errorf("%s should differ from %s", c[0], c[1])
		}
	}
	// A forged huge exponent must be cheap to compare.
	start := time.Now()
	if numbersEqual("1e999999999", "1e999999998") {
		t.Error("different huge exponents compared equal")
	}
	if !numbersEqual("10e999999999", "1e1000000000") {
		t.Error("equal huge exponents compared different")
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("huge exponent comparison took %s", d)
	}
}

func TestParseExpectation(t *testing.T) {
	ok := map[string]string{
		`{"returns": 1}`:            "returns",
		`{"returns": null}`:         "returns",
		`{"returns": {"a": [1]}}`:   "returns",
		`{"raises": "ValueError"}`:  "raises",
		` {"raises":"KeyError"} `:   "raises",
		`{"returns": "raises"}`:     "returns",
		`{"returns": [1, "x", {}]}`: "returns",
	}
	for raw, kind := range ok {
		e, err := parseExpectation(json.RawMessage(raw))
		if err != nil || e.kind != kind {
			t.Errorf("%s: kind %q err %v", raw, e.kind, err)
		}
	}
	bad := []string{
		``, `null`, `1`, `"returns"`, `[]`, `{}`,
		`{"equals": 1}`,
		`{"returns": 1, "raises": "ValueError"}`,
		`{"raises": ""}`,
		`{"raises": 1}`,
		`{"raises": "ValueError", "message": "x"}`,
		`{"returns": 1} {"returns": 2}`,
		`{"returns": 1`,
	}
	for _, raw := range bad {
		if _, err := parseExpectation(json.RawMessage(raw)); !errors.Is(err, errUnsupportedExpectation) {
			t.Errorf("%q: want unsupported expectation, got %v", raw, err)
		}
	}
}

func TestParseObservationAndJudge(t *testing.T) {
	ret1, _ := parseExpectation(json.RawMessage(`{"returns": 1}`))
	raisesVE, _ := parseExpectation(json.RawMessage(`{"raises": "ValueError"}`))
	cases := []struct {
		line string
		exp  expectation
		want string
	}{
		{`{"returns":1.0}`, ret1, statusPass},
		{`{"returns":2}`, ret1, statusFail},
		{`{"returns":true}`, ret1, statusFail},
		{`{"raises":"ValueError","mro":["ValueError","Exception","BaseException","object"],"message":"x"}`, raisesVE, statusPass},
		{`{"raises":"UnicodeDecodeError","mro":["UnicodeDecodeError","UnicodeError","ValueError","Exception","BaseException","object"],"message":""}`, raisesVE, statusPass},
		{`{"raises":"TypeError","mro":["TypeError","Exception","BaseException","object"],"message":"x"}`, raisesVE, statusFail},
		{`{"raises":"ValueError","mro":["ValueError"],"message":"x"}`, ret1, statusFail},
		{`{"returns":1}`, raisesVE, statusFail},
		{`{"error":"import failed: SyntaxError: bad"}`, ret1, statusFail},
		{`{"error":"entrypoint not found: f"}`, ret1, statusFail},
		{`{"error":"result not JSON-serializable: set"}`, ret1, statusFail},
		{`{"error":"driver: bad request: KeyError"}`, ret1, statusError},
	}
	for _, c := range cases {
		obs, err := parseObservation([]byte(c.line))
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		if got, _ := judge(c.exp, obs); got != c.want {
			t.Errorf("%s vs %+v: got %s want %s", c.line, c.exp, got, c.want)
		}
	}
	malformed := []string{
		`1`, `[]`, `{}`, `{"returns":1,"extra":2}`,
		`{"raises":"ValueError"}`,
		`{"raises":"ValueError","mro":[],"message":"x"}`,
		`{"raises":"ValueError","mro":["Exception"],"message":"x"}`,
		`{"raises":"ValueError","mro":[1],"message":"x"}`,
		`{"error":1}`,
		`{"returns":1}{"returns":1}`,
	}
	for _, m := range malformed {
		if _, err := parseObservation([]byte(m)); err == nil {
			t.Errorf("%s: accepted malformed result", m)
		}
	}
}

func TestSingleLine(t *testing.T) {
	good := "{\"returns\":1}\n"
	if l, err := singleLine([]byte(good)); err != nil || string(l) != `{"returns":1}` {
		t.Fatalf("good line: %q %v", l, err)
	}
	for _, bad := range []string{"", "{\"returns\":1}", "{}\n{}\n", "{}\nx", "\n\n"} {
		if _, err := singleLine([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{"a.py", "src/page_size.py", "a/b/c/d.txt", "x..y/z", ".hidden/f"} {
		if err := validatePath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x.py", "a/../b", "a/./b", "./a", "a//b", "a/", `a\b`, "a\x00b", "a\nb", strings.Repeat("x", 256)} {
		if err := validatePath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestCapBuf(t *testing.T) {
	b := capBuf{limit: 4}
	n, err := b.Write([]byte("abcdef"))
	if n != 6 || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	b.Write([]byte("gh"))
	if s := b.String(); !strings.HasPrefix(s, "abcd\n[truncated: 8 bytes total, 4 kept]") {
		t.Fatalf("got %q", s)
	}
}

func TestComputeDigestCoversInputs(t *testing.T) {
	base := computeDigest([]byte("d"), []byte("s"), "bwrap 1", "Python 3")
	if !strings.HasPrefix(base, DigestPrefix) || len(base) != len(DigestPrefix)+64 {
		t.Fatalf("bad digest %q", base)
	}
	for _, other := range []string{
		computeDigest([]byte("d2"), []byte("s"), "bwrap 1", "Python 3"),
		computeDigest([]byte("d"), []byte("s2"), "bwrap 1", "Python 3"),
		computeDigest([]byte("d"), []byte("s"), "bwrap 2", "Python 3"),
		computeDigest([]byte("d"), []byte("s"), "bwrap 1", "Python 4"),
		computeDigest([]byte("ds"), []byte(""), "bwrap 1", "Python 3"),
	} {
		if other == base {
			t.Errorf("digest did not change")
		}
	}
}
