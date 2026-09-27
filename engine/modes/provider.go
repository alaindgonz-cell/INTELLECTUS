// Package modes adapts model providers to the core's `submit` command for
// the Planner, Coder and Tester roles.
//
// Model output is UNTRUSTED DATA. Adapters pass the provider's raw text to
// `submit` byte-for-byte; the engine never parses model text to make any
// authorization or routing decision. Only the core interprets it.
//
// There is deliberately no live network provider: providers sit behind the
// Provider interface and only RecordedProvider (scripted responses) exists.
package modes

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Request is what the engine asks a model.
type Request struct {
	Role   string
	TaskID string
	// Phase is an engine hint: "plan", "initial", "repair", "action", ...
	Phase string
	// Call is the per-role call index, assigned by the provider.
	Call   int
	Prompt string
	// Vars carries core-issued values the prompt refers to (candidate_root,
	// idempotency_key, requirement refs, ...). Recorded providers use them
	// to template scripted answers.
	Vars map[string]string
}

// Usage is token accounting reported by a provider.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Response is a model's raw answer.
type Response struct {
	Text           string
	ModelRequested string
	ModelReturned  string
	Usage          Usage
	// Ref identifies the stored provider response (for provenance).
	Ref string
}

// Provider is a model backend.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
}

// ErrNoRecording is returned when a RecordedProvider has no script entry.
var ErrNoRecording = errors.New("recorded provider: no recorded response")

// RecordedFunc produces a scripted response text for a request.
type RecordedFunc func(req Request) (string, error)

// RecordedProvider returns fixed, scripted responses keyed by role and
// per-role call index, falling back to Func. It never touches a network.
// Its output is SIMULATED model output.
type RecordedProvider struct {
	Model string
	Func  RecordedFunc

	mu        sync.Mutex
	responses map[string]string
	calls     map[string]int
	log       []Request
}

// NewRecordedProvider returns an empty recorded provider.
func NewRecordedProvider() *RecordedProvider {
	return &RecordedProvider{Model: "recorded/v1", responses: map[string]string{}, calls: map[string]int{}}
}

func recKey(role string, idx int) string { return fmt.Sprintf("%s#%d", role, idx) }

// Add scripts the response for the idx-th call (0-based) of role.
func (p *RecordedProvider) Add(role string, idx int, text string) *RecordedProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responses[recKey(role, idx)] = text
	return p
}

// Name implements Provider.
func (p *RecordedProvider) Name() string { return "recorded" }

// Calls returns the requests seen so far.
func (p *RecordedProvider) Calls() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.log...)
}

// Complete implements Provider.
func (p *RecordedProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	p.mu.Lock()
	idx := p.calls[req.Role]
	p.calls[req.Role] = idx + 1
	req.Call = idx
	p.log = append(p.log, req)
	text, ok := p.responses[recKey(req.Role, idx)]
	fn := p.Func
	p.mu.Unlock()
	if !ok {
		if fn == nil {
			return Response{}, fmt.Errorf("%w for %s", ErrNoRecording, recKey(req.Role, idx))
		}
		var err error
		if text, err = fn(req); err != nil {
			return Response{}, err
		}
	}
	return Response{
		Text:           text,
		ModelRequested: p.Model,
		ModelReturned:  p.Model,
		Ref:            "recorded:" + recKey(req.Role, idx),
	}, nil
}
