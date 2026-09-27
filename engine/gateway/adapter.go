// Package gateway is the tool gateway: it executes AUTHORIZED actions that
// the core has launched (dispatch_begin -> STARTED) against external
// targets through Adapters, and reconciles OUTCOME_UNKNOWN actions.
//
// Rules:
//   - An adapter error, panic or context cancellation during Execute is
//     recorded as record_outcome_unknown. Failure is never assumed.
//   - The reconciler only calls Adapter.Reconcile (a read). It never
//     re-executes an action.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// Observation is what an adapter observed after executing an effect.
type Observation struct {
	// Outcome is SUCCEEDED or FAILED (the target positively reported it).
	Outcome string
	Data    map[string]any
}

// Finding is a reconcile read result (effect_observed true/false/nil).
type Finding = coreclient.Finding

// Adapter executes and reconciles one family of external tools.
type Adapter interface {
	Execute(ctx context.Context, toolID string, args json.RawMessage, idempotencyKey string) (Observation, error)
	Reconcile(ctx context.Context, toolID string, args json.RawMessage, idempotencyKey string) (Finding, error)
}

// FailMode configures a FakeTargetAdapter failure.
type FailMode int

const (
	// FailNone: apply the effect and report success.
	FailNone FailMode = iota
	// FailCrashAfterEffect: apply the effect, then return a transport error
	// (the caller cannot know whether the effect happened).
	FailCrashAfterEffect
	// FailPanicAfterEffect: apply the effect, then panic.
	FailPanicAfterEffect
	// FailErrorBeforeEffect: return an error without applying the effect.
	FailErrorBeforeEffect
	// FailHangAfterEffect: apply the effect, then block until ctx is done.
	FailHangAfterEffect
)

// Effect is one effect recorded by the fake target.
type Effect struct {
	ToolID         string
	Arguments      json.RawMessage
	IdempotencyKey string
	Seq            int
}

// FakeTargetAdapter is an in-memory external target (SIMULATED). Effects
// are keyed by idempotency key: re-applying the same key is a no-op, like
// a well-behaved idempotent API.
type FakeTargetAdapter struct {
	mu      sync.Mutex
	effects map[string]Effect
	fail    FailMode
	seq     int

	executes   atomic.Int64
	reconciles atomic.Int64
}

// NewFakeTargetAdapter returns an empty fake target.
func NewFakeTargetAdapter() *FakeTargetAdapter {
	return &FakeTargetAdapter{effects: map[string]Effect{}}
}

// SetFailMode configures the next and subsequent Execute calls.
func (a *FakeTargetAdapter) SetFailMode(m FailMode) {
	a.mu.Lock()
	a.fail = m
	a.mu.Unlock()
}

// ExecuteCount returns how many times Execute was called.
func (a *FakeTargetAdapter) ExecuteCount() int64 { return a.executes.Load() }

// ReconcileCount returns how many times Reconcile was called.
func (a *FakeTargetAdapter) ReconcileCount() int64 { return a.reconciles.Load() }

// Effects returns a copy of the applied effects.
func (a *FakeTargetAdapter) Effects() map[string]Effect {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]Effect, len(a.effects))
	for k, v := range a.effects {
		out[k] = v
	}
	return out
}

// ErrSimulatedCrash is the transport failure injected after an effect.
var ErrSimulatedCrash = errors.New("fake target: connection reset after request was written (SIMULATED crash)")

// Execute implements Adapter.
func (a *FakeTargetAdapter) Execute(ctx context.Context, toolID string, args json.RawMessage, key string) (Observation, error) {
	a.executes.Add(1)
	if key == "" {
		return Observation{}, errors.New("fake target: idempotency key required")
	}
	a.mu.Lock()
	mode := a.fail
	if mode == FailErrorBeforeEffect {
		a.mu.Unlock()
		return Observation{}, errors.New("fake target: refused before applying (SIMULATED)")
	}
	eff, existed := a.effects[key]
	if !existed {
		a.seq++
		eff = Effect{ToolID: toolID, Arguments: append(json.RawMessage(nil), args...), IdempotencyKey: key, Seq: a.seq}
		a.effects[key] = eff
	}
	a.mu.Unlock()

	switch mode {
	case FailCrashAfterEffect:
		return Observation{}, ErrSimulatedCrash
	case FailPanicAfterEffect:
		panic("fake target: panic after applying effect (SIMULATED)")
	case FailHangAfterEffect:
		<-ctx.Done()
		return Observation{}, ctx.Err()
	}
	data := argFields(eff.Arguments)
	data["target"] = "fake"
	data["effect_seq"] = eff.Seq
	data["idempotency_key"] = key
	data["replayed"] = existed
	return Observation{Outcome: "SUCCEEDED", Data: data}, nil
}

// argFields decodes a JSON object of arguments preserving integers exactly
// (json.Number), so the echoed fields stay canonical (no floats). The fake
// target "holds" what it was given; a real target would return its state.
func argFields(args json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(args) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) == nil {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// Reconcile implements Adapter: a pure read by idempotency key.
func (a *FakeTargetAdapter) Reconcile(ctx context.Context, toolID string, args json.RawMessage, key string) (Finding, error) {
	a.reconciles.Add(1)
	if err := ctx.Err(); err != nil {
		return Finding{}, err
	}
	a.mu.Lock()
	eff, ok := a.effects[key]
	a.mu.Unlock()
	observed := ok
	details := map[string]any{}
	if ok {
		details = argFields(eff.Arguments)
		details["effect_seq"] = eff.Seq
		details["tool_id"] = eff.ToolID
	}
	details["target"] = "fake"
	details["idempotency_key"] = key
	details["lookup"] = "by_idempotency_key"
	return Finding{EffectObserved: &observed, Details: details}, nil
}

// safeExecute runs Execute converting a panic into an error.
func safeExecute(ctx context.Context, a Adapter, toolID string, args json.RawMessage, key string) (obs Observation, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("adapter panic: %v", r)
		}
	}()
	return a.Execute(ctx, toolID, args, key)
}
