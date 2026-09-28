package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/gateway"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakecore"
)

// gatewayCore scripts the dispatch lifecycle for one external action and
// tracks the resulting dispatch state like the core would.
type gatewayCore struct {
	mu    sync.Mutex
	state string
}

func newGatewayCore(t *testing.T) (*fakecore.Server, *coreclient.Client, *gatewayCore) {
	t.Helper()
	g := &gatewayCore{state: "AUTHORIZED_INTENT"}
	srv := fakecore.New()
	srv.Handle("dispatch_begin", func(args json.RawMessage) (any, *coreclient.CoreError) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.state != "AUTHORIZED_INTENT" {
			return map[string]any{"status": "NOT_ELIGIBLE", "reasons": []any{map[string]any{"code": "ALREADY_DISPATCHED"}}}, nil
		}
		g.state = "DISPATCH_STARTED"
		return map[string]any{"status": "STARTED", "attempt_id": "att:1", "tool_id": "export_view",
			"arguments": map[string]any{"approved_root": "sha256:abc"}, "idempotency_key": "k-export-1"}, nil
	})
	srv.Handle("record_outcome", func(args json.RawMessage) (any, *coreclient.CoreError) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.state = "OUTCOME_OBSERVED"
		return map[string]any{"status": "OUTCOME_OBSERVED", "postconditions": "PASS"}, nil
	})
	srv.Handle("record_outcome_unknown", func(args json.RawMessage) (any, *coreclient.CoreError) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.state = "OUTCOME_UNKNOWN"
		return map[string]any{"status": "OUTCOME_UNKNOWN"}, nil
	})
	srv.Handle("pending_reconciliation", func(json.RawMessage) (any, *coreclient.CoreError) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.state != "OUTCOME_UNKNOWN" {
			return map[string]any{"actions": []any{}}, nil
		}
		return map[string]any{"actions": []any{map[string]any{"action_id": "act:7", "attempt_id": "att:1", "tool_id": "export_view",
			"arguments": map[string]any{"approved_root": "sha256:abc"}, "idempotency_key": "k-export-1", "dispatch_state": "OUTCOME_UNKNOWN"}}}, nil
	})
	srv.Handle("record_reconciliation", func(args json.RawMessage) (any, *coreclient.CoreError) {
		g.mu.Lock()
		defer g.mu.Unlock()
		f := fakecore.Arg[coreclient.Finding](args, "finding")
		if f.EffectObserved != nil && *f.EffectObserved {
			g.state = "OUTCOME_OBSERVED"
			return map[string]any{"dispatch_state": "OUTCOME_OBSERVED", "requires_human_review": false}, nil
		}
		return map[string]any{"dispatch_state": "OUTCOME_UNKNOWN", "requires_human_review": true}, nil
	})
	c := srv.Start()
	t.Cleanup(func() { c.Close() })
	return srv, c, g
}

func TestCrashAfterEffectIsUnknownAndReconcilerNeverReExecutes(t *testing.T) {
	for _, mode := range []gateway.FailMode{gateway.FailCrashAfterEffect, gateway.FailPanicAfterEffect} {
		srv, c, _ := newGatewayCore(t)
		ad := gateway.NewFakeTargetAdapter()
		ad.SetFailMode(mode)
		adapters := map[string]gateway.Adapter{"export_view": ad}
		d := &gateway.Dispatcher{Core: c, Session: coreclient.StringID("sess:gw"), Adapters: adapters}
		res, err := d.Dispatch(context.Background(), coreclient.StringID("act:7"))
		if err != nil {
			t.Fatal(err)
		}
		if res.ExecErr == nil || res.Outcome.Status != "OUTCOME_UNKNOWN" {
			t.Fatalf("mode %d: want OUTCOME_UNKNOWN, got %+v", mode, res.Outcome)
		}
		if srv.Count("record_outcome") != 0 || srv.Count("record_outcome_unknown") != 1 {
			t.Fatal("crash after effect must be recorded as unknown, never as FAILED/SUCCEEDED")
		}
		if len(ad.Effects()) != 1 {
			t.Fatal("effect should have been applied before the crash")
		}

		rec := &gateway.Reconciler{Core: c, Session: coreclient.StringID("sess:gw"), Adapters: adapters}
		out, err := rec.RunOnce(context.Background())
		if err != nil || len(out) != 1 {
			t.Fatalf("reconcile: %+v %v", out, err)
		}
		if out[0].Finding.EffectObserved == nil || !*out[0].Finding.EffectObserved || out[0].Result.DispatchState != "OUTCOME_OBSERVED" {
			t.Fatalf("finding %+v result %+v", out[0].Finding, out[0].Result)
		}
		// Run again: nothing pending, still no re-execution.
		if _, err := rec.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ad.ExecuteCount() != 1 {
			t.Fatalf("adapter Execute count = %d, want exactly 1", ad.ExecuteCount())
		}
		if ad.ReconcileCount() != 1 {
			t.Fatalf("reconcile reads = %d, want 1", ad.ReconcileCount())
		}
		if srv.Count("dispatch_begin") != 1 {
			t.Fatal("reconciler must never re-dispatch")
		}
	}
}

