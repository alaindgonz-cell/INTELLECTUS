package modes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// ErrBudgetExceeded is returned when the engine-local model-call budget is
// spent. (The authoritative repair budget lives in the core.)
var ErrBudgetExceeded = errors.New("modes: model-call budget exceeded")

// Budget bounds model calls across all goroutines.
type Budget struct {
	MaxCalls int64 // <= 0 means unlimited
	used     atomic.Int64
}

// Take reserves one call.
func (b *Budget) Take() error {
	if b == nil || b.MaxCalls <= 0 {
		return nil
	}
	if b.used.Add(1) > b.MaxCalls {
		b.used.Add(-1)
		return ErrBudgetExceeded
	}
	return nil
}

// Used returns calls consumed.
func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// Mode is one model role (planner/coder/tester) bound to a core session.
type Mode struct {
	Role     string
	Session  coreclient.ID
	Provider Provider
	Core     *coreclient.Client
	Budget   *Budget
	// Timeout bounds one model call (0 = none).
	Timeout time.Duration
}

// NewPlanner, NewCoder and NewTester build role adapters.
func NewPlanner(core *coreclient.Client, session coreclient.ID, p Provider) *Mode {
	return &Mode{Role: coreclient.RolePlanner, Session: session, Provider: p, Core: core}
}

// NewCoder builds a coder adapter.
func NewCoder(core *coreclient.Client, session coreclient.ID, p Provider) *Mode {
	return &Mode{Role: coreclient.RoleCoder, Session: session, Provider: p, Core: core}
}

// NewTester builds a tester adapter.
func NewTester(core *coreclient.Client, session coreclient.ID, p Provider) *Mode {
	return &Mode{Role: coreclient.RoleTester, Session: session, Provider: p, Core: core}
}

// Invocation records one model call and its submission.
type Invocation struct {
	Request  Request
	Response Response
	// Raw is exactly what was passed to submit (== Response.Text).
	Raw    string
	Submit *coreclient.SubmitResult
}

// Generate calls the provider only.
func (m *Mode) Generate(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := m.Budget.Take(); err != nil {
		return Response{}, err
	}
	req.Role = m.Role
	cctx := ctx
	if m.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, m.Timeout)
		defer cancel()
	}
	return m.Provider.Complete(cctx, req)
}

// Submit passes raw model text to the core UNMODIFIED.
func (m *Mode) Submit(ctx context.Context, raw string) (*coreclient.SubmitResult, error) {
	return m.Core.Submit(ctx, m.Session, raw)
}

// Invoke = Generate + Submit(raw text unmodified).
func (m *Mode) Invoke(ctx context.Context, req Request) (*Invocation, error) {
	resp, err := m.Generate(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%s model call: %w", m.Role, err)
	}
	req.Role = m.Role
	inv := &Invocation{Request: req, Response: resp, Raw: resp.Text}
	sub, err := m.Submit(ctx, resp.Text)
	if err != nil {
		return inv, fmt.Errorf("%s submit: %w", m.Role, err)
	}
	inv.Submit = sub
	return inv, nil
}

// InvokeParallel runs independent model calls in a bounded worker pool.
// Model calls proceed in parallel; submissions go through the shared client
// (which serializes pipe writes). Results are returned in input order. A
// cancelled context stops jobs that have not started.
func (m *Mode) InvokeParallel(ctx context.Context, reqs []Request, maxWorkers int) ([]*Invocation, []error) {
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	invs := make([]*Invocation, len(reqs))
	errs := make([]error, len(reqs))
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup
	for i := range reqs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			for j := i; j < len(reqs); j++ {
				errs[j] = ctx.Err()
			}
			wg.Wait()
			return invs, errs
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				errs[i] = err
				return
			}
			invs[i], errs[i] = m.Invoke(ctx, reqs[i])
		}(i)
	}
	wg.Wait()
	return invs, errs
}
