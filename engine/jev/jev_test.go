package jev_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakeapi"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakecore"
	"github.com/alaindgonz-cell/intellectus/engine/jev"
)

const fakeKey = "ts-test-FAKE-KEY-1234"

// TestMain clears every Jev-related variable so no test can pick up real
// credentials or endpoints from the environment.
func TestMain(m *testing.M) {
	for _, k := range []string{"TYPESAFE_API_KEY", "OPENROUTER_API_KEY", "JEV_PROVIDER", "JEV_BASE_URL", "JEV_MODEL"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

func question() advisor.Question {
	return advisor.Question{
		TaskID:       "task:42",
		Eligible:     []string{"GATHER_CONTEXT", "REPAIR", "ESCALATE"},
		FailureCodes: []string{"acceptance_tests:FAIL", "unit:FAIL"},
		View:         []byte(`{ "task": "task:42", "failures": [ "TestFoo" ] }`),
	}
}

// liveBody is shaped like the response the community harness recorded.
var liveBody = []byte(`{"model":"jev-1.13.0","answers":{"route":{"type":"choice","choice":"REPAIR","confidence":0.98,` +
	`"probabilities":{"REPAIR":0.96,"GATHER_CONTEXT":0.03,"ESCALATE":0.01}}},"usage":{"input_tokens":518,"output_tokens":69},"cost":"0"}`)

type sleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *sleeper) Sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays = append(s.delays, d)
	return nil
}

func startJev(t *testing.T, script func(fakeapi.JevRequest) (int, any)) *fakeapi.Jev {
	t.Helper()
	f := fakeapi.NewJev(script)
	t.Cleanup(f.Close)
	return f
}

func typesafeClient(f *fakeapi.Jev, s *sleeper, mut ...func(*jev.Config)) *jev.Client {
	cfg := jev.Config{Provider: "typesafe", APIKey: fakeKey, BaseURL: f.URL() + "/v1/systemone", Sleep: s.Sleep}
	for _, m := range mut {
		m(&cfg)
	}
	return jev.New(cfg)
}

func TestAdviseHappyPath(t *testing.T) {
	f := startJev(t, func(fakeapi.JevRequest) (int, any) { return 200, liveBody })
	s := &sleeper{}
	adv, err := typesafeClient(f, s).Advise(context.Background(), question())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(liveBody)
	want := advisor.Advice{
		Choice:          "REPAIR",
		ProbabilitiesBP: map[string]int64{"REPAIR": 9600, "GATHER_CONTEXT": 300, "ESCALATE": 100},
		ConfidenceBP:    9800,
		ModelRequested:  "jev-1.13.0",
		ModelReturned:   "jev-1.13.0",
		Mode:            "LIVE",
		Usage:           map[string]int64{"input_tokens": 518, "output_tokens": 69},
		ProviderRef:     "jev:sha256:" + hex.EncodeToString(sum[:]),
		CostUSD:         "0",
		Raw:             liveBody,
	}
	if !reflect.DeepEqual(adv, want) {
		t.Fatalf("advice\n got %+v\nwant %+v", adv, want)
	}

	reqs := f.Requests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != "POST" || r.Path != "/v1/systemone" {
		t.Fatalf("endpoint %s %s", r.Method, r.Path)
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer "+fakeKey {
		t.Fatalf("headers: %v", r.Header)
	}
	if r.Header.Get("HTTP-Referer") != "" || r.Header.Get("X-Title") != "" {
		t.Fatal("OpenRouter headers must not be sent to TypeSafe")
	}
	keys := make([]string, 0, len(r.Fields))
	for k := range r.Fields {
		keys = append(keys, k)
	}
	if len(keys) != 3 || r.Fields["model"] != "jev-1.13.0" {
		t.Fatalf("body keys %v, model %v; want exactly model, state, questions", keys, r.Fields["model"])
	}
	wantQ := map[string]fakeapi.JevQuestion{"route": {
		Type:         "choice",
		Instructions: "You are routing an automated software-repair loop after a failed attempt. Choose the single most useful next step.",
		Criteria: map[string]string{
			"GATHER_CONTEXT": "Collect more information (failure analysis, relevant files) before changing code again",
			"REPAIR":         "Fix the current candidate directly using the reported test failures",
			"ESCALATE":       "Stop and ask the human operator for guidance",
		},
	}}
	if !reflect.DeepEqual(r.Questions, wantQ) {
		t.Fatalf("questions\n got %+v\nwant %+v", r.Questions, wantQ)
	}
	for _, s := range []string{"task_id: task:42", "failure_codes: acceptance_tests:FAIL, unit:FAIL",
		"eligible_options: GATHER_CONTEXT, REPAIR, ESCALATE", `{"task":"task:42","failures":["TestFoo"]}`} {
		if !strings.Contains(r.State, s) {
			t.Errorf("state lacks %q:\n%s", s, r.State)
		}
	}
	if len(s.delays) != 0 {
		t.Fatalf("no retry expected, slept %v", s.delays)
	}
}

