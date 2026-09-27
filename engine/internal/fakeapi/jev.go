package fakeapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"time"
)

// JevQuestion is one decoded entry of the request's "questions".
type JevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// JevRequest is a received System One request.
type JevRequest struct {
	Seq       int
	Method    string
	Path      string
	Header    http.Header
	Body      []byte
	Fields    map[string]any
	Model     string
	State     string
	Questions map[string]JevQuestion
	// DecodeErr is set when the body was not the expected JSON shape.
	DecodeErr error
}

// JevResponse lets a Jev script set headers or a delay. Return it as the
// body value; Body inside it is then written like a plain body.
type JevResponse struct {
	Header http.Header
	Body   any
	Delay  time.Duration
}

// Jev is a fake of the TypeSafe Jev System One endpoint (any path, POST).
type Jev struct {
	script func(JevRequest) (int, any)

	mu   sync.Mutex
	reqs []JevRequest
	srv  *httptest.Server
}

func sortedCriteria(c map[string]string) []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NewJevHandler returns an unstarted fake, usable as an http.Handler.
//
// The script returns an HTTP status and a body: []byte, json.RawMessage or
// string bodies are written verbatim (for malformed-response tests), a
// JevResponse adds headers/delay, anything else is JSON-encoded. A nil
// script answers with JevChoice for the first criteria option.
func NewJevHandler(script func(JevRequest) (status int, body any)) *Jev {
	if script == nil {
		script = func(r JevRequest) (int, any) {
			for _, q := range r.Questions {
				opts := sortedCriteria(q.Criteria)
				if len(opts) > 0 {
					return http.StatusOK, JevChoice(r.Model, opts[0], 0.9, nil)
				}
			}
			return http.StatusBadRequest, map[string]any{"error": "no questions"}
		}
	}
	return &Jev{script: script}
}

// NewJev starts the fake on a loopback httptest server.
func NewJev(script func(JevRequest) (status int, body any)) *Jev {
	j := NewJevHandler(script)
	j.srv = httptest.NewServer(j)
	return j
}

// URL is the base URL of the started server ("" if not started). The fake
// answers on every path, so URL() + "/v1/systemone" works as a base URL.
func (j *Jev) URL() string {
	if j.srv == nil {
		return ""
	}
	return j.srv.URL
}

// Close stops the started server.
func (j *Jev) Close() {
	if j.srv != nil {
		j.srv.Close()
	}
}

// Requests returns a copy of every request received so far.
func (j *Jev) Requests() []JevRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]JevRequest(nil), j.reqs...)
}

// ServeHTTP implements http.Handler.
func (j *Jev) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	req := JevRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
	if err := json.Unmarshal(body, &req.Fields); err != nil {
		req.DecodeErr = err
	} else {
		var typed struct {
			Model     string                 `json:"model"`
			State     string                 `json:"state"`
			Questions map[string]JevQuestion `json:"questions"`
		}
		if err := json.Unmarshal(body, &typed); err != nil {
			req.DecodeErr = err
		}
		req.Model, req.State, req.Questions = typed.Model, typed.State, typed.Questions
	}
	j.mu.Lock()
	req.Seq = len(j.reqs)
	j.reqs = append(j.reqs, req)
	j.mu.Unlock()

	status, out := j.script(req)
	var header http.Header
	if jr, ok := out.(JevResponse); ok {
		header, out = jr.Header, jr.Body
		if jr.Delay > 0 {
			t := time.NewTimer(jr.Delay)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
	}
	for k, vs := range header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	var raw []byte
	switch b := out.(type) {
	case nil:
	case []byte:
		raw = b
	case json.RawMessage:
		raw = b
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(b)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// JevChoice builds a well-formed System One response answering the "route"
// question with choice. probs may be nil (then choice gets confidence and
// nothing else is listed).
func JevChoice(model, choice string, confidence float64, probs map[string]float64) map[string]any {
	if probs == nil {
		probs = map[string]float64{choice: confidence}
	}
	return map[string]any{
		"model": model,
		"answers": map[string]any{"route": map[string]any{
			"type": "choice", "choice": choice, "confidence": confidence, "probabilities": probs,
		}},
		"usage": map[string]any{"input_tokens": 518, "output_tokens": 69},
		"cost":  "0",
	}
}
