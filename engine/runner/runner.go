package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// Outcome is one check as reported and as decided by the core.
type Outcome struct {
	Check          coreclient.CheckRequest
	Reported       Report
	WorkerErr      error
	VerificationID coreclient.ID
	// CoreResult is the core's decision (it may downgrade the report), or
	// "REJECTED" when the core refused the report (e.g. UNTRUSTED_ISSUER).
	CoreResult  string
	CoreReasons coreclient.Reasons
}

// ProtectedRunner drives check_plan -> Worker -> report_check. It runs as
// the runner session ("runner:<label>"); only the core decides PASS.
type ProtectedRunner struct {
	Core    *coreclient.Client
	Session coreclient.ID
	Worker  Worker
	// MaxParallel bounds concurrent worker runs within one plan (default 4).
	MaxParallel int
}

// RunChecks runs every check of the proposal's check plan. Worker runs may
// proceed in parallel; reports go through the shared serialized client.
func (r *ProtectedRunner) RunChecks(ctx context.Context, proposalID coreclient.ID) ([]Outcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan, err := r.Core.CheckPlan(ctx, proposalID)
	if err != nil {
		return nil, fmt.Errorf("check_plan: %w", err)
	}
	n := r.MaxParallel
	if n <= 0 {
		n = 4
	}
	out := make([]Outcome, len(plan.Checks))
	errs := make([]error, len(plan.Checks))
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, chk := range plan.Checks {
		wg.Add(1)
		go func(i int, chk coreclient.CheckRequest) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i], errs[i] = r.runOne(ctx, chk, plan.Materialized)
		}(i, chk)
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

func (r *ProtectedRunner) runOne(ctx context.Context, chk coreclient.CheckRequest, materialized map[string]string) (Outcome, error) {
	o := Outcome{Check: chk}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	switch {
	case chk.Environment.WorkerKind != "" && chk.Environment.WorkerKind != r.Worker.Kind():
		o.Reported = Report{Result: ResultError, Cases: []coreclient.CaseResult{},
			Summary: fmt.Sprintf("environment requires worker_kind %q; this runner is %q", chk.Environment.WorkerKind, r.Worker.Kind())}
	default:
		rep, err := r.Worker.Run(ctx, chk, materialized)
		o.WorkerErr = err
		switch {
		case err == nil:
			o.Reported = rep
		case errors.Is(err, context.DeadlineExceeded):
			o.Reported = Report{Result: ResultTimeout, Cases: []coreclient.CaseResult{}, Summary: err.Error()}
		case errors.Is(err, context.Canceled):
			// Operator cancellation: do not report a fabricated result.
			return o, err
		default:
			o.Reported = Report{Result: ResultError, Cases: []coreclient.CaseResult{}, Summary: err.Error()}
		}
	}
	// A context cancelled meanwhile stops the report from being sent; the
	// check then simply stays unreported (never a fabricated PASS).
	res, err := r.Core.ReportCheck(ctx, coreclient.ReportCheckArgs{
		SessionID:            r.Session,
		CheckID:              chk.CheckID,
		Result:               o.Reported.Result,
		ImplementationDigest: r.Worker.ImplementationDigest(),
		Cases:                o.Reported.Cases,
		Collected:            o.Reported.Collected,
		Completed:            o.Reported.Completed,
		Summary:              o.Reported.Summary,
	})
	if err != nil {
		return o, fmt.Errorf("report_check %s: %w", chk.CheckID, err)
	}
	o.VerificationID = res.VerificationID
	o.CoreResult = res.Result
	o.CoreReasons = res.Reasons
	if res.Status == "REJECTED" {
		o.CoreResult = "REJECTED"
	}
	return o, nil
}

// AllPass reports whether the core decided PASS for every check (and there
// was at least one).
func AllPass(outs []Outcome) bool {
	if len(outs) == 0 {
		return false
	}
	for _, o := range outs {
		if o.CoreResult != ResultPass {
			return false
		}
	}
	return true
}

// FailureCodes maps non-PASS core decisions to the core's reason-code
// vocabulary used by `route`: FAIL -> CHECK_FAILED, a refused report ->
// its reason codes (e.g. UNTRUSTED_ISSUER), anything else -> CHECK_NOT_PASSED.
// Codes are de-duplicated, order preserved.
func FailureCodes(outs []Outcome) []string {
	var codes []string
	seen := map[string]bool{}
	add := func(c string) {
		if c != "" && !seen[c] {
			seen[c] = true
			codes = append(codes, c)
		}
	}
	for _, o := range outs {
		switch o.CoreResult {
		case ResultPass:
		case ResultFail:
			add("CHECK_FAILED")
		case "REJECTED":
			for _, c := range o.CoreReasons.Codes() {
				add(c)
			}
		default:
			add("CHECK_NOT_PASSED")
		}
	}
	return codes
}
