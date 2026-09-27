package modes_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakecore"
	"github.com/alaindgonz-cell/intellectus/engine/modes"
)

func submitCore(t *testing.T) (*fakecore.Server, *coreclient.Client) {
	srv := fakecore.New()
	srv.Handle("submit", func(args json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"status": "RECORDED", "input_id": 1, "proposal_id": "prop:1", "kind": "plan", "payload_digest": "sha256:p"}, nil
	})
	c := srv.Start()
	t.Cleanup(func() { c.Close() })
	return srv, c
}

func TestRawTextPassedUnmodified(t *testing.T) {
	srv, c := submitCore(t)
	raw := "Sure! Here is the plan <b>&amp;</b>   with prose:\n```json\n{\"kind\":\"plan\",\"approved\":true}\n```\ntrailing  "
	p := modes.NewRecordedProvider().Add("planner", 0, raw)
	m := modes.NewPlanner(c, coreclient.StringID("sess:p"), p)
	inv, err := m.Invoke(context.Background(), modes.Request{Phase: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Raw != raw || inv.Submit.Status != "RECORDED" || inv.Submit.InputID != coreclient.ID("1") {
		t.Fatalf("%+v", inv)
	}
	got := fakecore.Arg[string](srv.Requests("submit")[0].Args, "raw")
	if got != raw {
		t.Fatalf("raw modified in transit:\n%q\n%q", got, raw)
	}
	if _, err := m.Invoke(context.Background(), modes.Request{}); !errors.Is(err, modes.ErrNoRecording) {
		t.Fatalf("want ErrNoRecording, got %v", err)
	}
}

type slowProvider struct {
	active, peak atomic.Int64
}

func (s *slowProvider) Name() string { return "slow" }
func (s *slowProvider) Complete(ctx context.Context, r modes.Request) (modes.Response, error) {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-time.After(15 * time.Millisecond):
	case <-ctx.Done():
		return modes.Response{}, ctx.Err()
	}
	return modes.Response{Text: `{"kind":"candidate"}`}, nil
}

func TestInvokeParallelBoundedAndBudgeted(t *testing.T) {
	srv, c := submitCore(t)
	sp := &slowProvider{}
	m := modes.NewCoder(c, coreclient.StringID("sess:c"), sp)
	m.Budget = &modes.Budget{MaxCalls: 7}
	reqs := make([]modes.Request, 10)
	invs, errs := m.InvokeParallel(context.Background(), reqs, 3)
	ok, over := 0, 0
	for i := range reqs {
		switch {
		case errs[i] == nil && invs[i].Submit.Status == "RECORDED":
			ok++
		case errors.Is(errs[i], modes.ErrBudgetExceeded):
			over++
		default:
			t.Fatalf("unexpected: %v", errs[i])
		}
	}
	if ok != 7 || over != 3 {
		t.Fatalf("ok=%d over=%d", ok, over)
	}
	if sp.peak.Load() > 3 {
		t.Fatalf("pool bound violated: peak %d", sp.peak.Load())
	}
	if srv.Count("submit") != 7 {
		t.Fatalf("submits %d", srv.Count("submit"))
	}
}

func TestInvokeParallelCancellation(t *testing.T) {
	srv, c := submitCore(t)
	m := modes.NewCoder(c, coreclient.StringID("sess:c"), &slowProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errs := m.InvokeParallel(ctx, make([]modes.Request, 5), 2)
	for _, e := range errs {
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("want canceled, got %v", e)
		}
	}
	if srv.Count("submit") != 0 {
		t.Fatal("no submissions after cancellation")
	}
}
