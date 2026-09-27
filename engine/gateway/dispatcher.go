package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// ErrCancelled is returned when a dispatch is refused because the context
// was cancelled before the launch boundary.
var ErrCancelled = errors.New("gateway: dispatch cancelled before launch")

// DispatchResult summarises one dispatch.
type DispatchResult struct {
	Begin *coreclient.DispatchBeginResult
	// Executed is true iff the adapter's Execute was called.
	Executed    bool
	Observation Observation
	ExecErr     error
	Outcome     *coreclient.RecordOutcomeResult
}

// Dispatcher launches actions through the core's launch boundary and runs
// external effects via adapters, as the gateway session.
type Dispatcher struct {
	Core     *coreclient.Client
	Session  coreclient.ID
	Adapters map[string]Adapter
}

// Dispatch performs dispatch_begin for actionID and, for STARTED external
// actions, executes the adapter and records the outcome. COMPLETED
// (internal effects such as promote_local) needs no execution.
func (d *Dispatcher) Dispatch(ctx context.Context, actionID coreclient.ID) (*DispatchResult, error) {
	// Operator cancellation stops new dispatches immediately.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	begin, err := d.Core.DispatchBegin(ctx, actionID)
	if err != nil {
		return nil, fmt.Errorf("dispatch_begin: %w", err)
	}
	res := &DispatchResult{Begin: begin}
	if begin.Status != "STARTED" {
		return res, nil // COMPLETED / CANCELLED / EXPIRED / NOT_ELIGIBLE
	}

	// From here on DISPATCH_STARTED is committed: whatever happens must be
	// recorded, even if ctx is cancelled, so recording uses a detached ctx.
	rec := context.WithoutCancel(ctx)
	adapter := d.Adapters[begin.ToolID]
	if adapter == nil {
		// Nothing was sent to any target: this is a positive FAILED.
		res.Outcome, err = d.Core.RecordOutcome(rec, d.Session, actionID, begin.AttemptID, "FAILED",
			map[string]any{"error": "no adapter registered for tool " + begin.ToolID, "executed": false})
		return res, err
	}
	if cerr := ctx.Err(); cerr != nil {
		// Cancelled between the launch boundary and execution: nothing was
		// sent to the target, so this is a positive (known) FAILED.
		res.Outcome, err = d.Core.RecordOutcome(rec, d.Session, actionID, begin.AttemptID, "FAILED",
			map[string]any{"error": "cancelled before execution: " + cerr.Error(), "executed": false})
		return res, err
	}
	res.Executed = true
	obs, execErr := safeExecute(ctx, adapter, begin.ToolID, begin.Arguments, begin.IdempotencyKey)
	res.Observation, res.ExecErr = obs, execErr
	if execErr != nil || (obs.Outcome != "SUCCEEDED" && obs.Outcome != "FAILED") {
		msg := "adapter returned no definite outcome"
		if execErr != nil {
			msg = execErr.Error()
		}
		// Never assume failure: the effect may or may not have happened.
		res.Outcome, err = d.Core.RecordOutcomeUnknown(rec, d.Session, actionID, begin.AttemptID, msg)
		return res, err
	}
	data := obs.Data
	if data == nil {
		data = map[string]any{}
	}
	res.Outcome, err = d.Core.RecordOutcome(rec, d.Session, actionID, begin.AttemptID, obs.Outcome, data)
	return res, err
}
