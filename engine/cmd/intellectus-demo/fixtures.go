package main

// Fixed inputs of the illustrative page-size workflow.

const (
	projectID = "proj-page-size"
	taskID    = "task:page-size"
	ctxID     = "ctx:page-size"
	target    = "src/page_size.py"
)

// policyJSON is written verbatim (the exact shape the core expects).
const policyJSON = `{"version":"P1","tools":{"promote_local":{"effect":"internal","requires_approval":true,"required_checks":["acceptance_tests","formal_context"],"idempotent":true},"export_view":{"effect":"external","requires_approval":true,"required_checks":[],"idempotent":false}},"trusted_issuers":{"acceptance_tests":[{"principal":"runner:fake","implementation_digest":"fake-worker/v1"}]},"protected_paths":["tests/acceptance/"],"max_repairs":2,"role_clearance":{"planner":"internal","coder":"internal","tester":"internal","runner":"internal","gateway":"internal","advisor":"public"},"jev":{"mode":"SHADOW","min_confidence_bp":8000}}
`

const r1Text = "parse_page_size(raw) must accept only strings of one to three ASCII digits whose integer value is between 1 and 100 inclusive; leading zeros are allowed. Every other input (including non-strings, signs, whitespace, decimals and non-ASCII digits) must raise ValueError."

const stubSrc = `def parse_page_size(raw: str) -> int:
    raise NotImplementedError
`

// The protected acceptance test. It is registered as a path in the
// approved root; it is NEVER executed by this build (fake worker only).
const acceptanceTestSrc = `# Protected acceptance test for R1 (tests/acceptance/ is a protected path).
# NOTE: this build never executes it; the fake worker reports scripted results.
import pytest
from src.page_size import parse_page_size

OK = [("1", 1), ("100", 100), ("001", 1)]
BAD = ["0", "101", "", "+1", " 5", "1.0", "١", None]

@pytest.mark.parametrize("raw,want", OK)
def test_ok(raw, want):
    assert parse_page_size(raw) == want

@pytest.mark.parametrize("raw", BAD)
def test_rejects(raw):
    with pytest.raises(ValueError):
        parse_page_size(raw)
`

// A0: int(raw) without syntax checks. Under real Python semantics it
// accepts "+1" -> 1, " 5" -> 5 and "١" -> 1 (int() accepts Unicode
// digits), so plus, space and arabic_indic fail.
const a0Src = `def parse_page_size(raw: str) -> int:
    if not isinstance(raw, str):
        raise ValueError("page size must be a string")

    value = int(raw)
    if not 1 <= value <= 100:
        raise ValueError("page size out of range")

    return value
`

// A1: the contract's regex fullmatch version (verbatim).
const a1Src = `import re

def parse_page_size(raw: str) -> int:
    if not isinstance(raw, str):
        raise ValueError("page size must be a string")

    if re.fullmatch(r"[0-9]{1,3}", raw) is None:
        raise ValueError("invalid page size syntax")

    value = int(raw)
    if not 1 <= value <= 100:
        raise ValueError("page size out of range")

    return value
`

// A2: A1 with the upper bound changed to 200 (never checked in the demo).
const a2Src = `import re

def parse_page_size(raw: str) -> int:
    if not isinstance(raw, str):
        raise ValueError("page size must be a string")

    if re.fullmatch(r"[0-9]{1,3}", raw) is None:
        raise ValueError("invalid page size syntax")

    value = int(raw)
    if not 1 <= value <= 200:
        raise ValueError("page size out of range")

    return value
`

type tcase struct {
	Name   string `json:"name"`
	Input  any    `json:"input"`
	Expect any    `json:"expect"`
}

var manifestCases = []tcase{
	{"ok_1", "1", 1},
	{"ok_100", "100", 100},
	{"ok_001", "001", 1},
	{"zero", "0", "ValueError"},
	{"over", "101", "ValueError"},
	{"empty", "", "ValueError"},
	{"plus", "+1", "ValueError"},
	{"space", " 5", "ValueError"},
	{"decimal", "1.0", "ValueError"},
	{"arabic_indic", "١", "ValueError"},
	{"none", nil, "ValueError"},
}

// script returns per-case scripted outcomes: all PASS except failing.
func script(failing ...string) map[string]string {
	fail := map[string]bool{}
	for _, f := range failing {
		fail[f] = true
	}
	out := map[string]string{}
	for _, c := range manifestCases {
		if fail[c.Name] {
			out[c.Name] = "FAIL"
		} else {
			out[c.Name] = "PASS"
		}
	}
	return out
}
