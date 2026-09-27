// Command uidev is a DEV-ONLY fixture server for the INTELLECTUS web UI.
//
// It serves web.Static and implements every endpoint of docs/UI_API.md with
// in-memory, SIMULATED data plus a scripted timeline, so the UI can be
// developed and screenshot-tested without a core, a model provider or a
// sandbox. Nothing here is authoritative, nothing is executed, and it is never
// shipped as the product. /api/status carries a "UI DEV FIXTURES — simulated
// data" detail so it cannot be mistaken for a real run.
//
//	go run ./internal/uidev [--addr 127.0.0.1:7788] [--token secret] [--speed 1]
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/web"
)

const devBanner = "UI DEV FIXTURES — simulated data"

// csp mirrors the header sent by the real server (engine/web), so the
// fixture catches any inline script/style the UI might introduce.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

const (
	coderModel = "claude-opus-5"
	jevModel   = "jev-1.13.0"
	operator   = "operator:ed25519:4f1c9a0e"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7788", "listen address")
	token := flag.String("token", "", "if set, require the session cookie obtained by opening /?token=<token> (mimics intellectus serve)")
	speed := flag.Float64("speed", 1, "timeline speed multiplier (2 = twice as fast)")
	flag.Parse()
	if *speed <= 0 {
		log.Fatal("--speed must be > 0")
	}
	s := newServer(*token, *speed)
	url := "http://" + *addr + "/"
	if *token != "" {
		url += "?token=" + *token
	}
	log.Printf("uidev: %s — open %s", devBanner, url)
	srv := &http.Server{Addr: *addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// ---------------------------------------------------------------- API types

type Entrypoint struct {
	Language string `json:"language"`
	Path     string `json:"path"`
	Function string `json:"function"`
}

type Case struct {
	Name   string         `json:"name"`
	Input  string         `json:"input"`
	Expect map[string]any `json:"expect"`
}

type Message struct {
	ID     string  `json:"id"`
	Role   string  `json:"role"`
	TS     int64   `json:"ts"`
	Text   string  `json:"text"`
	TaskID *string `json:"task_id"`
	Card   any     `json:"card"`
}

type ProposalCard struct {
	Type        string     `json:"type"`
	ProposalID  string     `json:"proposal_id"`
	Status      string     `json:"status"`
	Title       string     `json:"title"`
	Requirement string     `json:"requirement"`
	Entrypoint  Entrypoint `json:"entrypoint"`
	Cases       []Case     `json:"cases"`
	TaskID      *string    `json:"task_id"`
}

type Check struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Issuer string `json:"issuer"`
	Detail string `json:"detail"`
}

type ApprovalCard struct {
	Type         string  `json:"type"`
	ApprovalID   string  `json:"approval_id"`
	Status       string  `json:"status"`
	TaskID       *string `json:"task_id"`
	ToolID       string  `json:"tool_id"`
	Title        string  `json:"title"`
	ActionDigest string  `json:"action_digest"`
	FromRoot     string  `json:"from_root"`
	ToRoot       string  `json:"to_root"`
	Checks       []Check `json:"checks"`
	Summary      string  `json:"summary"`
}

type ReportCard struct {
	Type         string   `json:"type"`
	TaskID       string   `json:"task_id"`
	Status       string   `json:"status"`
	ApprovedRoot string   `json:"approved_root"`
	Lines        []string `json:"lines"`
	Limitations  []string `json:"limitations"`
}

type Item struct {
	ID     string         `json:"id"`
	TS     int64          `json:"ts"`
	TaskID *string        `json:"task_id"`
	Kind   string         `json:"kind"`
	Status string         `json:"status"`
	Title  string         `json:"title"`
	Detail string         `json:"detail,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

type TaskSummary struct {
	TaskID           string `json:"task_id"`
	Title            string `json:"title"`
	Status           string `json:"status"`
	RepairsUsed      int    `json:"repairs_used"`
	MaxRepairs       int    `json:"max_repairs"`
	FailedCandidates int    `json:"failed_candidates"`
	Candidates       int    `json:"candidates"`
	Phase            string `json:"phase"`
	UpdatedTS        int64  `json:"updated_ts"`
}

type CaseDetail struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Input      string `json:"input"`
	Expected   string `json:"expected"`
	Observed   string `json:"observed"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int    `json:"duration_ms"`
}

type Candidate struct {
	ProposalID    string       `json:"proposal_id"`
	CandidateRoot string       `json:"candidate_root"`
	Acceptance    string       `json:"acceptance"`
	Details       []CaseDetail `json:"details"`
}

type Assessment struct {
	AssessmentID   string  `json:"assessment_id"`
	Choice         *string `json:"choice"`
	ConfidenceBP   *int    `json:"confidence_bp"`
	AppliedChoice  string  `json:"applied_choice"`
	UsedAdvisor    bool    `json:"used_advisor"`
	FallbackReason *string `json:"fallback_reason"`
	Mode           string  `json:"mode"`
}

type Postcondition struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
}

type Action struct {
	ActionID       string          `json:"action_id"`
	ToolID         string          `json:"tool_id"`
	State          string          `json:"state"`
	Postconditions []Postcondition `json:"postconditions"`
}

type Event struct {
	Sequence   int    `json:"sequence"`
	Type       string `json:"type"`
	Actor      string `json:"actor"`
	RecordedAt int64  `json:"recorded_at"`
	Summary    string `json:"summary"`
}

type Probe struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type Status struct {
	ProjectID    string `json:"project_id"`
	ApprovedRoot string `json:"approved_root"`
	HeadSequence int    `json:"head_sequence"`
	Core         struct {
		ReducerVersion int    `json:"reducer_version"`
		Deductor       string `json:"deductor"`
		DeductorDigest string `json:"deductor_digest"`
	} `json:"core"`
	Claude struct {
		Configured bool              `json:"configured"`
		Provider   string            `json:"provider"`
		Models     map[string]string `json:"models"`
		Detail     string            `json:"detail"`
	} `json:"claude"`
	Jev struct {
		Configured bool   `json:"configured"`
		PolicyMode string `json:"policy_mode"`
		Model      string `json:"model"`
		Detail     string `json:"detail"`
	} `json:"jev"`
	Sandbox struct {
		Available            bool    `json:"available"`
		Verified             bool    `json:"verified"`
		Trusted              bool    `json:"trusted"`
		Kind                 string  `json:"kind"`
		ImplementationDigest string  `json:"implementation_digest"`
		Probes               []Probe `json:"probes"`
		VerifiedAt           int64   `json:"verified_at"`
		Detail               string  `json:"detail"`
	} `json:"sandbox"`
	Usage struct {
		ModelCalls           int    `json:"model_calls"`
		InputTokens          int    `json:"input_tokens"`
		OutputTokens         int    `json:"output_tokens"`
		CacheReadInputTokens int    `json:"cache_read_input_tokens"`
		EstimatedCostUSD     string `json:"estimated_cost_usd"`
		JevCalls             int    `json:"jev_calls"`
		JevCostUSD           string `json:"jev_cost_usd"`
	} `json:"usage"`
	Busy bool `json:"busy"`
}

// --------------------------------------------------------------- scenarios

// scenario is one scripted request → proposal → candidates story.
type scenario struct {
	title, requirement string
	entry              Entrypoint
	cases              []Case
	plan               string
	a0Files, a1Files   map[string]string // files written by A0 / A1 on top of the base tree
	a0Fail             map[string]string // case name → observed result for A0 failures
	repairNote         string
}