func TestProbabilitiesNormalizedToBasisPoints(t *testing.T) {
	body := `{"model":"jev-1.13.0","answers":{"route":{"type":"choice","choice":"REPAIR","confidence":0.5,` +
		`"probabilities":{"REPAIR":0.3333,"GATHER_CONTEXT":0.3333,"ESCALATE":0.3333}}},"cost":0.000042}`
	f := startJev(t, func(fakeapi.JevRequest) (int, any) { return 200, body })
	adv, err := typesafeClient(f, &sleeper{}).Advise(context.Background(), question())
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, v := range adv.ProbabilitiesBP {
		total += v
	}
	if total != 10000 || adv.ConfidenceBP != 5000 || adv.CostUSD != "0.000042" || len(adv.Usage) != 0 {
		t.Fatalf("advice %+v (sum %d)", adv, total)
	}
	// Largest remainder with name tie-break: ESCALATE gets the extra point.
	if !reflect.DeepEqual(adv.ProbabilitiesBP, map[string]int64{"ESCALATE": 3334, "GATHER_CONTEXT": 3333, "REPAIR": 3333}) {
		t.Fatalf("bp = %v", adv.ProbabilitiesBP)
	}
}

func TestMalformedResponsesAreErrors(t *testing.T) {
	route := func(inner string) string {
		return `{"model":"jev-1.13.0","answers":{"route":` + inner + `}}`
	}
	cases := map[string]string{
		"not json":              `<html>oops</html>`,
		"trailing data":         string(liveBody) + `{}`,
		"array":                 `[]`,
		"no model":              `{"answers":{"route":{"type":"choice","choice":"REPAIR","confidence":0.9}}}`,
		"answers not object":    `{"model":"m","answers":[]}`,
		"route missing":         `{"model":"m","answers":{"other":{}}}`,
		"wrong type":            route(`{"type":"score","score":1.2,"confidence":0.9}`),
		"choice not string":     route(`{"type":"choice","choice":1,"confidence":0.9}`),
		"choice not eligible":   route(`{"type":"choice","choice":"STOP","confidence":0.9}`),
		"choice unknown":        route(`{"type":"choice","choice":"DEPLOY","confidence":0.9}`),
		"confidence missing":    route(`{"type":"choice","choice":"REPAIR"}`),
		"confidence > 1":        route(`{"type":"choice","choice":"REPAIR","confidence":1.5}`),
		"confidence string":     route(`{"type":"choice","choice":"REPAIR","confidence":"0.9"}`),
		"confidence negative":   route(`{"type":"choice","choice":"REPAIR","confidence":-0.1}`),
		"prob outside eligible": route(`{"type":"choice","choice":"REPAIR","confidence":0.9,"probabilities":{"REPAIR":0.9,"STOP":0.1}}`),
		"prob > 1":              route(`{"type":"choice","choice":"REPAIR","confidence":0.9,"probabilities":{"REPAIR":1.2}}`),
		"prob not number":       route(`{"type":"choice","choice":"REPAIR","confidence":0.9,"probabilities":{"REPAIR":"high"}}`),
		"prob sum far from 1":   route(`{"type":"choice","choice":"REPAIR","confidence":0.9,"probabilities":{"REPAIR":0.5}}`),
		"probs not object":      route(`{"type":"choice","choice":"REPAIR","confidence":0.9,"probabilities":[0.9]}`),
		"usage bad":             `{"model":"m","answers":{"route":{"type":"choice","choice":"REPAIR","confidence":0.9}},"usage":{"input_tokens":-3}}`,
		"huge confidence":       route(`{"type":"choice","choice":"REPAIR","confidence":1e999}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := startJev(t, func(fakeapi.JevRequest) (int, any) { return 200, body })
			_, err := typesafeClient(f, &sleeper{}).Advise(context.Background(), question())
			if !errors.Is(err, jev.ErrMalformed) {
				t.Fatalf("want ErrMalformed, got %v", err)
			}
			if n := len(f.Requests()); n != 1 {
				t.Fatalf("malformed responses must not be retried; %d requests", n)
			}
		})
	}
}

func TestRetryOn429HonorsRetryAfter(t *testing.T) {
	f := startJev(t, func(r fakeapi.JevRequest) (int, any) {
		if r.Seq == 0 {
			return 429, fakeapi.JevResponse{Header: http.Header{"Retry-After": {"1"}}, Body: `{"error":"rate limited"}`}
		}
		return 200, liveBody
	})
	s := &sleeper{}
	adv, err := typesafeClient(f, s).Advise(context.Background(), question())
	if err != nil || adv.Choice != "REPAIR" {
		t.Fatalf("adv=%+v err=%v", adv, err)
	}
	if len(f.Requests()) != 2 || !reflect.DeepEqual(s.delays, []time.Duration{time.Second}) {
		t.Fatalf("requests=%d delays=%v", len(f.Requests()), s.delays)
	}
}

func TestRetryBackoffAndBudget(t *testing.T) {
	t.Run("5xx exhausts retries with exponential backoff", func(t *testing.T) {
		f := startJev(t, func(fakeapi.JevRequest) (int, any) { return 503, `{"error":"unavailable"}` })
		s := &sleeper{}
		_, err := typesafeClient(f, s, func(c *jev.Config) { c.RetryBaseDelay = 100 * time.Millisecond }).Advise(context.Background(), question())
		if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("err = %v", err)
		}
		if len(f.Requests()) != 3 || !reflect.DeepEqual(s.delays, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}) {
			t.Fatalf("requests=%d delays=%v", len(f.Requests()), s.delays)
		}
	})
	t.Run("retry-after beyond budget is not waited for", func(t *testing.T) {
		f := startJev(t, func(fakeapi.JevRequest) (int, any) {
			return 429, fakeapi.JevResponse{Header: http.Header{"Retry-After": {"60"}}, Body: `{}`}
		})
		s := &sleeper{}
		_, err := typesafeClient(f, s).Advise(context.Background(), question())
		if err == nil || !strings.Contains(err.Error(), "retry budget") || len(s.delays) != 0 || len(f.Requests()) != 1 {
			t.Fatalf("err=%v delays=%v requests=%d", err, s.delays, len(f.Requests()))
		}
	})
	t.Run("no retries configured", func(t *testing.T) {
		f := startJev(t, func(fakeapi.JevRequest) (int, any) { return 500, `{}` })
		_, err := typesafeClient(f, &sleeper{}, func(c *jev.Config) { c.MaxRetries = -1 }).Advise(context.Background(), question())
		if err == nil || len(f.Requests()) != 1 {
			t.Fatalf("err=%v requests=%d", err, len(f.Requests()))
		}
	})
	t.Run("400 is not retried", func(t *testing.T) {
		f := startJev(t, func(fakeapi.JevRequest) (int, any) { return 400, `{"error":"bad question"}` })
		_, err := typesafeClient(f, &sleeper{}).Advise(context.Background(), question())
		if err == nil || !strings.Contains(err.Error(), "bad question") || len(f.Requests()) != 1 {
			t.Fatalf("err=%v requests=%d", err, len(f.Requests()))
		}
	})
}

func TestAuthErrorsAreNotRetriedAndNeverLeakTheKey(t *testing.T) {
	for _, status := range []int{401, 403} {
		f := startJev(t, func(fakeapi.JevRequest) (int, any) {
			return status, `{"error":"invalid key ` + fakeKey + `"}`
		})
		s := &sleeper{}
		_, err := typesafeClient(f, s).Advise(context.Background(), question())
		if err == nil || len(f.Requests()) != 1 || len(s.delays) != 0 {
			t.Fatalf("%d: err=%v requests=%d", status, err, len(f.Requests()))
		}
		if strings.Contains(err.Error(), fakeKey) || !strings.Contains(err.Error(), "[REDACTED]") {
			t.Fatalf("%d: key must be redacted: %v", status, err)
		}
	}
}

func TestTimeout(t *testing.T) {
	f := startJev(t, func(fakeapi.JevRequest) (int, any) {
		return 200, fakeapi.JevResponse{Delay: 2 * time.Second, Body: liveBody}
	})
	s := &sleeper{}
	start := time.Now()
	_, err := typesafeClient(f, s, func(c *jev.Config) { c.Timeout = 50 * time.Millisecond }).Advise(context.Background(), question())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded (the ShadowRunner records ADVISOR_TIMEOUT), got %v", err)
	}
	if len(f.Requests()) != 3 || len(s.delays) != 2 || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("requests=%d delays=%v elapsed=%s", len(f.Requests()), s.delays, time.Since(start))
	}

	// The caller's own deadline stops everything without further retries.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = typesafeClient(f, s).Advise(ctx, question())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline: %v", err)
	}
}

func TestQuestionValidationWithoutNetwork(t *testing.T) {
	f := startJev(t, nil)
	c := typesafeClient(f, &sleeper{})
	for name, elig := range map[string][]string{
		"single option": {"REPAIR"},
		"no options":    nil,
		"duplicate":     {"REPAIR", "REPAIR"},
		"unknown":       {"REPAIR", "DEPLOY"},
	} {
		q := question()
		q.Eligible = elig
		if _, err := c.Advise(context.Background(), q); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("invalid questions made %d requests", n)
	}
}

func TestMissingKeyMakesNoNetworkCall(t *testing.T) {
	f := startJev(t, nil)
	for _, p := range []string{"typesafe", "openrouter"} {
		c := jev.New(jev.Config{Provider: p, BaseURL: f.URL()})
		if c.Describe().Configured {
			t.Fatalf("%s: configured without a key", p)
		}
		if _, err := c.Advise(context.Background(), question()); !errors.Is(err, jev.ErrNotConfigured) {
			t.Fatalf("%s: want ErrNotConfigured, got %v", p, err)
		}
	}
	if len(f.Requests()) != 0 {
		t.Fatal("network call without a key")
	}
	bad := jev.New(jev.Config{Provider: "acme", APIKey: fakeKey})
	if _, err := bad.Advise(context.Background(), question()); err == nil || bad.Describe().Configured {
		t.Fatal("unknown provider must be refused")
	}
}

func TestStateIsTruncated(t *testing.T) {
	q := question()
	q.View = []byte(`{"log":"` + strings.Repeat("é", 200000) + `"}`)
	s := jev.BuildState(q)
	if n := utf8.RuneCountInString(s); n > jev.MaxStateChars || n < jev.MaxStateChars-100 {
		t.Fatalf("state has %d characters", n)
	}
	if !strings.HasSuffix(s, "[truncated]") || !strings.Contains(s, "task_id: task:42") {
		t.Fatal("truncated state must keep the header and mark the cut")
	}
	f := startJev(t, func(fakeapi.JevRequest) (int, any) { return 200, liveBody })
	if _, err := typesafeClient(f, &sleeper{}).Advise(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if got := f.Requests()[0].State; got != s {
		t.Fatal("sent state differs from BuildState")
	}
}

func TestOpenRouterRequest(t *testing.T) {
	f := startJev(t, func(fakeapi.JevRequest) (int, any) {
		return 200, fakeapi.JevChoice("typesafe/jev-1.13", "GATHER_CONTEXT", 0.7,
			map[string]float64{"GATHER_CONTEXT": 0.7, "REPAIR": 0.2, "ESCALATE": 0.1})
	})
	c := jev.New(jev.Config{Provider: "openrouter", APIKey: "or-FAKE", BaseURL: f.URL() + "/api/alpha/decisions", Sleep: (&sleeper{}).Sleep})
	adv, err := c.Advise(context.Background(), question())
	if err != nil {
		t.Fatal(err)
	}
	if adv.Choice != "GATHER_CONTEXT" || adv.ModelRequested != "typesafe/jev-1.13" || adv.ModelReturned != "typesafe/jev-1.13" || adv.Mode != "LIVE" {
		t.Fatalf("advice %+v", adv)
	}
	r := f.Requests()[0]
	if r.Path != "/api/alpha/decisions" || r.Header.Get("Authorization") != "Bearer or-FAKE" ||
		r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get("HTTP-Referer") != "https://github.com/alaindgonz-cell/intellectus" || r.Header.Get("X-Title") != "INTELLECTUS" {
		t.Fatalf("path %s headers %v", r.Path, r.Header)
	}
	if r.Fields["model"] != "typesafe/jev-1.13" || !reflect.DeepEqual(r.Fields["provider"], map[string]any{"only": []any{"typesafe"}, "allow_fallbacks": false}) {
		t.Fatalf("body model %v provider %v", r.Fields["model"], r.Fields["provider"])
	}
	if len(r.Fields) != 4 || r.Questions["route"].Type != "choice" || len(r.Questions["route"].Criteria) != 3 || r.State == "" {
		t.Fatalf("body %v", r.Fields)
	}
}

func TestOpenRouterAuthErrorMentionsAlphaAccess(t *testing.T) {
	f := startJev(t, func(fakeapi.JevRequest) (int, any) {
		return 401, `{"error":{"message":"No auth credentials found","code":401}}`
	})
	c := jev.New(jev.Config{Provider: "openrouter", APIKey: "or-FAKE", BaseURL: f.URL()})
	_, err := c.Advise(context.Background(), question())
	if err == nil || !strings.Contains(err.Error(), "alpha access") || !strings.Contains(err.Error(), "HTTP 401") || len(f.Requests()) != 1 {
		t.Fatalf("err=%v requests=%d", err, len(f.Requests()))
	}
	if d := c.Describe(); !strings.Contains(d.Detail, "alpha access") {
		t.Fatalf("describe should warn about alpha access: %+v", d)
	}
}

func TestProviderSelectionFromEnvironment(t *testing.T) {
	type want struct{ provider, model, host string }
	cases := []struct {
		name string
		env  map[string]string
		cfg  jev.Config
		want want
	}{
		{"nothing set", nil, jev.Config{}, want{"typesafe", "jev-1.13.0", "api.typesafe.ai"}},
		{"typesafe key", map[string]string{"TYPESAFE_API_KEY": "a"}, jev.Config{}, want{"typesafe", "jev-1.13.0", "api.typesafe.ai"}},
		{"openrouter key", map[string]string{"OPENROUTER_API_KEY": "b"}, jev.Config{}, want{"openrouter", "typesafe/jev-1.13", "openrouter.ai"}},
		{"both keys prefer typesafe", map[string]string{"TYPESAFE_API_KEY": "a", "OPENROUTER_API_KEY": "b"}, jev.Config{}, want{"typesafe", "jev-1.13.0", "api.typesafe.ai"}},
		{"JEV_PROVIDER wins", map[string]string{"TYPESAFE_API_KEY": "a", "OPENROUTER_API_KEY": "b", "JEV_PROVIDER": "openrouter"}, jev.Config{}, want{"openrouter", "typesafe/jev-1.13", "openrouter.ai"}},
		{"config wins over env", map[string]string{"JEV_PROVIDER": "openrouter", "OPENROUTER_API_KEY": "b"}, jev.Config{Provider: "typesafe", APIKey: "x"}, want{"typesafe", "jev-1.13.0", "api.typesafe.ai"}},
		{"JEV_MODEL and JEV_BASE_URL (typesafe)", map[string]string{"TYPESAFE_API_KEY": "a", "JEV_MODEL": "jev-1.14.0", "JEV_BASE_URL": "https://jev.internal.example/v1/systemone"}, jev.Config{}, want{"typesafe", "jev-1.14.0", "jev.internal.example"}},
		{"JEV_BASE_URL is not applied to openrouter", map[string]string{"OPENROUTER_API_KEY": "b", "JEV_BASE_URL": "https://jev.internal.example/v1/systemone", "JEV_MODEL": "typesafe/jev-1.14"}, jev.Config{}, want{"openrouter", "typesafe/jev-1.14", "openrouter.ai"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			d := jev.New(tc.cfg).Describe()
			if d.Provider != tc.want.provider || d.Model != tc.want.model || d.Host != tc.want.host {
				t.Fatalf("got %+v, want %+v", d, tc.want)
			}
			configured := len(tc.env) > 0 || tc.cfg.APIKey != ""
			if d.Configured != configured {
				t.Fatalf("configured = %v, want %v", d.Configured, configured)
			}
			b, _ := json.Marshal(d)
			for _, secret := range []string{`"a"`, `"b"`, `"x"`} {
				if strings.Contains(string(b), secret) {
					t.Fatalf("describe leaks a key: %s", b)
				}
			}
		})
	}
}

// TestShadowRunnerAcceptsLiveAdvice runs the real ShadowRunner against a
// fake core and the fake Jev endpoint: the advice must pass the runner's
// own well-formedness checks and be recorded as a SHADOW assessment.
func TestShadowRunnerAcceptsLiveAdvice(t *testing.T) {
	f := startJev(t, func(fakeapi.JevRequest) (int, any) {
		return 200, fakeapi.JevChoice("jev-1.13.0", "REPAIR", 0.9, map[string]float64{"REPAIR": 0.333, "ESCALATE": 0.333, "STOP": 0.334})
	})
	core := fakecore.New()
	core.Handle("route", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"eligible": []string{"REPAIR", "ESCALATE", "STOP"}, "deterministic_choice": nil, "reason": "test"}, nil
	})
	core.Handle("view", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"snapshot_sequence": 5, "state_digest": "sha256:view", "policy_version": "P1"}, nil
	})
	core.Handle("record_assessment", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"assessment_id": "asm:1", "applied_choice": "REPAIR", "used_advisor": false}, nil
	})
	cc := core.Start()
	t.Cleanup(func() { cc.Close() })

	sr := &advisor.ShadowRunner{Core: cc, Session: coreclient.StringID("adv"), Advisor: typesafeClient(f, &sleeper{}), Mode: advisor.ModeShadow, Timeout: 5 * time.Second}
	d, err := sr.Decide(context.Background(), "task:x", []string{"acceptance_tests:FAIL"})
	if err != nil {
		t.Fatal(err)
	}
	if d.AdviceErr != nil || d.FallbackReason != advisor.FallbackShadow || d.Advice == nil || d.Advice.Mode != "LIVE" {
		t.Fatalf("decision %+v (advice err %v)", d, d.AdviceErr)
	}
	reqs := core.Requests("record_assessment")
	var a coreclient.AssessmentArgs
	if err := json.Unmarshal(reqs[len(reqs)-1].Args, &a); err != nil {
		t.Fatal(err)
	}
	if a.Mode != "SHADOW" || a.Choice == nil || *a.Choice != "REPAIR" || a.ModelRequested != "jev-1.13.0" ||
		!strings.HasPrefix(a.ProviderResponseRef, "jev:sha256:") || a.ConfidenceBP != 9000 {
		t.Fatalf("assessment %+v", a)
	}
}