func TestErrorBeforeEffectStillUnknownAndReconcileFindsNothing(t *testing.T) {
	_, c, _ := newGatewayCore(t)
	ad := gateway.NewFakeTargetAdapter()
	ad.SetFailMode(gateway.FailErrorBeforeEffect)
	adapters := map[string]gateway.Adapter{"export_view": ad}
	d := &gateway.Dispatcher{Core: c, Session: coreclient.StringID("gw"), Adapters: adapters}
	res, err := d.Dispatch(context.Background(), coreclient.StringID("act:7"))
	if err != nil || res.Outcome.Status != "OUTCOME_UNKNOWN" {
		t.Fatalf("any adapter error must be OUTCOME_UNKNOWN (never assumed failure): %+v %v", res, err)
	}
	out, _ := (&gateway.Reconciler{Core: c, Session: coreclient.StringID("gw"), Adapters: adapters}).RunOnce(context.Background())
	if out[0].Finding.EffectObserved == nil || *out[0].Finding.EffectObserved || !out[0].Result.RequiresHumanReview {
		t.Fatalf("finding %+v", out[0])
	}
	if ad.ExecuteCount() != 1 {
		t.Fatal("no re-execution")
	}
}

func TestHangThenCancelIsUnknown(t *testing.T) {
	srv, c, _ := newGatewayCore(t)
	ad := gateway.NewFakeTargetAdapter()
	ad.SetFailMode(gateway.FailHangAfterEffect)
	d := &gateway.Dispatcher{Core: c, Session: coreclient.StringID("gw"), Adapters: map[string]gateway.Adapter{"export_view": ad}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	res, err := d.Dispatch(ctx, coreclient.StringID("act:7"))
	if err != nil || res.Outcome.Status != "OUTCOME_UNKNOWN" {
		t.Fatalf("hang+cancel: %+v %v", res, err)
	}
	if srv.Count("record_outcome_unknown") != 1 {
		t.Fatal("unknown outcome must be recorded even though ctx was cancelled")
	}
}

func TestSuccessRecordsOutcome(t *testing.T) {
	srv, c, _ := newGatewayCore(t)
	ad := gateway.NewFakeTargetAdapter()
	d := &gateway.Dispatcher{Core: c, Session: coreclient.StringID("gw"), Adapters: map[string]gateway.Adapter{"export_view": ad}}
	res, err := d.Dispatch(context.Background(), coreclient.StringID("act:7"))
	if err != nil || res.Outcome.Status != "OUTCOME_OBSERVED" || srv.Count("record_outcome") != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestCancelledContextNeverDispatches(t *testing.T) {
	srv, c, _ := newGatewayCore(t)
	ad := gateway.NewFakeTargetAdapter()
	d := &gateway.Dispatcher{Core: c, Session: coreclient.StringID("gw"), Adapters: map[string]gateway.Adapter{"export_view": ad}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.Dispatch(ctx, coreclient.StringID("act:7"))
	if !errors.Is(err, gateway.ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if srv.Count("dispatch_begin") != 0 || ad.ExecuteCount() != 0 {
		t.Fatal("cancelled context must stop new dispatches before the launch boundary")
	}
}

func TestInternalCompletedNeedsNoExecution(t *testing.T) {
	srv := fakecore.New()
	srv.Handle("dispatch_begin", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"status": "COMPLETED", "result": map[string]any{"approved_root": "sha256:x"}}, nil
	})
	c := srv.Start()
	defer c.Close()
	ad := gateway.NewFakeTargetAdapter()
	d := &gateway.Dispatcher{Core: c, Session: coreclient.StringID("gw"), Adapters: map[string]gateway.Adapter{"promote_local": ad}}
	res, err := d.Dispatch(context.Background(), coreclient.StringID("act:1"))
	if err != nil || res.Begin.Status != "COMPLETED" || res.Executed || ad.ExecuteCount() != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}