func returns(v any) map[string]any      { return map[string]any{"returns": v} }
func raises(name string) map[string]any { return map[string]any{"raises": name} }

const pageSizeA0 = `def parse_page_size(raw):
    value = int(raw.strip())
    if value < 1 or value > 100:
        raise ValueError("page size must be 1..100")
    return value
`

const pageSizeA1 = `"""Strict page-size parsing."""
import re

_PAGE_SIZE = re.compile(r"[0-9]{1,3}")


def parse_page_size(raw):
    """Return raw as an int if it is 1-3 ASCII digits with value 1..100."""
    if not isinstance(raw, str) or not _PAGE_SIZE.fullmatch(raw):
        raise ValueError("page size must be 1-3 ASCII digits")
    value = int(raw)
    if not 1 <= value <= 100:
        raise ValueError("page size must be 1..100")
    return value
`

const paginationV1 = `"""Pagination helpers."""

DEFAULT_PAGE_SIZE = 20


def paginate(items, page=1, page_size=None):
    """Return one page of items."""
    size = int(page_size) if page_size is not None else DEFAULT_PAGE_SIZE
    if page < 1:
        raise ValueError("page must be >= 1")
    start = (page - 1) * size
    return items[start:start + size]
`

const paginationV2 = `"""Pagination helpers."""

from .page_size import parse_page_size

DEFAULT_PAGE_SIZE = 20


def paginate(items, page=1, page_size=None):
    """Return one page of items.

    page_size may come straight from a query string, so it is parsed
    strictly: 1-3 ASCII digits, value 1..100.
    """
    size = parse_page_size(page_size) if page_size is not None else DEFAULT_PAGE_SIZE
    if page < 1:
        raise ValueError("page must be >= 1")
    start = (page - 1) * size
    return items[start:start + size]
`

const slugifyA0 = `import re


def slugify(text):
    words = re.findall(r"[a-z0-9]+", text.lower())
    if not words:
        raise ValueError("nothing to slugify")
    return "-".join(words)
`

const slugifyA1 = `"""Text helpers."""
import re
import unicodedata


def slugify(text):
    """Lower-case ASCII slug: words joined by single hyphens."""
    ascii_text = unicodedata.normalize("NFKD", text).encode("ascii", "ignore").decode("ascii")
    words = re.findall(r"[a-z0-9]+", ascii_text.lower())
    if not words:
        raise ValueError("nothing to slugify")
    return "-".join(words)
`

func pageSizeScenario() *scenario {
	return &scenario{
		title: "Strict page-size parsing",
		requirement: "parse_page_size(raw) accepts only strings of 1–3 ASCII digits whose value is between 1 and 100 inclusive, " +
			"and returns that value as an int. Any other input — signs, whitespace, non-ASCII digits, empty strings or " +
			"out-of-range values — raises ValueError. paginate() uses it for user-supplied page sizes.",
		entry: Entrypoint{Language: "python", Path: "src/page_size.py", Function: "parse_page_size"},
		cases: []Case{
			{"ok_1", "1", returns(1)},
			{"ok_20", "20", returns(20)},
			{"ok_100", "100", returns(100)},
			{"zero", "0", raises("ValueError")},
			{"too_big", "101", raises("ValueError")},
			{"four_digits", "1000", raises("ValueError")},
			{"empty", "", raises("ValueError")},
			{"plus", "+1", raises("ValueError")},
			{"space", " 5", raises("ValueError")},
			{"arabic_indic", "٣", raises("ValueError")},
			{"exponent", "1e2", raises("ValueError")},
		},
		plan: "1. Add src/page_size.py with parse_page_size(raw: str) -> int.\n" +
			"2. Accept exactly 1–3 ASCII digits; reject signs, whitespace and non-ASCII digits; range 1..100.\n" +
			"3. Call it from paginate() so page sizes from query strings are validated.",
		a0Files: map[string]string{"src/page_size.py": pageSizeA0},
		a1Files: map[string]string{"src/page_size.py": pageSizeA1, "src/pagination.py": paginationV2},
		a0Fail:  map[string]string{"plus": "returned 1", "space": "returned 5", "arabic_indic": "returned 3"},
		repairNote: "Repair: int() alone accepts '+1', ' 5' and '٣' (Unicode digits), so A0 let them through.\n" +
			"A1 requires a full match of [0-9]{1,3} before converting, and wires parse_page_size into paginate().",
	}
}

func slugifyScenario() *scenario {
	return &scenario{
		title: "slugify(text) helper",
		requirement: "slugify(text) returns a lower-case ASCII slug: accents are transliterated, every run of other characters " +
			"becomes a single hyphen, and there are no leading or trailing hyphens. Text with no letters or digits raises ValueError.",
		entry: Entrypoint{Language: "python", Path: "src/text.py", Function: "slugify"},
		cases: []Case{
			{"simple", "Hello World", returns("hello-world")},
			{"trim", "  Trim me  ", returns("trim-me")},
			{"punctuation", "Hello, World!", returns("hello-world")},
			{"accents", "Déjà vu", returns("deja-vu")},
			{"naive_cafe", "naïve café", returns("naive-cafe")},
			{"repeated", "a -- b", returns("a-b")},
			{"empty", "", raises("ValueError")},
			{"only_symbols", "!!!", raises("ValueError")},
		},
		plan: "1. Add src/text.py with slugify(text: str) -> str.\n" +
			"2. Transliterate to ASCII (NFKD), lower-case, keep runs of [a-z0-9], join with single hyphens.\n" +
			"3. Raise ValueError when nothing is left.",
		a0Files:    map[string]string{"src/text.py": slugifyA0},
		a1Files:    map[string]string{"src/text.py": slugifyA1},
		a0Fail:     map[string]string{"accents": "returned 'd-j-vu'", "naive_cafe": "returned 'na-ve-caf'"},
		repairNote: "Repair: A0 dropped non-ASCII letters instead of transliterating them. A1 normalises with NFKD and drops only the combining marks.",
	}
}

func (sc *scenario) expectText(c Case) string {
	if r, ok := c.Expect["raises"]; ok {
		return fmt.Sprintf("raises %v", r)
	}
	b, _ := json.Marshal(c.Expect["returns"])
	return "returns " + string(b)
}

func (sc *scenario) details(candidate string) ([]CaseDetail, int) {
	out := make([]CaseDetail, 0, len(sc.cases))
	failed := 0
	for i, c := range sc.cases {
		d := CaseDetail{Name: c.Name, Status: "PASS", Input: c.Input, Expected: sc.expectText(c), DurationMS: 3 + (i*7)%17}
		if r, ok := c.Expect["raises"]; ok {
			d.Observed = fmt.Sprintf("raised %v", r)
		} else {
			b, _ := json.Marshal(c.Expect["returns"])
			d.Observed = "returned " + string(b)
		}
		if obs, bad := sc.a0Fail[c.Name]; bad && candidate == "A0" {
			failed++
			d.Status = "FAIL"
			d.Observed = obs
			d.Stderr = fmt.Sprintf("Traceback (most recent call last):\n  File \"/work/.intellectus/run_case.py\", line 41, in <module>\n    check(case)\nAssertionError: %s: expected %s, %s", c.Name, d.Expected, obs)
		}
		out = append(out, d)
	}
	return out, failed
}

// ------------------------------------------------------------------ broker

type broker struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func (b *broker) subscribe() chan []byte {
	ch := make(chan []byte, 512)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broker) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *broker) publish(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("uidev: marshal %s: %v", event, err)
		return
	}
	msg := []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default: // slow client: drop it; EventSource reconnects and refetches
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// ------------------------------------------------------------------ server

