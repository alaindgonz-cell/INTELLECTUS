// Package jev is a live advisor.DecisionAdvisor backed by TypeSafe's Jev
// "System One" decision model, reached either directly (TypeSafe) or through
// OpenRouter's native decisions endpoint.
//
// Wire format (from the MIT-licensed community harness
// github.com/ismaelsoilet/jev-harness @37ab8c6 and a response it recorded;
// TypeSafe's own documentation was not reachable when this was written):
//
//	POST <base URL>
//	Content-Type: application/json
//	Authorization: Bearer <key>
//	{"model": "jev-1.13.0", "state": "<text>",
//	 "questions": {"route": {"type": "choice", "instructions": "...",
//	                         "criteria": {"REPAIR": "<description>", ...}}}}
//
//	200 {"model": "jev-1.13.0",
//	     "answers": {"route": {"type": "choice", "choice": "REPAIR",
//	                           "confidence": 0.98, "probabilities": {"REPAIR": 0.96, ...}}},
//	     "usage": {"input_tokens": 518, "output_tokens": 69}, "cost": "0"}
//
// OpenRouter takes the same body plus "provider": {"only": ["typesafe"],
// "allow_fallbacks": false} at /api/alpha/decisions (alpha access only).
//
// The advisor is untrusted: responses are parsed strictly and anything
// unexpected is an error, which the ShadowRunner records as a fallback. The
// API key is never logged or put in an error.
package jev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// Providers.
const (
	ProviderTypeSafe   = "typesafe"
	ProviderOpenRouter = "openrouter"
)

// Defaults.
const (
	DefaultTypeSafeURL     = "https://api.typesafe.ai/v1/systemone"
	DefaultOpenRouterURL   = "https://openrouter.ai/api/alpha/decisions"
	DefaultTypeSafeModel   = "jev-1.13.0" // pinned: "jev-latest" is a moving alias
	DefaultOpenRouterModel = "typesafe/jev-1.13"
	DefaultTimeout         = 8 * time.Second
	DefaultMaxRetries      = 2
	DefaultRetryBaseDelay  = 250 * time.Millisecond
	DefaultMaxRetryWait    = 3 * time.Second

	// MaxStateChars caps the state string (the provider allows ~64k tokens
	// in total; the harness caps state at 128000 characters).
	MaxStateChars = 120000
	// maxTotalChars caps state + questions (harness limit).
	maxTotalChars = 256000
	// maxResponseBytes bounds how much of a response is read.
	maxResponseBytes = 1 << 20

	// QuestionID is the id of the single question asked.
	QuestionID = "route"
	// Instructions is the fixed question text.
	Instructions = "You are routing an automated software-repair loop after a failed attempt. Choose the single most useful next step."

	userAgent  = "Mozilla/5.0 (compatible; IntellectusEngine/0.1; +https://github.com/alaindgonz-cell/intellectus)"
	orReferer  = "https://github.com/alaindgonz-cell/intellectus"
	orTitle    = "INTELLECTUS"
	sumEpsilon = 0.02
)

// Criteria describes each route option to the model.
var Criteria = map[string]string{
	coreclient.RouteGatherContext: "Collect more information (failure analysis, relevant files) before changing code again",
	coreclient.RouteRepair:        "Fix the current candidate directly using the reported test failures",
	coreclient.RouteReplan:        "The approach looks wrong; revise the plan before writing new code",
	coreclient.RouteEscalate:      "Stop and ask the human operator for guidance",
	coreclient.RouteStop:          "Stop working on this task",
}

// ErrNotConfigured: no API key is available for the selected provider.
var ErrNotConfigured = errors.New("jev: not configured (no API key)")

// ErrMalformed is wrapped by every error about an unusable response.
var ErrMalformed = errors.New("jev: malformed response")

