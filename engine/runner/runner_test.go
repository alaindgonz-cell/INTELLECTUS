package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakecore"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

const target = "src/page_size.py"

func manifest(names ...string) coreclient.CheckRequest {
	var cases []coreclient.TestCase
	for _, n := range names {
		cases = append(cases, coreclient.TestCase{Name: n, Input: json.RawMessage(`"x"`), Expect: json.RawMessage(`1`)})
	}
	return coreclient.CheckRequest{
		CheckID: coreclient.StringID("chk:1"), CheckKind: "acceptance_tests",
		TestManifest: coreclient.TestManifest{ID: "T1", Cases: cases},
		Environment:  coreclient.Environment{ID: "ENV1", WorkerKind: "fake"},
	}
}

func TestFakeWorkerScripting(t *testing.T) {
	good, bad := "def f(): return 1\n", "def f(): return 2\n"
	w := runner.NewFakeWorker(target, map[string]map[string]string{
		runner.ContentKey(good): {"a": "PASS", "b": "PASS"},
		runner.ContentKey(bad):  {"a": "PASS", "b": "FAIL"},
	})
	if w.Kind() != "fake" || w.ImplementationDigest() != "fake-worker/v1" || w.Label() != "fake" {
		t.Fatal("fake worker identity")
	}
	chk := manifest("a", "b")
	ctx := context.Background()

	r, err := w.Run(ctx, chk, map[string]string{target: good})
	if err != nil || r.Result != "PASS" || !r.Completed || r.Collected != 2 {
		t.Fatalf("good: %+v %v", r, err)
	}
	r, _ = w.Run(ctx, chk, map[string]string{target: bad})
	if r.Result != "FAIL" || r.Cases[1].Status != "FAIL" || !r.Completed {
		t.Fatalf("bad: %+v", r)
	}
	r, _ = w.Run(ctx, chk, map[string]string{target: "something else"})
	if r.Result != "UNKNOWN" || r.Completed {
		t.Fatalf("unknown content must be UNKNOWN/incomplete: %+v", r)
	}
	r, _ = w.Run(ctx, chk, map[string]string{"other.py": good})
	if r.Result != "UNKNOWN" || r.Completed {
		t.Fatalf("missing target must be UNKNOWN: %+v", r)
	}
	// A manifest case the script does not cover is never claimed PASS.
	r, _ = w.Run(ctx, manifest("a", "b", "c"), map[string]string{target: good})
	if r.Result != "UNKNOWN" || r.Cases[2].Status != "UNKNOWN" {
		t.Fatalf("unscripted case: %+v", r)
	}
	r, _ = w.Run(ctx, manifest("a", "b", "c"), map[string]string{target: bad})
	if r.Result != "FAIL" {
		t.Fatalf("a scripted FAIL dominates: %+v", r)
	}
}

func TestSandboxWorkerFailsClosed(t *testing.T) {
	var w runner.SandboxWorker
	_, err := w.Run(context.Background(), manifest("a"), map[string]string{target: "import os; os.system('rm -rf /')"})
	if !errors.Is(err, runner.ErrSandboxUnavailable) || err.Error() != "generated-code execution disabled: no verified host sandbox (milestone M4)" {
		t.Fatalf("sandbox must fail closed, got %v", err)
	}
}

func checkCore(t *testing.T, workerKind string) (*fakecore.Server, *coreclient.Client) {
	t.Helper()
	srv := fakecore.New()
	srv.Handle("check_plan", func(json.RawMessage) (any, *coreclient.CoreError) {
		chk := manifest("a", "b")
		chk.Environment.WorkerKind = workerKind
		return map[string]any{"checks": []any{chk}, "materialized": map[string]string{target: "def f(): return 1\n"}}, nil
	})
	srv.Handle("report_check", func(args json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"verification_id": "ver:1", "result": fakecore.Arg[string](args, "result")}, nil
	})
	c := srv.Start()
	t.Cleanup(func() { c.Close() })
	return srv, c
}

func TestProtectedRunnerReportsFakeWorker(t *testing.T) {
	srv, c := checkCore(t, "fake")
	w := runner.NewFakeWorker(target, map[string]map[string]string{
		runner.ContentKey("def f(): return 1\n"): {"a": "PASS", "b": "PASS"},
	})
	pr := &runner.ProtectedRunner{Core: c, Session: coreclient.StringID("sess:runner"), Worker: w}
	outs, err := pr.RunChecks(context.Background(), coreclient.StringID("prop:1"))
	if err != nil || !runner.AllPass(outs) {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
	reps := srv.Requests("report_check")
	if len(reps) != 1 {
		t.Fatalf("want 1 report, got %d", len(reps))
	}
	var a coreclient.ReportCheckArgs
	_ = json.Unmarshal(reps[0].Args, &a)
	if a.ImplementationDigest != "fake-worker/v1" || a.Collected != 2 || !a.Completed || a.Result != "PASS" || a.SessionID.String() != "sess:runner" {
		t.Fatalf("report args %+v", a)
	}
}

func TestProtectedRunnerSandboxReportsError(t *testing.T) {
	srv, c := checkCore(t, "sandbox")
	pr := &runner.ProtectedRunner{Core: c, Session: coreclient.StringID("sess:runner"), Worker: runner.SandboxWorker{}}
	outs, err := pr.RunChecks(context.Background(), coreclient.StringID("prop:1"))
	if err != nil {
		t.Fatal(err)
	}
	if runner.AllPass(outs) || outs[0].CoreResult != "ERROR" || !errors.Is(outs[0].WorkerErr, runner.ErrSandboxUnavailable) {
		t.Fatalf("sandbox outcome %+v", outs[0])
	}
	var a coreclient.ReportCheckArgs
	_ = json.Unmarshal(srv.Requests("report_check")[0].Args, &a)
	if a.Completed || a.Result != "ERROR" {
		t.Fatalf("sandbox must report ERROR/incomplete: %+v", a)
	}
}

func TestProtectedRunnerWorkerKindMismatch(t *testing.T) {
	_, c := checkCore(t, "sandbox")
	pr := &runner.ProtectedRunner{Core: c, Session: coreclient.StringID("s"), Worker: runner.NewFakeWorker(target, nil)}
	outs, err := pr.RunChecks(context.Background(), coreclient.StringID("prop:1"))
	if err != nil || outs[0].Reported.Result != "ERROR" {
		t.Fatalf("kind mismatch must report ERROR: %+v %v", outs, err)
	}
}