type task struct {
	sum         TaskSummary
	requirement string
	entry       Entrypoint
	cases       []Case
	candidates  []Candidate
	assessments []Assessment
	actions     []Action
	report      map[string]any
	sc          *scenario
	baseRoot    string
	a1Root      string
	cancel      context.CancelFunc
}

type server struct {
	token  string
	speed  float64
	broker *broker
	static http.Handler

	mu        sync.Mutex
	status    Status
	events    []Event
	messages  []*Message
	items     []*Item
	tasks     []*task
	taskByID  map[string]*task
	proposals map[string]*ProposalCard
	propSc    map[string]*scenario
	approvals map[string]*ApprovalCard
	roots     map[string]map[string]string
	nMsg      int
	nItem     int
	nProp     int
	nAppr     int
	nAssess   int
	running   int
}

func newServer(token string, speed float64) *server {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		log.Fatal(err)
	}
	s := &server{
		token:     token,
		speed:     speed,
		broker:    &broker{subs: map[chan []byte]struct{}{}},
		static:    http.FileServerFS(sub),
		taskByID:  map[string]*task{},
		proposals: map[string]*ProposalCard{},
		propSc:    map[string]*scenario{},
		approvals: map[string]*ApprovalCard{},
		roots:     map[string]map[string]string{},
	}
	s.seed()
	return s
}

func nowMS() int64 { return time.Now().UnixMilli() }

func strp(s string) *string { return &s }

func digestOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (s *server) registerTree(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	parts := []string{"tree/v1"}
	for _, p := range paths {
		parts = append(parts, p, files[p])
	}
	d := digestOf(parts...)
	s.roots[d] = files
	return d
}

func overlay(base map[string]string, add map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(add))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}

func (s *server) d(sec float64) time.Duration {
	return time.Duration(sec / s.speed * float64(time.Second))
}

// All helpers below require s.mu to be held.

func (s *server) event(typ, actor, summary string) int {
	seq := len(s.events) + 1
	s.events = append(s.events, Event{Sequence: seq, Type: typ, Actor: actor, RecordedAt: nowMS(), Summary: summary})
	s.status.HeadSequence = seq
	return seq
}

func (s *server) pushStatus() {
	s.status.Busy = s.running > 0
	s.broker.publish("status", s.status)
}

func (s *server) addMessage(role, text string, taskID *string, card any) *Message {
	s.nMsg++
	m := &Message{ID: fmt.Sprintf("m-%d", s.nMsg), Role: role, TS: nowMS(), Text: text, TaskID: taskID, Card: card}
	s.messages = append(s.messages, m)
	s.broker.publish("chat", m)
	return m
}

func (s *server) republishCard(card any) {
	for _, m := range s.messages {
		if m.Card == card {
			s.broker.publish("chat", m)
			return
		}
	}
}

func (s *server) addItem(taskID *string, kind, status, title, detail string) *Item {
	s.nItem++
	it := &Item{ID: fmt.Sprintf("act-%d", s.nItem), TS: nowMS(), TaskID: taskID, Kind: kind, Status: status, Title: title, Detail: detail}
	s.items = append(s.items, it)
	s.broker.publish("activity", it)
	return it
}

func (s *server) updateItem(it *Item, status, title, detail string) {
	it.Status = status
	if title != "" {
		it.Title = title
	}
	if detail != "" {
		it.Detail = detail
	}
	s.broker.publish("activity", it)
}

func (s *server) modelCall(in, out, cache int) {
	u := &s.status.Usage
	u.ModelCalls++
	u.InputTokens += in
	u.OutputTokens += out
	u.CacheReadInputTokens += cache
	cost := float64(u.InputTokens)*5/1e6 + float64(u.OutputTokens)*25/1e6 + float64(u.CacheReadInputTokens)*0.5/1e6
	u.EstimatedCostUSD = strconv.FormatFloat(cost, 'f', 4, 64)
}

func (s *server) touchTask(t *task, phase string) {
	if phase != "" {
		t.sum.Phase = phase
	}
	t.sum.UpdatedTS = nowMS()
	s.broker.publish("tasks", map[string]string{"changed": t.sum.TaskID})
}

// ------------------------------------------------------------------- seed