// Config configures a Client. Empty fields take defaults from the
// environment, then from the constants above.
type Config struct {
	// Provider is "typesafe" or "openrouter". Empty: env JEV_PROVIDER;
	// else "typesafe" when APIKey is set or TYPESAFE_API_KEY is; else
	// "openrouter" when OPENROUTER_API_KEY is; else "typesafe".
	Provider string
	// APIKey defaults to TYPESAFE_API_KEY or OPENROUTER_API_KEY (by provider).
	APIKey string
	// BaseURL is the full endpoint URL. TypeSafe: env JEV_BASE_URL, else
	// DefaultTypeSafeURL. OpenRouter: DefaultOpenRouterURL (JEV_BASE_URL is
	// deliberately not applied, so an OpenRouter key is never sent to a
	// URL configured for TypeSafe).
	BaseURL string
	// Model defaults to env JEV_MODEL, else the provider default.
	Model string
	// Timeout bounds each HTTP attempt (default 8s).
	Timeout time.Duration
	// MaxRetries for 429/5xx/transport errors: 0 = default (2), negative = none.
	MaxRetries int
	// HTTPClient replaces the default client (which refuses redirects).
	HTTPClient *http.Client
	// RetryBaseDelay is the first backoff delay (doubling after); default 250ms.
	RetryBaseDelay time.Duration
	// MaxRetryWait caps the total time spent waiting between attempts
	// (default 3s). A Retry-After beyond the remaining budget ends retrying.
	MaxRetryWait time.Duration
	// Sleep waits between attempts (tests inject a recorder); nil = a timer
	// that honors ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Description is non-secret configuration for status displays.
type Description struct {
	Provider   string `json:"provider"`
	Configured bool   `json:"configured"`
	Model      string `json:"model"`
	Host       string `json:"host"`
	Detail     string `json:"detail,omitempty"`
}

// Client implements advisor.DecisionAdvisor. Safe for concurrent use.
type Client struct {
	provider   string
	key        string
	keySource  string
	endpoint   string
	host       string
	model      string
	timeout    time.Duration
	retries    int
	base       time.Duration
	maxWait    time.Duration
	sleep      func(context.Context, time.Duration) error
	hc         *http.Client
	cfgErr     error
	configured bool
}

var _ advisor.DecisionAdvisor = (*Client)(nil)

// New builds a Client. It performs no network I/O.
func New(cfg Config) *Client {
	c := &Client{timeout: cfg.Timeout, retries: cfg.MaxRetries, base: cfg.RetryBaseDelay, maxWait: cfg.MaxRetryWait, sleep: cfg.Sleep, hc: cfg.HTTPClient}
	c.provider = selectProvider(cfg)
	switch c.provider {
	case ProviderTypeSafe:
		c.key, c.keySource = cfg.APIKey, "config"
		if c.key == "" {
			c.key, c.keySource = os.Getenv("TYPESAFE_API_KEY"), "env TYPESAFE_API_KEY"
		}
		c.endpoint = firstNonEmpty(cfg.BaseURL, os.Getenv("JEV_BASE_URL"), DefaultTypeSafeURL)
		c.model = firstNonEmpty(cfg.Model, strings.TrimSpace(os.Getenv("JEV_MODEL")), DefaultTypeSafeModel)
	case ProviderOpenRouter:
		c.key, c.keySource = cfg.APIKey, "config"
		if c.key == "" {
			c.key, c.keySource = os.Getenv("OPENROUTER_API_KEY"), "env OPENROUTER_API_KEY"
		}
		c.endpoint = firstNonEmpty(cfg.BaseURL, DefaultOpenRouterURL)
		c.model = firstNonEmpty(cfg.Model, strings.TrimSpace(os.Getenv("JEV_MODEL")), DefaultOpenRouterModel)
	default:
		c.cfgErr = fmt.Errorf("jev: unknown provider %q (want %q or %q)", c.provider, ProviderTypeSafe, ProviderOpenRouter)
	}
	if u, err := url.Parse(c.endpoint); c.cfgErr == nil && (err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http")) {
		c.cfgErr = fmt.Errorf("jev: invalid base URL for provider %s", c.provider)
	} else if err == nil {
		c.host = u.Host
	}
	c.configured = c.cfgErr == nil && c.key != ""
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	switch {
	case c.retries == 0:
		c.retries = DefaultMaxRetries
	case c.retries < 0:
		c.retries = 0
	}
	if c.base <= 0 {
		c.base = DefaultRetryBaseDelay
	}
	if c.maxWait <= 0 {
		c.maxWait = DefaultMaxRetryWait
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.hc == nil {
		c.hc = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return c
}

func selectProvider(cfg Config) string {
	if p := strings.ToLower(strings.TrimSpace(cfg.Provider)); p != "" {
		return p
	}
	if p := strings.ToLower(strings.TrimSpace(os.Getenv("JEV_PROVIDER"))); p != "" {
		return p
	}
	switch {
	case cfg.APIKey != "", os.Getenv("TYPESAFE_API_KEY") != "":
		return ProviderTypeSafe
	case os.Getenv("OPENROUTER_API_KEY") != "":
		return ProviderOpenRouter
	}
	return ProviderTypeSafe
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Describe reports the provider, model and host; never the key.
func (c *Client) Describe() Description {
	d := Description{Provider: c.provider, Configured: c.configured, Model: c.model, Host: c.host}
	switch {
	case c.cfgErr != nil:
		d.Detail = c.cfgErr.Error()
	case !c.configured:
		d.Detail = "no API key (set TYPESAFE_API_KEY or OPENROUTER_API_KEY)"
	default:
		d.Detail = "credentials: " + c.keySource
	}
	if c.provider == ProviderOpenRouter {
		d.Detail += "; OpenRouter's Jev decisions endpoint requires approved alpha access"
	}
	return d
}

// wire types (field order fixed for a stable request body).
type wireQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type wireProvider struct {
	Only           []string `json:"only"`
	AllowFallbacks bool     `json:"allow_fallbacks"`
}

type wireRequest struct {
	Model     string                  `json:"model"`
	State     string                  `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
	Provider  *wireProvider           `json:"provider,omitempty"`
}

// Advise implements advisor.DecisionAdvisor.
func (c *Client) Advise(ctx context.Context, q advisor.Question) (advisor.Advice, error) {
	if c.cfgErr != nil {
		return advisor.Advice{}, c.cfgErr
	}
	if c.key == "" {
		return advisor.Advice{}, ErrNotConfigured
	}
	criteria, err := criteriaFor(q.Eligible)
	if err != nil {
		return advisor.Advice{}, err
	}
	req := wireRequest{
		Model: c.model,
		State: BuildState(q),
		Questions: map[string]wireQuestion{QuestionID: {
			Type: "choice", Instructions: Instructions, Criteria: criteria,
		}},
	}
	if c.provider == ProviderOpenRouter {
		req.Provider = &wireProvider{Only: []string{"typesafe"}, AllowFallbacks: false}
	}
	body, err := marshalNoEscape(req)
	if err != nil {
		return advisor.Advice{}, fmt.Errorf("jev: encode request: %w", err)
	}
	qjson, _ := marshalNoEscape(req.Questions)
	if n := utf8.RuneCountInString(req.State) + utf8.RuneCount(qjson); n > maxTotalChars {
		return advisor.Advice{}, fmt.Errorf("jev: payload of %d characters exceeds the %d limit", n, maxTotalChars)
	}
	raw, err := c.send(ctx, body)
	if err != nil {
		return advisor.Advice{}, err
	}
	adv, err := parseResponse(raw, q.Eligible)
	if err != nil {
		return advisor.Advice{}, err
	}
	adv.ModelRequested = c.model
	return adv, nil
}

func criteriaFor(eligible []string) (map[string]string, error) {
	if len(eligible) < 2 {
		return nil, fmt.Errorf("jev: a choice question needs at least 2 eligible options, got %d", len(eligible))
	}
	out := make(map[string]string, len(eligible))
	for _, opt := range eligible {
		desc, ok := Criteria[opt]
		if !ok {
			return nil, fmt.Errorf("jev: no description for route option %q", opt)
		}
		if _, dup := out[opt]; dup {
			return nil, fmt.Errorf("jev: duplicate eligible option %q", opt)
		}
		out[opt] = desc
	}
	return out, nil
}

// BuildState renders the question as the "state" text, at most
// MaxStateChars characters. The view (already permission-filtered by the
// core) is compacted when it is valid JSON and truncated when too long.
func BuildState(q advisor.Question) string {
	codes := "(none)"
	if len(q.FailureCodes) > 0 {
		codes = strings.Join(q.FailureCodes, ", ")
	}
	var head strings.Builder
	head.WriteString("INTELLECTUS automated software-repair loop: routing decision after a failed attempt.\n")
	fmt.Fprintf(&head, "task_id: %s\n", q.TaskID)
	fmt.Fprintf(&head, "failure_codes: %s\n", codes)
	fmt.Fprintf(&head, "eligible_options: %s\n", strings.Join(q.Eligible, ", "))
	head.WriteString("task view (permission-filtered JSON):\n")

	view := "(unavailable)"
	if len(bytes.TrimSpace(q.View)) > 0 {
		var buf bytes.Buffer
		if json.Compact(&buf, q.View) == nil {
			view = buf.String()
		} else {
			view = strings.ToValidUTF8(string(q.View), "�")
		}
	}
	h := head.String()
	room := MaxStateChars - utf8.RuneCountInString(h)
	const marker = "\n[truncated]"
	if utf8.RuneCountInString(view) > room {
		keep := room - utf8.RuneCountInString(marker)
		if keep < 0 {
			keep = 0
		}
		r := []rune(view)
		view = string(r[:keep]) + marker
	}
	s := h + view
	if utf8.RuneCountInString(s) > MaxStateChars {
		s = string([]rune(s)[:MaxStateChars])
	}
	return s
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// httpError is a non-2xx answer.
type httpError struct {
	status  int
	snippet string
	extra   string
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("jev: provider returned HTTP %d", e.status)
	if e.extra != "" {
		msg += " (" + e.extra + ")"
	}
	if e.snippet != "" {
		msg += ": " + e.snippet
	}
	return msg
}

// send POSTs body with bounded retries and returns the 200 response body.
func (c *Client) send(ctx context.Context, body []byte) ([]byte, error) {
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		raw, status, header, err := c.attempt(ctx, body)
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("jev: %w", cerr)
		}
		if err == nil && status == http.StatusOK {
			return raw, nil
		}
		retryable := (err != nil && !errors.Is(err, ErrMalformed)) || status == http.StatusTooManyRequests || status >= 500
		if err == nil {
			herr := &httpError{status: status, snippet: c.snippet(raw)}
			switch {
			case (status == http.StatusUnauthorized || status == http.StatusForbidden) && c.provider == ProviderOpenRouter:
				herr.extra = "authentication/authorization failed; OpenRouter's Jev decisions endpoint requires approved alpha access"
			case status == http.StatusUnauthorized || status == http.StatusForbidden:
				herr.extra = "authentication/authorization failed; check TYPESAFE_API_KEY"
			case status >= 300 && status < 400:
				herr.extra = "redirects are not followed"
			}
			err = herr
		}
		if !retryable || attempt >= c.retries {
			return nil, err
		}
		delay := c.base << attempt
		if ra, ok := retryAfter(header); ok {
			delay = ra
		}
		if waited+delay > c.maxWait {
			return nil, fmt.Errorf("%w (not retried: next wait %s exceeds the %s retry budget)", err, delay, c.maxWait)
		}
		if dl, ok := ctx.Deadline(); ok && time.Now().Add(delay).After(dl) {
			return nil, fmt.Errorf("%w (not retried: the deadline is sooner than the next wait %s)", err, delay)
		}
		if serr := c.sleep(ctx, delay); serr != nil {
			return nil, fmt.Errorf("jev: %w", serr)
		}
		waited += delay
	}
}

// attempt performs one POST. A per-attempt timeout yields an error that
// wraps context.DeadlineExceeded.
func (c *Client) attempt(ctx context.Context, body []byte) ([]byte, int, http.Header, error) {
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("jev: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", userAgent)
	if c.provider == ProviderOpenRouter {
		req.Header.Set("HTTP-Referer", orReferer)
		req.Header.Set("X-Title", orTitle)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, 0, nil, fmt.Errorf("jev: attempt timed out after %s: %w", c.timeout, context.DeadlineExceeded)
		}
		return nil, 0, nil, fmt.Errorf("jev: transport error: %s", c.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, 0, nil, fmt.Errorf("jev: attempt timed out after %s: %w", c.timeout, context.DeadlineExceeded)
		}
		return nil, 0, nil, fmt.Errorf("jev: reading response: %s", c.redact(err.Error()))
	}
	if len(raw) > maxResponseBytes {
		return nil, 0, nil, fmt.Errorf("%w: response larger than %d bytes", ErrMalformed, maxResponseBytes)
	}
	return raw, resp.StatusCode, resp.Header, nil
}

func (c *Client) redact(s string) string {
	if c.key != "" {
		s = strings.ReplaceAll(s, c.key, "[REDACTED]")
	}
	return s
}

func (c *Client) snippet(raw []byte) string {
	s := strings.ToValidUTF8(strings.TrimSpace(string(raw)), "?")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return c.redact(s)
}

// retryAfter parses Retry-After (delta-seconds or HTTP-date).
func retryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 && !math.IsInf(secs, 0) {
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// parseResponse validates a 200 body strictly and maps it to Advice.
func parseResponse(raw []byte, eligible []string) (advisor.Advice, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return advisor.Advice{}, malformed("body is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return advisor.Advice{}, malformed("trailing data after the JSON object")
	}
	model, ok := top["model"].(string)
	if !ok || model == "" {
		return advisor.Advice{}, malformed("missing model")
	}
	answers, ok := top["answers"].(map[string]any)
	if !ok {
		return advisor.Advice{}, malformed("answers is not an object")
	}
	route, ok := answers[QuestionID].(map[string]any)
	if !ok {
		return advisor.Advice{}, malformed("answers.%s is missing or not an object", QuestionID)
	}
	if route["type"] != "choice" {
		return advisor.Advice{}, malformed("answers.%s.type is %v, want \"choice\"", QuestionID, route["type"])
	}
	choice, ok := route["choice"].(string)
	if !ok {
		return advisor.Advice{}, malformed("answers.%s.choice is not a string", QuestionID)
	}
	if !contains(eligible, choice) {
		return advisor.Advice{}, malformed("choice %q is not one of the eligible options %v", choice, eligible)
	}
	conf, err := unitNumber(route["confidence"], "confidence")
	if err != nil {
		return advisor.Advice{}, err
	}
	probsBP, err := probabilities(route["probabilities"], eligible)
	if err != nil {
		return advisor.Advice{}, err
	}
	usage, err := usageOf(top["usage"])
	if err != nil {
		return advisor.Advice{}, err
	}
	sum := sha256.Sum256(raw)
	return advisor.Advice{
		Choice:          choice,
		ProbabilitiesBP: probsBP,
		ConfidenceBP:    int64(math.Round(conf * 10000)),
		ModelReturned:   model,
		Mode:            advisor.ModeLive,
		Usage:           usage,
		ProviderRef:     "jev:sha256:" + hex.EncodeToString(sum[:]),
		CostUSD:         costOf(top["cost"]),
		Raw:             json.RawMessage(append([]byte(nil), raw...)),
	}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func unitNumber(v any, field string) (float64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, malformed("%s is not a number", field)
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
		return 0, malformed("%s %s is not a finite number in [0,1]", field, n)
	}
	return f, nil
}

// probabilities maps the distribution to basis points summing to exactly
// 10000 (largest-remainder rounding; ties broken by option name), as the
// core requires. The raw values must sum to 1 within sumEpsilon. Absent or
// null probabilities yield nil.
func probabilities(v any, eligible []string) (map[string]int64, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, malformed("probabilities is not an object")
	}
	if len(m) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	vals := map[string]float64{}
	total := 0.0
	for k, x := range m {
		if !contains(eligible, k) {
			return nil, malformed("probability for %q, which is not an eligible option", k)
		}
		f, err := unitNumber(x, "probability for "+k)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
		vals[k] = f
		total += f
	}
	if math.Abs(total-1) > sumEpsilon {
		return nil, malformed("probabilities sum to %.4f, want 1", total)
	}
	sort.Strings(keys)
	out := make(map[string]int64, len(keys))
	type rem struct {
		k string
		r float64
	}
	var rems []rem
	var assigned int64
	for _, k := range keys {
		exact := vals[k] / total * 10000
		fl := math.Floor(exact)
		out[k] = int64(fl)
		assigned += int64(fl)
		rems = append(rems, rem{k, exact - fl})
	}
	sort.SliceStable(rems, func(i, j int) bool { return rems[i].r > rems[j].r })
	for i := 0; assigned < 10000; i++ {
		out[rems[i%len(rems)].k]++
		assigned++
	}
	return out, nil
}

func usageOf(v any) (map[string]int64, error) {
	out := map[string]int64{}
	if v == nil {
		return out, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, malformed("usage is not an object")
	}
	for _, k := range []string{"input_tokens", "output_tokens"} {
		x, present := m[k]
		if !present || x == nil {
			continue
		}
		n, ok := x.(json.Number)
		if !ok {
			return nil, malformed("usage.%s is not a number", k)
		}
		i, err := n.Int64()
		if err != nil || i < 0 {
			return nil, malformed("usage.%s %s is not a non-negative integer", k, n)
		}
		out[k] = i
	}
	return out, nil
}

// costOf accepts a number or a numeric string; anything else is "".
func costOf(v any) string {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	default:
		return ""
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
