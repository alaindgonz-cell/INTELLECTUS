package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

// Case statuses.
const (
	statusPass    = runner.ResultPass
	statusFail    = runner.ResultFail
	statusError   = runner.ResultError
	statusTimeout = runner.ResultTimeout
)

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type caseOutcome struct {
	detail   runner.CaseDetail
	executed bool
	aborted  error
}

// Run implements runner.Worker. For each manifest case it runs a fresh
// sandboxed interpreter that imports the entrypoint module from the
// candidate tree and calls the entrypoint function with the case input.
// Manifest or candidate-tree problems yield an ERROR report (nothing is
// executed); only context cancellation yields an error.
func (w *Worker) Run(ctx context.Context, check coreclient.CheckRequest, materialized map[string]string) (runner.Report, error) {
	if err := ctx.Err(); err != nil {
		return runner.Report{}, err
	}
	refuse := func(format string, args ...any) (runner.Report, error) {
		return runner.Report{
			Result:  runner.ResultError,
			Cases:   []coreclient.CaseResult{},
			Summary: "bwrap sandbox refused the check: " + fmt.Sprintf(format, args...) + " (no candidate code was executed)",
		}, nil
	}
	m := check.TestManifest
	ep := m.Entrypoint
	switch {
	case ep == nil:
		return refuse("test manifest %q has no entrypoint (language, path, function)", m.ID)
	case ep.Language != "python":
		return refuse("entrypoint language %q is not supported (only \"python\")", ep.Language)
	case validatePath(ep.Path) != nil:
		return refuse("entrypoint path %q is invalid: %v", ep.Path, validatePath(ep.Path))
	case !strings.HasSuffix(ep.Path, ".py"):
		return refuse("entrypoint path %q is not a .py module", ep.Path)
	case !identifierRE.MatchString(ep.Function):
		return refuse("entrypoint function %q is not a Python identifier", ep.Function)
	case len(m.Cases) == 0:
		return refuse("test manifest %q has no cases", m.ID)
	}
	if _, ok := materialized[ep.Path]; !ok {
		return refuse("entrypoint module %q is not in the candidate tree", ep.Path)
	}
	seen := map[string]bool{}
	for _, tc := range m.Cases {
		if tc.Name == "" || seen[tc.Name] {
			return refuse("test manifest %q has an empty or duplicate case name %q", m.ID, tc.Name)
		}
		seen[tc.Name] = true
	}
	if err := validateTree(materialized); err != nil {
		return refuse("%v", err)
	}
	rundir, workDir, driverFile, err := w.prepareRun(materialized)
	if err != nil {
		return refuse("%v", err)
	}
	defer os.RemoveAll(rundir)

	outcomes := make([]caseOutcome, len(m.Cases))
	var wg sync.WaitGroup
	for i, tc := range m.Cases {
		wg.Add(1)
		go func(i int, tc coreclient.TestCase) {
			defer wg.Done()
			outcomes[i] = w.evalCase(ctx, workDir, driverFile, ep, tc)
		}(i, tc)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return runner.Report{}, err
	}
	for _, o := range outcomes {
		if o.aborted != nil {
			return runner.Report{}, o.aborted
		}
	}
	return w.assemble(outcomes), nil
}