func (s *server) seed() {
	now := nowMS()
	st := &s.status
	st.ProjectID = "pager"
	st.Core.ReducerVersion = 2
	st.Core.Deductor = "cross"
	st.Core.DeductorDigest = "cross(rust-reference/v1," + digestOf("mojo-deductor", "1.1.0") + ")"
	st.Claude.Configured = true
	st.Claude.Provider = "anthropic"
	st.Claude.Models = map[string]string{"intake": coderModel, "planner": coderModel, "coder": coderModel, "tester": coderModel}
	st.Claude.Detail = devBanner
	st.Jev.Configured = true
	st.Jev.PolicyMode = "SHADOW"
	st.Jev.Model = jevModel
	st.Jev.Detail = devBanner
	sb := &st.Sandbox
	sb.Available, sb.Verified, sb.Trusted = true, true, false
	sb.Kind = "bwrap"
	sb.ImplementationDigest = "bwrap-python/v1:" + digestOf("bwrap 0.9.0", "python 3.12.4")
	sb.VerifiedAt = now - 4*60*1000
	sb.Detail = devBanner
	sb.Probes = []Probe{
		{"network_egress_blocked", true, "connect 1.1.1.1:443: OSError [Errno 101] Network is unreachable"},
		{"root_filesystem_readonly", true, "open('/usr/lib/probe', 'w'): PermissionError [Errno 30]"},
		{"home_not_mounted", true, "$HOME is an empty tmpfs"},
		{"pid_namespace", true, "pid 1 inside the sandbox is bwrap"},
		{"no_new_privileges", true, "PR_GET_NO_NEW_PRIVS = 1"},
		{"resource_limits", true, "RLIMIT_AS 512 MiB, RLIMIT_NPROC 64, wall clock 10 s"},
	}
	st.Usage.EstimatedCostUSD = "0.0000"
	st.Usage.JevCostUSD = "0.0000"

	initial := map[string]string{
		"README.md": "# pager\n\nA tiny pagination helper. It is the demo project for INTELLECTUS.\n\n" +
			"```python\nfrom src.pagination import paginate\npaginate(list(range(100)), page=2, page_size=\"20\")\n```\n",
		"pyproject.toml":    "[project]\nname = \"pager\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n\n[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\n",
		"src/__init__.py":   "",
		"src/pagination.py": paginationV1,
	}
	r0 := s.registerTree(initial)
	withTests := overlay(initial, map[string]string{
		"tests/test_pagination.py": "from src.pagination import paginate\n\n\n" +
			"def test_first_page():\n    assert paginate(list(range(50)), 1, 10) == list(range(10))\n\n\n" +
			"def test_second_page():\n    assert paginate(list(range(50)), 2, 10) == list(range(10, 20))\n\n\n" +
			"def test_default_size():\n    assert len(paginate(list(range(50)))) == 20\n\n\n" +
			"def test_past_the_end():\n    assert paginate(list(range(5)), 3, 10) == []\n",
	})
	r1 := s.registerTree(withTests)

	type ev struct{ typ, actor, sum string }
	seedEvents := []ev{
		{"genesis", "core", "project pager initialised (reducer v2, schema v1)"},
		{"operator_command_accepted", operator, "operator key registered"},
		{"policy_set", operator, "policy v1: promote_local requires acceptance_tests + formal_context; trusted issuers: runner:sandbox, core:deductor:cross"},
		{"environment_registered", operator, "environment python-3.12 " + digestOf("env", "python-3.12")[:23] + "…"},
		{"tree_registered", "engine", "tree " + r0[:23] + "… (4 files) registered as approved root"},
		{"session_opened", "core", "session s-1 opened for engine"},
		{"session_opened", "core", "session s-2 opened for role planner"},
		{"session_opened", "core", "session s-3 opened for role coder"},
		{"session_opened", "core", "session s-4 opened for role tester"},
		{"input_recorded", operator, "operator request recorded: add tests for paginate()"},
		{"node_added", operator, "req:1@1 requirement: paginate() has unit tests for first, second, default and past-the-end pages"},
		{"test_manifest_registered", operator, "manifest m-1 (4 cases) registered"},
		{"task_opened", operator, "task:13 opened: Add tests for paginate()"},
		{"proposal_recorded", "role:planner", "prop:14 plan (2 steps)"},
		{"proposal_recorded", "role:coder", "prop:15 candidate A0 (tests/test_pagination.py)"},
		{"tree_registered", "engine", "tree " + r1[:23] + "… registered for prop:15"},
		{"check_requested", "engine", "acceptance_tests on prop:15"},
		{"verification_issued", "runner:sandbox", "acceptance_tests PASS on prop:15 (4/4)"},
		{"context_evaluated", "core:deductor:cross", "formal context ctx:1 consistent (9 facts, 3 rules)"},
		{"admission_rejected", "core", "promote_local for prop:15: MISSING_APPROVAL"},
		{"approval_granted", operator, "approval a-1 bound to action digest " + digestOf("seed-action")[:23] + "…"},
		{"intent_authorized", "core", "act:22 promote_local authorized"},
		{"dispatch_started", "core", "act:22 dispatch started"},
		{"approved_root_advanced", "core", "approved root " + r0[:15] + "… → " + r1[:15] + "…"},
		{"outcome_observed", "core", "act:22 outcome observed: promoted"},
		{"postconditions_evaluated", "core", "act:22 postconditions hold (2/2)"},
		{"task_transition", "core", "task:13 OPEN → COMPLETED (promote_local)"},
	}
	start := now - 3*3600*1000
	for i, e := range seedEvents {
		s.events = append(s.events, Event{Sequence: i + 1, Type: e.typ, Actor: e.actor, RecordedAt: start + int64(i)*37_000, Summary: e.sum})
	}
	st.HeadSequence = len(s.events)
	st.ApprovedRoot = r1

	seedCases := []Case{
		{"test_first_page", "paginate(range(50), 1, 10)", returns("[0..9]")},
		{"test_second_page", "paginate(range(50), 2, 10)", returns("[10..19]")},
		{"test_default_size", "paginate(range(50))", returns("20 items")},
		{"test_past_the_end", "paginate(range(5), 3, 10)", returns("[]")},
	}
	var seedDetails []CaseDetail
	for i, c := range seedCases {
		b, _ := json.Marshal(c.Expect["returns"])
		seedDetails = append(seedDetails, CaseDetail{Name: c.Name, Status: "PASS", Input: c.Input, Expected: "returns " + string(b), Observed: "returned " + string(b), DurationMS: 4 + i})
	}
	t := &task{
		sum: TaskSummary{TaskID: "task:13", Title: "Add tests for paginate()", Status: "COMPLETED", MaxRepairs: 3,
			Candidates: 1, Phase: "done", UpdatedTS: start + 26*37_000},
		requirement: "paginate() has unit tests for the first page, the second page, the default page size and a page past the end.",
		entry:       Entrypoint{Language: "python", Path: "tests/test_pagination.py", Function: "test_*"},
		cases:       seedCases,
		candidates:  []Candidate{{ProposalID: "prop:15", CandidateRoot: r1, Acceptance: "PASS", Details: seedDetails}},
		actions:     []Action{{ActionID: "act:22", ToolID: "promote_local", State: "POSTCONDITIONS_EVALUATED", Postconditions: []Postcondition{{"approved_root_is_candidate", "PASS"}, {"tree_digest_matches", "PASS"}}}},
		baseRoot:    r0,
	}
	t.report = map[string]any{
		"task":          map[string]any{"task_id": "task:13", "status": "COMPLETED", "completed_by": "promote_local", "repairs_used": 0},
		"approved_root": r1,
		"root_history":  []string{r0, r1},
		"verifications": []map[string]any{{"verification_id": "ver:18", "check_kind": "acceptance_tests", "result": "PASS", "issuer": "runner:sandbox", "applicability_limits": []string{"SIMULATED: " + devBanner}}},
		"limitations":   standardLimitations(),
	}
	s.tasks = append(s.tasks, t)
	s.taskByID[t.sum.TaskID] = t

	ago := func(min int) int64 { return now - int64(min)*60*1000 }
	s.items = []*Item{
		{ID: "act-boot-1", TS: ago(6), Kind: "system", Status: "ok", Title: "Core started — reducer v2, deductor cross (rust-reference ⇄ mojo)", Detail: "head #" + strconv.Itoa(len(s.events)) + ", replay verified, state digest matches"},
		{ID: "act-boot-2", TS: ago(5), Kind: "model", Status: "info", Title: "Claude configured — anthropic · " + coderModel + " for intake, planner, coder and tester"},
		{ID: "act-boot-3", TS: ago(5), Kind: "jev", Status: "info", Title: "Jev " + jevModel + " in SHADOW mode — advice is recorded, deterministic routing decides"},
		{ID: "act-boot-4", TS: ago(4), Kind: "sandbox", Status: "ok", Title: "Sandbox probes passed (6/6) — bwrap", Detail: probeDetail(sb.Probes)},
		{ID: "act-boot-5", TS: ago(4), Kind: "sandbox", Status: "warn", Title: "Sandbox implementation is not trusted by policy yet — trust it from the Sandbox pill", Detail: sb.ImplementationDigest},
	}
}

func probeDetail(ps []Probe) string {
	var b strings.Builder
	for _, p := range ps {
		mark := "PASS"
		if !p.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "%s  %-26s %s\n", mark, p.Name, p.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

func standardLimitations() []string {
	return []string{
		"Test results are evidence about the exact recorded runs, not proof of general correctness.",
		"No claim of absence of security vulnerabilities; no production deployment was performed.",
		"Formal checks cover only the bounded ground Horn fragment of the named contexts.",
		devBanner + ": nothing was executed; every result on this page is scripted.",
	}
}

// ----------------------------------------------------------------- routes

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.getStatus)
	mux.HandleFunc("GET /api/chat", s.getChat)
	mux.HandleFunc("POST /api/chat", s.postChat)
	mux.HandleFunc("POST /api/proposals/{id}/approve", s.approveProposal)
	mux.HandleFunc("POST /api/proposals/{id}/discard", s.discardProposal)
	mux.HandleFunc("POST /api/approvals/{id}/approve", s.approveApproval)
	mux.HandleFunc("POST /api/approvals/{id}/reject", s.rejectApproval)
	mux.HandleFunc("GET /api/activity", s.getActivity)
	mux.HandleFunc("GET /api/tasks", s.getTasks)
	mux.HandleFunc("GET /api/tasks/{id}", s.getTask)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.cancelTask)
	mux.HandleFunc("GET /api/tree", s.getTree)
	mux.HandleFunc("GET /api/diff", s.getDiff)
	mux.HandleFunc("GET /api/events", s.getEvents)
	mux.HandleFunc("POST /api/export", s.postExport)
	mux.HandleFunc("POST /api/policy/jev-mode", s.postJevMode)
	mux.HandleFunc("POST /api/policy/trust-sandbox", s.postTrustSandbox)
	mux.HandleFunc("GET /api/stream", s.stream)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	mux.HandleFunc("/", s.serveStatic)
	return s.middleware(mux)
}

