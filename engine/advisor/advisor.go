// Package advisor implements the optional Jev DecisionAdvisor seam.
//
// Deterministic rules come first: the core's `route` is always called, and
// when it returns a deterministic_choice the advisor is not consulted. When
// it does not, the advisor may be consulted and its assessment is recorded
// with `record_assessment`; the route that is APPLIED is always the core's
// applied_choice. In SHADOW mode (and for any non-LIVE advisor, e.g.
// SIMULATED) that is the core's deterministic fallback.
//
// The advisor can only name one of the core's eligible options. It has no
// way to raise budgets, mark tasks complete or execute anything.
package advisor

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"
)

// Advisor modes as recorded.
const (
	ModeLive      = "LIVE"
	ModeShadow    = "SHADOW"
	ModeSimulated = "SIMULATED"
	ModeOff       = "OFF"
)

// Question is what the advisor is asked.
type Question struct {
	TaskID       string
	Eligible     []string
	FailureCodes []string
	// View is the advisor-clearance (permission-filtered) view, raw.
	View []byte
}

// Advice is an advisor's answer. All probabilities are basis points.
type Advice struct {
	Choice          string
	ProbabilitiesBP map[string]int64
	ConfidenceBP    int64
	ModelRequested  string
	ModelReturned   string
	// Mode is the advisor's own mode (SIMULATED for SimulatedAdvisor, LIVE
	// only for a real model provider — none exists in this build).
	Mode        string
	Usage       map[string]int64
	ProviderRef string
}

// DecisionAdvisor is the Jev seam.
type DecisionAdvisor interface {
	Advise(ctx context.Context, q Question) (Advice, error)
}

// SimulatedAdvisor returns scripted advice (mode SIMULATED). Script entries
// are consumed in order; the last one repeats.
type SimulatedAdvisor struct {
	Script []Advice
	// Delay simulates latency (respecting ctx).
	Delay time.Duration
	// Err, if set, is returned instead of advice.
	Err error

	mu    sync.Mutex
	calls int
}

// ErrNoScript is returned when a SimulatedAdvisor has no scripted advice.
var ErrNoScript = errors.New("simulated advisor: no scripted advice")

// Calls returns how often Advise was called.
func (s *SimulatedAdvisor) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Advise implements DecisionAdvisor.
func (s *SimulatedAdvisor) Advise(ctx context.Context, q Question) (Advice, error) {
	s.mu.Lock()
	idx := s.calls
	s.calls++
	s.mu.Unlock()
	if s.Delay > 0 {
		t := time.NewTimer(s.Delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return Advice{}, ctx.Err()
		}
	}
	if s.Err != nil {
		return Advice{}, s.Err
	}
	if len(s.Script) == 0 {
		return Advice{}, ErrNoScript
	}
	if idx >= len(s.Script) {
		idx = len(s.Script) - 1
	}
	a := s.Script[idx]
	if a.Mode == "" {
		a.Mode = ModeSimulated
	}
	if a.ModelRequested == "" {
		a.ModelRequested = "simulated-jev/v1"
	}
	if a.ModelReturned == "" {
		a.ModelReturned = a.ModelRequested
	}
	if a.ProviderRef == "" {
		a.ProviderRef = "simulated:advisor#" + strconv.Itoa(idx)
	}
	return a, nil
}