func (w *Worker) assemble(outcomes []caseOutcome) runner.Report {
	rep := runner.Report{
		Cases:     make([]coreclient.CaseResult, 0, len(outcomes)),
		Details:   make([]runner.CaseDetail, 0, len(outcomes)),
		Completed: true,
	}
	var passed int
	var failing, errored, timedOut []string
	for _, o := range outcomes {
		d := o.detail
		rep.Cases = append(rep.Cases, coreclient.CaseResult{Name: d.Name, Status: d.Status})
		rep.Details = append(rep.Details, d)
		if o.executed {
			rep.Collected++
		}
		switch d.Status {
		case statusPass:
			passed++
		case statusFail:
			failing = append(failing, d.Name)
		case statusTimeout:
			timedOut = append(timedOut, d.Name)
		case statusError:
			errored = append(errored, d.Name)
		default:
			rep.Completed = false
			errored = append(errored, d.Name)
		}
	}
	n := len(outcomes)
	switch {
	case n > 0 && passed == n && rep.Completed:
		rep.Result = runner.ResultPass
	case len(failing) > 0:
		rep.Result = runner.ResultFail
	case len(timedOut) > 0:
		rep.Result = runner.ResultTimeout
	default:
		rep.Result = runner.ResultError
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d cases passed in bwrap sandbox (uid %s, no network, no host files)", passed, n, w.uidList())
	for _, part := range []struct {
		label string
		names []string
	}{{"failing", failing}, {"timeouts", timedOut}, {"errors", errored}} {
		if len(part.names) > 0 {
			fmt.Fprintf(&b, "; %s: [%s]", part.label, strings.Join(part.names, " "))
		}
	}
	rep.Summary = b.String()
	return rep
}

func (w *Worker) uidList() string {
	parts := make([]string, len(w.uids))
	for i, u := range w.uids {
		parts[i] = fmt.Sprint(u)
	}
	return strings.Join(parts, ",")
}

// evalCase runs and judges one case.
func (w *Worker) evalCase(ctx context.Context, workDir, driverFile string, ep *coreclient.Entrypoint, tc coreclient.TestCase) caseOutcome {
	input := rawOrNull(tc.Input)
	o := caseOutcome{detail: runner.CaseDetail{Name: tc.Name, Input: input, Expected: rawOrNull(tc.Expect)}}
	exp, err := parseExpectation(tc.Expect)
	if err != nil {
		o.detail.Status = statusError
		o.detail.Observed = errorJSON(err.Error())
		return o
	}
	if !json.Valid(tc.Input) && len(bytes.TrimSpace(tc.Input)) != 0 {
		o.detail.Status = statusError
		o.detail.Observed = errorJSON("case input is not valid JSON")
		return o
	}
	req, err := json.Marshal(struct {
		Path     string          `json:"path"`
		Function string          `json:"function"`
		Input    json.RawMessage `json:"input"`
	}{ep.Path, ep.Function, input})
	if err != nil {
		o.detail.Status = statusError
		o.detail.Observed = errorJSON("encoding case request: " + err.Error())
		return o
	}
	run := w.runCase(ctx, caseSpec{workDir: workDir, driverFile: driverFile, request: req, timeout: w.cfg.CaseTimeout})
	if run.aborted != nil {
		o.aborted = run.aborted
		return o
	}
	o.executed = run.startErr == nil
	o.detail.Stdout = run.stdout.String()
	o.detail.Stderr = run.stderr.String()
	o.detail.DurationMS = run.duration.Milliseconds()
	o.detail.Status, o.detail.Observed = w.classify(run, exp, w.cfg.CaseTimeout)
	return o
}

// classify turns what the host observed into a case status. Only a single,
// newline-terminated, well-formed result line from an interpreter that
// exited 0 is ever judged; everything else is TIMEOUT or ERROR.
func (w *Worker) classify(run caseRun, exp expectation, timeout time.Duration) (string, json.RawMessage) {
	switch {
	case run.startErr != nil:
		return statusError, errorJSON("sandbox failed to start: " + run.startErr.Error())
	case run.timedOut:
		return statusTimeout, errorJSON(fmt.Sprintf("wall-clock timeout after %s: sandbox killed", timeout))
	case run.signaled:
		return statusError, errorJSON(fmt.Sprintf("sandbox monitor killed by signal %v", run.signal))
	}
	cpuLimit := time.Duration(w.cfg.CPUSeconds) * time.Second
	if run.exitCode == 128+int(syscall.SIGXCPU) ||
		(run.exitCode == 128+int(syscall.SIGKILL) && run.cpu >= cpuLimit) {
		return statusTimeout, errorJSON(fmt.Sprintf("CPU time limit (%ds) exceeded", w.cfg.CPUSeconds))
	}
	if run.resultOver {
		return statusError, errorJSON(fmt.Sprintf("result exceeds %d bytes", maxResultBytes))
	}
	if run.resultErr != nil {
		return statusError, errorJSON("reading result: " + run.resultErr.Error())
	}
	if run.exitCode != 0 {
		msg := describeExit(run.exitCode)
		if len(run.result) == 0 {
			msg = "no result line; " + msg
		} else {
			msg = "result written but " + msg
		}
		return statusError, errorJSON(msg)
	}
	line, err := singleLine(run.result)
	if err != nil {
		return statusError, errorJSON(err.Error())
	}
	obs, err := parseObservation(line)
	if err != nil {
		return statusError, errorJSON(err.Error())
	}
	status, _ := judge(exp, obs)
	var compact bytes.Buffer
	if json.Compact(&compact, line) != nil {
		return statusError, errorJSON("result line is not JSON")
	}
	return status, compact.Bytes()
}

// singleLine requires exactly one newline-terminated line.
func singleLine(data []byte) ([]byte, error) {
	switch n := bytes.Count(data, []byte("\n")); {
	case len(data) == 0:
		return nil, fmt.Errorf("no result line")
	case n == 0:
		return nil, fmt.Errorf("unterminated result line")
	case n > 1:
		return nil, fmt.Errorf("%d result lines (exactly one allowed)", n)
	case data[len(data)-1] != '\n':
		return nil, fmt.Errorf("trailing bytes after the result line")
	}
	return data[:len(data)-1], nil
}

func describeExit(code int) string {
	if code > 128 && code < 128+65 {
		sig := syscall.Signal(code - 128)
		return fmt.Sprintf("interpreter exit status %d: killed by signal %d (%v), or exited with that status", code, code-128, sig)
	}
	if code == 1 {
		return "interpreter or sandbox setup exited with status 1"
	}
	return fmt.Sprintf("interpreter exited with status %d", code)
}

func errorJSON(msg string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

// rawOrNull keeps valid JSON, maps absent to null and wraps anything else
// as a JSON string so a CaseDetail always marshals.
func rawOrNull(b json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return json.RawMessage("null")
	}
	if json.Valid(t) {
		return append(json.RawMessage(nil), t...)
	}
	s, _ := json.Marshal(string(t))
	return s
}