func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if s.token != "" && !s.authed(r) {
				writeErr(w, http.StatusUnauthorized, "session missing or expired")
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("X-Intellectus-CSRF") != "1" {
					writeErr(w, http.StatusForbidden, "missing X-Intellectus-CSRF header")
					return
				}
				if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
					writeErr(w, http.StatusForbidden, "cross-origin request refused")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) authed(r *http.Request) bool {
	c, err := r.Cookie("intellectus_session")
	return err == nil && c.Value == s.token
}

func (s *server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" {
		if tok := r.URL.Query().Get("token"); tok != "" && r.URL.Path == "/" {
			if tok != s.token {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "intellectus_session", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if r.URL.Path == "/" && !s.authed(r) {
			http.Error(w, "Open the URL printed by uidev (with ?token=…).", http.StatusUnauthorized)
			return
		}
	}
	s.static.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("uidev: write: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(body, v)
}

// -------------------------------------------------------------- handlers

func (s *server) getStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Busy = s.running > 0
	writeJSON(w, http.StatusOK, s.status)
}

func (s *server) getChat(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.messages
	if msgs == nil {
		msgs = []*Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *server) postChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	s.mu.Lock()
	m := s.addMessage("user", text, nil, nil)
	s.event("input_recorded", operator, "operator chat message "+m.ID+" recorded ("+strconv.Itoa(len(text))+" chars)")
	s.running++
	s.pushStatus()
	s.mu.Unlock()
	go s.reply(text)
	writeJSON(w, http.StatusOK, map[string]string{"message_id": m.ID})
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// reply scripts the intake step for one chat message.
func (s *server) reply(text string) {
	lower := strings.ToLower(text)
	var sc *scenario
	question := false
	switch {
	case strings.Contains(lower, "slug"):
		sc = slugifyScenario()
	case strings.Contains(lower, "file") && (strings.Contains(lower, "?") || strings.HasPrefix(lower, "what") || strings.HasPrefix(lower, "list") || strings.HasPrefix(lower, "which")):
		question = true
	default:
		sc = pageSizeScenario()
	}

	time.Sleep(s.d(0.3))
	s.mu.Lock()
	it := s.addItem(nil, "model", "running", "Intake ("+coderModel+") is reading your request", "Request (untrusted, shown as text):\n"+clip(text, 2000))
	s.mu.Unlock()

	time.Sleep(s.d(1.3))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelCall(3800, 900, 2400)
	s.running--
	if question {
		files := s.roots[s.status.ApprovedRoot]
		paths := make([]string, 0, len(files))
		for p := range files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		s.updateItem(it, "ok", "Intake answered a question — no task needed", "")
		s.addMessage("assistant", fmt.Sprintf("The approved tree `%s` has %d files:\n\n```\n%s\n```\n\nOpen the Files tab to read them, or ask me to change something.",
			clip(s.status.ApprovedRoot, 19), len(paths), strings.Join(paths, "\n")), nil, nil)
		s.pushStatus()
		return
	}
	s.nProp++
	pid := fmt.Sprintf("p-%d", s.nProp)
	card := &ProposalCard{Type: "proposal", ProposalID: pid, Status: "pending", Title: sc.title, Requirement: sc.requirement, Entrypoint: sc.entry, Cases: sc.cases}
	s.proposals[pid] = card
	s.propSc[pid] = sc
	s.updateItem(it, "ok", "Intake drafted proposal "+pid+": "+sc.title, "")
	s.event("proposal_recorded", "role:intake", "draft proposal "+pid+" ("+strconv.Itoa(len(sc.cases))+" acceptance tests) — not yet signed")
	s.addMessage("assistant", "Here is how I understood your request:\n“"+clip(text, 280)+"”\n\n"+
		"I drafted the proposal below: `"+sc.entry.Function+"` in `"+sc.entry.Path+"`, checked by "+strconv.Itoa(len(sc.cases))+
		" acceptance tests. Nothing runs until you approve it.", nil, card)
	s.pushStatus()
}

func (s *server) approveProposal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	card, ok := s.proposals[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown proposal "+id)
		return
	}
	if card.Status != "pending" {
		writeErr(w, http.StatusConflict, "proposal "+id+" is already "+card.Status)
		return
	}
	sc := s.propSc[id]
	s.event("operator_command_accepted", operator, "operator signed requirement + "+strconv.Itoa(len(sc.cases))+" acceptance tests for "+id)
	s.event("node_added", operator, "requirement node added: "+sc.title)
	s.event("test_manifest_registered", operator, "manifest for "+id+" ("+strconv.Itoa(len(sc.cases))+" cases)")
	seq := s.event("task_opened", operator, "task opened: "+sc.title)
	tid := fmt.Sprintf("task:%d", seq)
	s.events[seq-1].Summary = tid + " opened: " + sc.title
	card.Status = "approved"
	card.TaskID = strp(tid)
	t := &task{
		sum:         TaskSummary{TaskID: tid, Title: sc.title, Status: "OPEN", MaxRepairs: 3, Phase: "planning", UpdatedTS: nowMS()},
		requirement: sc.requirement, entry: sc.entry, cases: sc.cases, sc: sc, baseRoot: s.status.ApprovedRoot,
		candidates: []Candidate{}, assessments: []Assessment{}, actions: []Action{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	s.tasks = append(s.tasks, t)
	s.taskByID[tid] = t
	s.republishCard(card)
	s.addItem(strp(tid), "core", "ok", "Operator signed the requirement and "+strconv.Itoa(len(sc.cases))+" acceptance tests — "+tid+" opened", "")
	s.running++
	s.touchTask(t, "")
	s.pushStatus()
	go s.runTask(ctx, t)
	writeJSON(w, http.StatusOK, map[string]string{"task_id": tid})
}

func (s *server) discardProposal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	card, ok := s.proposals[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown proposal "+id)
		return
	}
	if card.Status != "pending" {
		writeErr(w, http.StatusConflict, "proposal "+id+" is already "+card.Status)
		return
	}
	card.Status = "discarded"
	s.republishCard(card)
	s.addItem(nil, "system", "info", "Proposal "+id+" discarded by the operator — nothing was started", "")
	writeJSON(w, http.StatusOK, map[string]any{})
}

// runTask plays the scripted timeline for one task (about 11 s at speed 1).
func (s *server) runTask(ctx context.Context, t *task) {
	tid := strp(t.sum.TaskID)
	sc := t.sc
	n := strconv.Itoa(len(sc.cases))
	done := false
	defer func() {
		if !done {
			s.mu.Lock()
			s.running--
			s.pushStatus()
			s.mu.Unlock()
		}
	}()
	// step waits, then runs f under the lock unless the task stopped meanwhile.
	step := func(sec float64, f func()) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(s.d(sec)):
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if t.sum.Status != "OPEN" {
			return false
		}
		f()
		s.pushStatus()
		return true
	}
	var planner, coder, sandbox, formal *Item
	var a0Prop, a1Prop string
	var a0Root, a1Root string
	steps := []struct {
		sec float64
		f   func()
	}{
		{0.4, func() {
			planner = s.addItem(tid, "model", "running", "Planner ("+coderModel+") is drafting a plan", "")
			s.touchTask(t, "planning")
		}},
		{1.6, func() {
			s.modelCall(5200, 700, 3100)
			seq := s.event("proposal_recorded", "role:planner", "plan for "+t.sum.TaskID+" (3 steps)")
			s.updateItem(planner, "ok", "Planner produced a 3-step plan (prop:"+strconv.Itoa(seq)+")", sc.plan)
		}},
		{0.3, func() {
			coder = s.addItem(tid, "model", "running", "Coder ("+coderModel+") is writing candidate A0", "")
			s.touchTask(t, "coding")
		}},
		{1.5, func() {
			s.modelCall(7400, 1300, 5200)
			seq := s.event("proposal_recorded", "role:coder", "candidate A0 for "+t.sum.TaskID)
			a0Prop = "prop:" + strconv.Itoa(seq)
			a0Root = s.registerTree(overlay(s.roots[t.baseRoot], sc.a0Files))
			s.event("tree_registered", "engine", "tree "+a0Root[:23]+"… registered for "+a0Prop)
			t.candidates = append(t.candidates, Candidate{ProposalID: a0Prop, CandidateRoot: a0Root, Acceptance: "PENDING", Details: []CaseDetail{}})
			t.sum.Candidates = len(t.candidates)
			s.updateItem(coder, "ok", "Coder submitted candidate A0 ("+a0Prop+") — "+sc.entry.Path, sc.a0Files[sc.entry.Path])
			s.touchTask(t, "")
		}},
		{0.3, func() {
			s.event("check_requested", "engine", "acceptance_tests on "+a0Prop)
			sandbox = s.addItem(tid, "sandbox", "running", "Running "+n+" acceptance tests on A0 in the bwrap sandbox", "")
			if !s.status.Sandbox.Trusted {
				s.addItem(tid, "sandbox", "warn", "Sandbox implementation is not trusted by policy — real receipts from it would be refused (fixture continues)", s.status.Sandbox.ImplementationDigest)
			}
			s.touchTask(t, "testing")
		}},
		{1.4, func() {
			details, failed := sc.details("A0")
			t.candidates[0].Details = details
			t.candidates[0].Acceptance = "FAIL"
			t.sum.FailedCandidates = 1
			var b strings.Builder
			for _, d := range details {
				if d.Status != "PASS" {
					in, _ := json.Marshal(d.Input)
					fmt.Fprintf(&b, "FAIL  %-13s input %-8s expected %-18s observed %s\n", d.Name, string(in), d.Expected, d.Observed)
				}
			}
			fmt.Fprintf(&b, "%d other cases passed.", len(details)-failed)
			s.event("verification_issued", "runner:sandbox", fmt.Sprintf("acceptance_tests FAIL on %s (%d/%d)", a0Prop, len(details)-failed, len(details)))
			s.updateItem(sandbox, "fail", fmt.Sprintf("A0 failed %d of %d acceptance tests", failed, len(details)), b.String())
			s.addItem(tid, "core", "info", "Core recorded the FAIL receipt — deterministic route: REPAIR (0/3 repairs used)", "failure_codes: [CHECK_FAILED]")
			s.touchTask(t, "routing")
		}},
		{0.5, func() {
			mode := s.status.Jev.PolicyMode
			s.nAssess++
			as := Assessment{AssessmentID: fmt.Sprintf("asm:%d", len(s.events)+1), AppliedChoice: "REPAIR", Mode: mode}
			var title, detail, status string
			switch mode {
			case "OFF":
				as.FallbackReason = strp("ADVISOR_OFF")
				title, status = "Jev is off — deterministic route REPAIR applied", "info"
				detail = "choice: —\napplied_choice: REPAIR\nfallback_reason: ADVISOR_OFF"
			case "LIVE":
				as.Choice, as.ConfidenceBP, as.UsedAdvisor = strp("REPAIR"), intp(8200), true
				title, status = "Jev (LIVE) chose REPAIR at 82% confidence — applied", "ok"
				detail = "choice: REPAIR\nconfidence: 82.0%\napplied_choice: REPAIR\nused_advisor: true"
				s.status.Usage.JevCalls++
			default:
				as.Choice, as.ConfidenceBP, as.FallbackReason = strp("REPAIR"), intp(8200), strp("SHADOW_MODE")
				title, status = "Jev (SHADOW) advised REPAIR at 82% — recorded only; deterministic route applied", "info"
				detail = "choice: REPAIR\nconfidence: 82.0%\napplied_choice: REPAIR (deterministic)\nfallback_reason: SHADOW_MODE"
				s.status.Usage.JevCalls++
			}
			t.assessments = append(t.assessments, as)
			s.event("assessment_recorded", "engine", "assessment for "+t.sum.TaskID+": applied REPAIR (mode "+mode+")")
			s.addItem(tid, "jev", status, title, detail)
			s.touchTask(t, "")
		}},
		{0.4, func() {
			t.sum.RepairsUsed = 1
			coder = s.addItem(tid, "model", "running", "Coder ("+coderModel+") is repairing A0 → A1 (repair 1/3)", "")
			s.touchTask(t, "coding")
		}},
		{1.5, func() {
			s.modelCall(9100, 1500, 7300)
			seq := s.event("proposal_recorded", "role:coder", "candidate A1 (repair of "+a0Prop+")")
			a1Prop = "prop:" + strconv.Itoa(seq)
			a1Root = s.registerTree(overlay(s.roots[t.baseRoot], sc.a1Files))
			t.a1Root = a1Root
			s.event("tree_registered", "engine", "tree "+a1Root[:23]+"… registered for "+a1Prop)
			t.candidates = append(t.candidates, Candidate{ProposalID: a1Prop, CandidateRoot: a1Root, Acceptance: "PENDING", Details: []CaseDetail{}})
			t.sum.Candidates = len(t.candidates)
			s.updateItem(coder, "ok", "Coder submitted candidate A1 ("+a1Prop+")", sc.repairNote+"\n\n"+sc.a1Files[sc.entry.Path])
			s.touchTask(t, "")
		}},
		{0.3, func() {
			s.event("check_requested", "engine", "acceptance_tests on "+a1Prop)
			sandbox = s.addItem(tid, "sandbox", "running", "Running "+n+" acceptance tests on A1 in the bwrap sandbox", "")
			s.touchTask(t, "testing")
		}},
		{1.3, func() {
			details, _ := sc.details("A1")
			t.candidates[1].Details = details
			t.candidates[1].Acceptance = "PASS"
			s.event("verification_issued", "runner:sandbox", "acceptance_tests PASS on "+a1Prop+" ("+n+"/"+n+")")
			s.updateItem(sandbox, "ok", "A1 passed "+n+"/"+n+" acceptance tests", "all "+n+" cases passed · network egress blocked · read-only root · 10 s limit")
			s.touchTask(t, "")
		}},
		{0.3, func() {
			formal = s.addItem(tid, "core", "running", "Checking the formal context (deductor cross: rust-reference ⇄ mojo)", "")
		}},
		{0.8, func() {
			s.event("context_evaluated", "core:deductor:cross", "formal context consistent for "+a1Prop+" (14 facts, 6 rules)")
			s.updateItem(formal, "ok", "Formal context consistent — both deductors agree, no contradiction", "facts: 14\nrules: 6\nderived: 9\ncontradictions: 0\nrust-reference and mojo fixpoints identical")
		}},
		{0.4, func() {
			seq := s.event("admission_rejected", "core", "promote_local for "+a1Prop+": MISSING_APPROVAL")
			actionID := "act:" + strconv.Itoa(seq)
			t.actions = append(t.actions, Action{ActionID: actionID, ToolID: "promote_local", State: "AWAITING_APPROVAL", Postconditions: []Postcondition{}})
			s.nAppr++
			aid := fmt.Sprintf("a-%d", s.nAppr)
			card := &ApprovalCard{
				Type: "approval", ApprovalID: aid, Status: "pending", TaskID: tid, ToolID: "promote_local",
				Title:        "Promote candidate A1 to the approved tree",
				ActionDigest: digestOf("promote_local", t.baseRoot, a1Root, a1Prop),
				FromRoot:     t.baseRoot,
				ToRoot:       a1Root,
				Checks: []Check{
					{Kind: "acceptance_tests", Result: "PASS", Issuer: "runner:sandbox", Detail: n + "/" + n + " cases passed in bwrap sandbox"},
					{Kind: "formal_context", Result: "PASS", Issuer: "core:deductor:cross", Detail: "spec consistent"},
				},
				Summary: "Candidate A1 (" + a1Prop + ") passed every required check. Approving signs this exact transition; the gateway then promotes it and the core re-checks the postconditions.",
			}
			s.approvals[aid] = card
			s.addItem(tid, "approval", "info", "Waiting for your approval to promote A1 ("+aid+")", "")
			s.addMessage("assistant", "Candidate A1 passed all "+n+" acceptance tests and the formal context check. Promotion needs your approval:", tid, card)
			s.running--
			done = true
			s.touchTask(t, "awaiting_approval")
		}},
	}
	for _, st := range steps {
		if !step(st.sec, st.f) {
			return
		}
	}
}

func intp(v int) *int { return &v }

func (s *server) approveApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	card, ok := s.approvals[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown approval "+id)
		return
	}
	if card.Status != "pending" {
		writeErr(w, http.StatusConflict, "approval "+id+" is already "+card.Status)
		return
	}
	if card.FromRoot != s.status.ApprovedRoot {
		card.Status = "stale"
		s.republishCard(card)
		s.addItem(card.TaskID, "approval", "warn", "Approval "+id+" is stale — the approved root changed since it was proposed", "")
		writeErr(w, http.StatusConflict, "approval "+id+" is stale: the approved root changed (BASE_ROOT_CHANGED)")
		return
	}
	var t *task
	if card.TaskID != nil {
		t = s.taskByID[*card.TaskID]
		if t != nil && t.sum.Status != "OPEN" {
			writeErr(w, http.StatusConflict, "task "+t.sum.TaskID+" is "+t.sum.Status)
			return
		}
	}
	card.Status = "approved"
	s.republishCard(card)
	s.event("operator_command_accepted", operator, "operator approval for "+id)
	s.event("approval_granted", operator, "approval "+id+" bound to action digest "+card.ActionDigest[:23]+"…")
	s.addItem(card.TaskID, "approval", "ok", "Operator approved "+id+" (bound to action digest "+card.ActionDigest[7:19]+"…)", "action_digest: "+card.ActionDigest)
	switch {
	case card.ToolID == "export_view":
		s.running++
		go s.runExport(card)
	case t != nil:
		s.running++
		go s.runPromotion(t, card)
	}
	s.pushStatus()
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *server) runPromotion(t *task, card *ApprovalCard) {
	tid := strp(t.sum.TaskID)
	time.Sleep(s.d(0.4))
	s.mu.Lock()
	act := &t.actions[len(t.actions)-1]
	s.event("intent_authorized", "core", act.ActionID+" promote_local authorized")
	s.event("dispatch_started", "core", act.ActionID+" dispatch started")
	act.State = "DISPATCH_STARTED"
	gw := s.addItem(tid, "gateway", "running", "Promoting A1 to the approved tree", "")
	s.touchTask(t, "promoting")
	s.pushStatus()
	s.mu.Unlock()

	time.Sleep(s.d(1.2))
	s.mu.Lock()
	old := s.status.ApprovedRoot
	s.status.ApprovedRoot = card.ToRoot
	s.event("approved_root_advanced", "core", "approved root "+old[:15]+"… → "+card.ToRoot[:15]+"…")
	s.event("outcome_observed", "core", act.ActionID+" outcome observed: promoted")
	act.State = "OUTCOME_OBSERVED"
	s.updateItem(gw, "ok", "Approved root advanced "+old[7:17]+"… → "+card.ToRoot[7:17]+"…", "from: "+old+"\nto:   "+card.ToRoot)
	s.pushStatus()
	s.mu.Unlock()

	time.Sleep(s.d(0.5))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.event("postconditions_evaluated", "core", act.ActionID+" postconditions hold (2/2)")
	s.event("task_transition", "core", t.sum.TaskID+" OPEN → COMPLETED (promote_local)")
	act.State = "POSTCONDITIONS_EVALUATED"
	act.Postconditions = []Postcondition{{"approved_root_is_candidate", "PASS"}, {"tree_digest_matches", "PASS"}}
	s.addItem(tid, "core", "ok", "Postconditions hold (2/2) — "+t.sum.TaskID+" completed", "approved_root_is_candidate: PASS\ntree_digest_matches: PASS")
	t.sum.Status = "COMPLETED"
	sc := t.sc
	n := strconv.Itoa(len(sc.cases))
	a0Failed := 0
	if len(t.candidates) > 0 {
		for _, d := range t.candidates[0].Details {
			if d.Status != "PASS" {
				a0Failed++
			}
		}
	}
	jevLine := "Jev: no assessment recorded"
	if len(t.assessments) > 0 {
		a := t.assessments[0]
		switch {
		case a.UsedAdvisor:
			jevLine = "Jev (LIVE) chose REPAIR at 82%; its choice was applied"
		case a.Choice != nil:
			jevLine = "Jev (" + a.Mode + ") advised REPAIR at 82%; the deterministic route was applied"
		default:
			jevLine = "Jev was off; the deterministic route REPAIR was applied"
		}
	}
	lines := []string{
		"Promoted candidate A1 (" + t.candidates[len(t.candidates)-1].ProposalID + ") to the approved tree.",
		"Acceptance tests: " + n + "/" + n + " PASS in the bwrap sandbox (A0 failed " + strconv.Itoa(a0Failed) + "/" + n + " and was repaired once).",
		"Formal context: consistent (deductor cross).",
		jevLine + ".",
	}
	var vers []map[string]any
	for _, c := range t.candidates {
		vers = append(vers, map[string]any{"verification_id": "ver:" + strings.TrimPrefix(c.ProposalID, "prop:"), "check_kind": "acceptance_tests",
			"result": c.Acceptance, "issuer": "runner:sandbox", "subject": map[string]string{"proposal_id": c.ProposalID, "candidate_root": c.CandidateRoot},
			"applicability_limits": []string{"SIMULATED: " + devBanner}, "used_for_authorization": c.Acceptance == "PASS"})
	}
	t.report = map[string]any{
		"task":                 map[string]any{"task_id": t.sum.TaskID, "status": "COMPLETED", "completed_by": "promote_local", "repairs_used": t.sum.RepairsUsed},
		"approved_root":        card.ToRoot,
		"root_history":         []string{t.baseRoot, card.ToRoot},
		"actions":              t.actions,
		"verifications":        vers,
		"assessments":          t.assessments,
		"admission_rejections": []map[string]any{{"proposal": t.candidates[len(t.candidates)-1].ProposalID + "@1", "reasons": []string{"MISSING_APPROVAL"}}},
		"limitations":          standardLimitations(),
		"snapshot_sequence":    len(s.events),
		"state_digest":         digestOf("state", strconv.Itoa(len(s.events))),
	}
	s.addMessage("assistant", "", tid, &ReportCard{Type: "report", TaskID: t.sum.TaskID, Status: "COMPLETED", ApprovedRoot: card.ToRoot, Lines: lines, Limitations: standardLimitations()})
	s.running--
	s.touchTask(t, "done")
	s.pushStatus()
}

