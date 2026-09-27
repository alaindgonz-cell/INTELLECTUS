// Package runner is the protected test runner: check_plan -> Worker ->
// report_check. Generated code is NEVER executed by this package. The only
// workers are FakeWorker (scripted outcomes keyed by content digest,
// SIMULATED) and SandboxWorker (fails closed until a verified host sandbox
// exists, milestone M4).
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// Check results a runner may report.
const (
	ResultPass    = "PASS"
	ResultFail    = "FAIL"
	ResultUnknown = "UNKNOWN"
	ResultError   = "ERROR"
	ResultTimeout = "TIMEOUT"
)

// Report is what a worker observed for one check.
type Report struct {
	Result    string
	Cases     []coreclient.CaseResult
	Collected int
	Completed bool
	Summary   string
}

// Worker evaluates one CheckRequest against a materialized candidate tree.
type Worker interface {
	// Kind must equal the environment's worker_kind.
	Kind() string
	// Label is the session label; the core principal is "runner:<Label>".
	Label() string
	// ImplementationDigest identifies the worker implementation to the core.
	ImplementationDigest() string
	Run(ctx context.Context, check coreclient.CheckRequest, materialized map[string]string) (Report, error)
}

// ---- FakeWorker ------------------------------------------------------------

// FakeWorker is a SIMULATED worker. It executes nothing: it looks up the
// SHA-256 (hex, of the raw bytes) of one target file in the materialized
// candidate and returns the scripted per-case outcomes for that content.
// Unknown content yields UNKNOWN with completed=false.
type FakeWorker struct {
	// TargetFile is the path whose content selects the script (e.g. "src/page_size.py").
	TargetFile string
	// Table maps sha256-hex(content) -> case name -> "PASS"/"FAIL".
	Table map[string]map[string]string
}

// NewFakeWorker builds a FakeWorker.
func NewFakeWorker(target string, table map[string]map[string]string) *FakeWorker {
	return &FakeWorker{TargetFile: target, Table: table}
}

// ContentKey is the lookup key for a file content (plain SHA-256 hex; a
// script key only, never a digest the core relies on).
func ContentKey(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// Kind implements Worker.
func (w *FakeWorker) Kind() string { return "fake" }

// Label implements Worker.
func (w *FakeWorker) Label() string { return "fake" }

// ImplementationDigest implements Worker.
func (w *FakeWorker) ImplementationDigest() string { return "fake-worker/v1" }

// Run implements Worker.
func (w *FakeWorker) Run(ctx context.Context, check coreclient.CheckRequest, materialized map[string]string) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	content, ok := materialized[w.TargetFile]
	if !ok {
		return Report{Result: ResultUnknown, Cases: []coreclient.CaseResult{}, Summary: fmt.Sprintf("SIMULATED fake worker: %s not present in candidate", w.TargetFile)}, nil
	}
	key := ContentKey(content)
	script, ok := w.Table[key]
	if !ok {
		return Report{Result: ResultUnknown, Cases: []coreclient.CaseResult{}, Summary: fmt.Sprintf("SIMULATED fake worker: no script for %s content sha256:%s", w.TargetFile, key[:12])}, nil
	}
	rep := Report{Completed: true, Cases: []coreclient.CaseResult{}}
	var failed, unknown []string
	for _, tc := range check.TestManifest.Cases {
		st, ok := script[tc.Name]
		if !ok {
			st = ResultUnknown // not scripted: never claim PASS
		}
		switch st {
		case ResultPass:
		case ResultFail:
			failed = append(failed, tc.Name)
		default:
			unknown = append(unknown, tc.Name)
		}
		rep.Cases = append(rep.Cases, coreclient.CaseResult{Name: tc.Name, Status: st})
	}
	rep.Collected = len(rep.Cases)
	sort.Strings(failed)
	sort.Strings(unknown)
	switch {
	case len(failed) > 0:
		rep.Result = ResultFail
		rep.Summary = fmt.Sprintf("SIMULATED fake worker: failing cases %v", failed)
	case len(unknown) > 0 || rep.Collected == 0:
		rep.Result = ResultUnknown
		rep.Summary = fmt.Sprintf("SIMULATED fake worker: unscripted cases %v", unknown)
	default:
		rep.Result = ResultPass
		rep.Summary = fmt.Sprintf("SIMULATED fake worker: %d/%d cases scripted PASS", rep.Collected, rep.Collected)
	}
	return rep, nil
}

// ---- SandboxWorker ---------------------------------------------------------

// ErrSandboxUnavailable is the fail-closed refusal of SandboxWorker.
var ErrSandboxUnavailable = errors.New("generated-code execution disabled: no verified host sandbox (milestone M4)")

// SandboxWorker is the placeholder for real execution. It always fails
// closed: no generated code runs until a verified host sandbox exists.
type SandboxWorker struct{}

// Kind implements Worker.
func (SandboxWorker) Kind() string { return "sandbox" }

// Label implements Worker.
func (SandboxWorker) Label() string { return "sandbox" }

// ImplementationDigest implements Worker.
func (SandboxWorker) ImplementationDigest() string { return "sandbox-worker/disabled" }

// Run implements Worker: always refuses.
func (SandboxWorker) Run(context.Context, coreclient.CheckRequest, map[string]string) (Report, error) {
	return Report{}, ErrSandboxUnavailable
}
