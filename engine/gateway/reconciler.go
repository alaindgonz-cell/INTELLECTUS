package gateway

import (
	"context"
	"fmt"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// Reconciliation is one reconciled action.
type Reconciliation struct {
	Action  coreclient.PendingAction
	Finding Finding
	ReadErr error
	Result  *coreclient.RecordReconciliationResult
}

// Reconciler resolves OUTCOME_UNKNOWN actions by READING the target. It
// holds no reference to any execute path and never re-dispatches.
type Reconciler struct {
	Core     *coreclient.Client
	Session  coreclient.ID
	Adapters map[string]Adapter
}

// RunOnce reconciles every action the core reports as pending.
func (r *Reconciler) RunOnce(ctx context.Context) ([]Reconciliation, error) {
	pend, err := r.Core.PendingReconciliation(ctx)
	if err != nil {
		return nil, fmt.Errorf("pending_reconciliation: %w", err)
	}
	var out []Reconciliation
	for _, a := range pend.Actions {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		rc := Reconciliation{Action: a}
		ad := r.Adapters[a.ToolID]
		if ad == nil {
			rc.Finding = Finding{EffectObserved: nil, Details: map[string]any{"error": "no adapter for tool " + a.ToolID}}
		} else {
			f, rerr := ad.Reconcile(ctx, a.ToolID, a.Arguments, a.IdempotencyKey)
			if rerr != nil {
				rc.ReadErr = rerr
				f = Finding{EffectObserved: nil, Details: map[string]any{"error": rerr.Error()}}
			}
			rc.Finding = f
		}
		rc.Result, err = r.Core.RecordReconciliation(ctx, r.Session, a.ActionID, rc.Finding)
		if err != nil {
			return out, fmt.Errorf("record_reconciliation %s: %w", a.ActionID, err)
		}
		out = append(out, rc)
	}
	return out, nil
}