func (s *server) runExport(card *ApprovalCard) {
	time.Sleep(s.d(0.4))
	s.mu.Lock()
	gw := s.addItem(nil, "gateway", "running", "Exporting the approved tree to ./intellectus-export", "")
	s.event("dispatch_started", "core", "export_view dispatch started")
	s.pushStatus()
	s.mu.Unlock()
	time.Sleep(s.d(1.2))
	s.mu.Lock()
	defer s.mu.Unlock()
	nfiles := len(s.roots[card.FromRoot])
	s.event("outcome_observed", "core", "export_view outcome observed: "+strconv.Itoa(nfiles)+" files written")
	s.updateItem(gw, "ok", "Exported "+strconv.Itoa(nfiles)+" files to ./intellectus-export (simulated)", "root: "+card.FromRoot)
	s.addMessage("system", "Export finished: "+strconv.Itoa(nfiles)+" files written to `./intellectus-export` (simulated).", nil, nil)
	s.running--
	s.pushStatus()
}

func (s *server) rejectApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	card, ok := s.approvals[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown approval "+id)
		return
	}
	if card.Status != "pending" {
		writeErr(w, http.StatusConflict, "approval "+id+" is already "+card.Status)
		return
	}
	card.Status = "rejected"
	s.republishCard(card)
	s.addItem(card.TaskID, "approval", "warn", "Operator rejected "+id+" — nothing was changed", "")
	if card.TaskID != nil {
		if t := s.taskByID[*card.TaskID]; t != nil && t.sum.Status == "OPEN" {
			t.sum.Status = "STOPPED"
			s.event("task_transition", operator, t.sum.TaskID+" OPEN → STOPPED (approval rejected)")
			s.addMessage("assistant", "", card.TaskID, &ReportCard{Type: "report", TaskID: t.sum.TaskID, Status: "STOPPED", ApprovedRoot: s.status.ApprovedRoot,
				Lines: []string{"The operator rejected the promotion; the approved tree is unchanged."}, Limitations: standardLimitations()})
			s.touchTask(t, "done")
		}
	}
	s.pushStatus()
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *server) getActivity(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 1000)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.items
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	if items == nil {
		items = []*Item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *server) getTasks(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskSummary, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t.sum)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

func (s *server) getTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.taskByID[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown task "+id)
		return
	}
	var report any
	if t.report != nil {
		report = t.report
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task": t.sum, "requirement": t.requirement, "entrypoint": t.entry, "cases": t.cases,
		"candidates": t.candidates, "assessments": t.assessments, "actions": t.actions, "report": report,
	})
}

func (s *server) cancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.taskByID[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown task "+id)
		return
	}
	if t.sum.Status != "OPEN" {
		writeErr(w, http.StatusConflict, "task "+id+" is "+t.sum.Status+", not OPEN")
		return
	}
	t.sum.Status = "CANCELLED"
	if t.cancel != nil {
		t.cancel()
	}
	for _, it := range s.items {
		if it.TaskID != nil && *it.TaskID == id && it.Status == "running" {
			s.updateItem(it, "warn", it.Title+" — cancelled", "")
		}
	}
	for _, c := range s.approvals {
		if c.TaskID != nil && *c.TaskID == id && c.Status == "pending" {
			c.Status = "stale"
			s.republishCard(c)
		}
	}
	s.event("task_transition", operator, id+" OPEN → CANCELLED (operator)")
	s.addItem(strp(id), "core", "warn", id+" cancelled by the operator", "")
	s.addMessage("assistant", "", strp(id), &ReportCard{Type: "report", TaskID: id, Status: "CANCELLED", ApprovedRoot: s.status.ApprovedRoot,
		Lines: []string{"Cancelled by the operator; the approved tree is unchanged."}, Limitations: standardLimitations()})
	s.touchTask(t, "done")
	s.pushStatus()
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *server) getTree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root := r.URL.Query().Get("root")
	if root == "" {
		root = s.status.ApprovedRoot
	}
	files, ok := s.roots[root]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown root "+root)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root": root, "files": files})
}

func (s *server) getDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if from == "" || to == "" {
		writeErr(w, http.StatusBadRequest, "from and to are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, okA := s.roots[from]
	b, okB := s.roots[to]
	if !okA || !okB {
		writeErr(w, http.StatusNotFound, "unknown root")
		return
	}
	paths := map[string]bool{}
	for p := range a {
		paths[p] = true
	}
	for p := range b {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	type fileDiff struct {
		Path   string `json:"path"`
		Status string `json:"status"`
		Old    string `json:"old"`
		New    string `json:"new"`
	}
	out := []fileDiff{}
	for _, p := range sorted {
		ov, inA := a[p]
		nv, inB := b[p]
		switch {
		case inA && !inB:
			out = append(out, fileDiff{p, "removed", ov, ""})
		case !inA && inB:
			out = append(out, fileDiff{p, "added", "", nv})
		case ov != nv:
			out = append(out, fileDiff{p, "modified", ov, nv})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "files": out})
}

func (s *server) getEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.Atoi(q.Get("after"))
	limit := 100
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if e.Sequence > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *server) postExport(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root := s.status.ApprovedRoot
	s.nAppr++
	aid := fmt.Sprintf("a-%d", s.nAppr)
	card := &ApprovalCard{
		Type: "approval", ApprovalID: aid, Status: "pending", ToolID: "export_view",
		Title:        "Export the approved tree to ./intellectus-export",
		ActionDigest: digestOf("export_view", root, aid),
		FromRoot:     root,
		ToRoot:       root,
		Checks:       []Check{{Kind: "approved_tree", Result: "PASS", Issuer: "core", Detail: "root is the current approved root"}},
		Summary:      "Writes every file of the approved tree to the export directory. It is an external effect, so it needs its own approval.",
	}
	s.approvals[aid] = card
	s.event("proposal_recorded", operator, "export_view proposed for "+root[:23]+"…")
	s.addItem(nil, "approval", "info", "Export proposed — waiting for your approval ("+aid+")", "")
	s.addMessage("assistant", "You asked to export the approved tree. Approve to write it to the export directory:", nil, card)
	s.pushStatus()
	writeJSON(w, http.StatusOK, map[string]string{"approval_id": aid})
}

func (s *server) postJevMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	mode := strings.ToUpper(body.Mode)
	if mode != "SHADOW" && mode != "LIVE" && mode != "OFF" {
		writeErr(w, http.StatusBadRequest, "mode must be SHADOW, LIVE or OFF")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.status.Jev.PolicyMode
	s.status.Jev.PolicyMode = mode
	s.event("policy_set", operator, "jev policy mode "+prev+" → "+mode)
	s.addItem(nil, "jev", "info", "Jev policy mode set to "+mode+" (operator-signed policy change)", "")
	s.pushStatus()
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *server) postTrustSandbox(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := &s.status.Sandbox
	if !sb.Available || !sb.Verified {
		writeErr(w, http.StatusConflict, "sandbox is not verified")
		return
	}
	sb.Trusted = true
	s.event("policy_set", operator, "trusted sandbox implementation "+sb.ImplementationDigest)
	s.addItem(nil, "core", "ok", "Policy updated: sandbox implementation trusted (operator-signed)", sb.ImplementationDigest)
	s.pushStatus()
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ch := s.broker.subscribe()
	defer s.broker.unsubscribe(ch)
	io.WriteString(w, "retry: 2000\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := io.WriteString(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
