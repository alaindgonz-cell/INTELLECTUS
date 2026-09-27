package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

// ---- helpers ---------------------------------------------------------------

// requireTools skips only when the sandbox tools are missing.
func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bwrap", "prlimit"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed: %v", tool, err)
		}
	}
}

// workRoot returns a fresh WorkRoot traversable by the sandbox uid.
func workRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "intellectus-sandbox-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newWorker(t *testing.T, cfg Config) *Worker {
	t.Helper()
	requireTools(t)
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = workRoot(t)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

type tcase struct{ name, input, expect string }

func manifest(path, fn string, cases []tcase) coreclient.CheckRequest {
	m := coreclient.TestManifest{
		ID:         "test-manifest",
		Digest:     "sha256:test",
		Entrypoint: &coreclient.Entrypoint{Language: "python", Path: path, Function: fn},
	}
	for _, c := range cases {
		m.Cases = append(m.Cases, coreclient.TestCase{Name: c.name, Input: json.RawMessage(c.input), Expect: json.RawMessage(c.expect)})
	}
	return coreclient.CheckRequest{CheckID: coreclient.StringID("chk:1"), CheckKind: "acceptance", TestManifest: m}
}

func statuses(rep runner.Report) map[string]string {
	out := map[string]string{}
	for _, c := range rep.Cases {
		out[c.Name] = c.Status
	}
	return out
}

func detail(t *testing.T, rep runner.Report, name string) runner.CaseDetail {
	t.Helper()
	for _, d := range rep.Details {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no detail for %s", name)
	return runner.CaseDetail{}
}

func logReport(t *testing.T, rep runner.Report) {
	t.Helper()
	t.Logf("Result=%s Completed=%v Collected=%d Summary=%q", rep.Result, rep.Completed, rep.Collected, rep.Summary)
	for _, d := range rep.Details {
		t.Logf("  %-22s %-7s %5dms observed=%s stdout=%q stderr=%q", d.Name, d.Status, d.DurationMS, d.Observed, truncate(d.Stdout, 160), truncate(d.Stderr, 160))
	}
}

// sandboxProcesses lists host processes running as any of uids.
func sandboxProcesses(uids []uint32) []int {
	var out []int
	for pid := range processTable() {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
				f := strings.Fields(rest)
				if len(f) > 0 {
					u, _ := strconv.ParseUint(f[0], 10, 32)
					if containsUID(uids, uint32(u)) {
						out = append(out, pid)
					}
				}
			}
		}
	}
	return out
}

// ---- (1) page-size example -------------------------------------------------

const pageSizeA0 = "def parse_page_size(raw):\n    return int(raw)\n"

const pageSizeA1 = `import re

def parse_page_size(raw: str) -> int:
    if not isinstance(raw, str):
        raise ValueError("page size must be a string")

    if re.fullmatch(r"[0-9]{1,3}", raw) is None:
        raise ValueError("invalid page size syntax")

    value = int(raw)
    if not 1 <= value <= 100:
        raise ValueError("page size out of range")

    return value
`

var pageSizeCases = []tcase{
	{"ok_1", `"1"`, `{"returns":1}`},
	{"ok_100", `"100"`, `{"returns":100}`},
	{"ok_001", `"001"`, `{"returns":1}`},
	{"zero", `"0"`, `{"raises":"ValueError"}`},
	{"over", `"101"`, `{"raises":"ValueError"}`},
	{"empty", `""`, `{"raises":"ValueError"}`},
	{"plus", `"+1"`, `{"raises":"ValueError"}`},
	{"space", `" 5"`, `{"raises":"ValueError"}`},
	{"decimal", `"1.0"`, `{"raises":"ValueError"}`},
	{"arabic_indic", `"\u0661"`, `{"raises":"ValueError"}`},
	{"none", `null`, `{"raises":"ValueError"}`},
}

func TestPageSizeExample(t *testing.T) {
	w := newWorker(t, Config{})
	check := manifest("src/page_size.py", "parse_page_size", pageSizeCases)

	t.Run("A1_passes", func(t *testing.T) {
		start := time.Now()
		rep, err := w.Run(context.Background(), check, map[string]string{
			"src/page_size.py": pageSizeA1,
			"README.md":        "candidate A1\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		logReport(t, rep)
		t.Logf("11 cases in %s wall (MaxParallel %d)", time.Since(start).Round(time.Millisecond), w.cfg.MaxParallel)
		if rep.Result != runner.ResultPass || !rep.Completed || rep.Collected != len(pageSizeCases) {
			t.Fatalf("A1: want PASS/completed/%d, got %s/%v/%d", len(pageSizeCases), rep.Result, rep.Completed, rep.Collected)
		}
		for i, c := range rep.Cases {
			if c.Name != pageSizeCases[i].name || c.Status != runner.ResultPass {
				t.Errorf("case %d: %+v", i, c)
			}
		}
		if len(rep.Details) != len(pageSizeCases) {
			t.Fatalf("details: %d", len(rep.Details))
		}
		if want := "11/11 cases passed in bwrap sandbox (uid "; !strings.HasPrefix(rep.Summary, want) {
			t.Errorf("summary %q", rep.Summary)
		}
		d := detail(t, rep, "ok_001")
		if string(d.Observed) != `{"returns":1}` || string(d.Input) != `"001"` || string(d.Expected) != `{"returns":1}` {
			t.Errorf("ok_001 detail: %+v", d)
		}
		d = detail(t, rep, "none")
		var obs struct {
			Raises  string   `json:"raises"`
			MRO     []string `json:"mro"`
			Message string   `json:"message"`
		}
		if err := json.Unmarshal(d.Observed, &obs); err != nil || obs.Raises != "ValueError" || obs.Message != "page size must be a string" {
			t.Errorf("none observed %s", d.Observed)
		}
	})

	t.Run("A0_fails_exactly", func(t *testing.T) {
		rep, err := w.Run(context.Background(), check, map[string]string{"src/page_size.py": pageSizeA0})
		if err != nil {
			t.Fatal(err)
		}
		logReport(t, rep)
		// int(): "0"->0, "101"->101, "+1"->1, " 5"->5, "١"->1 all return
		// instead of raising; int(None) raises TypeError (not a ValueError).
		// "" and "1.0" do raise ValueError; "1", "100", "001" return 1/100/1.
		wantFail := []string{"zero", "over", "plus", "space", "arabic_indic", "none"}
		var gotFail []string
		for _, c := range rep.Cases {
			switch c.Status {
			case runner.ResultFail:
				gotFail = append(gotFail, c.Name)
			case runner.ResultPass:
			default:
				t.Errorf("%s: status %s", c.Name, c.Status)
			}
		}
		sort.Strings(wantFail)
		sort.Strings(gotFail)
		if !reflect.DeepEqual(gotFail, wantFail) {
			t.Fatalf("A0 failing set %v, want %v", gotFail, wantFail)
		}
		if rep.Result != runner.ResultFail || !rep.Completed || rep.Collected != 11 {
			t.Fatalf("A0: %s %v %d", rep.Result, rep.Completed, rep.Collected)
		}
		if !strings.HasPrefix(rep.Summary, "5/11 cases passed") || !strings.Contains(rep.Summary, "failing: [zero over plus space arabic_indic none]") {
			t.Errorf("summary %q", rep.Summary)
		}
		if d := detail(t, rep, "space"); string(d.Observed) != `{"returns":5}` {
			t.Errorf("space observed %s", d.Observed)
		}
		if d := detail(t, rep, "none"); !bytes.Contains(d.Observed, []byte(`"raises":"TypeError"`)) {
			t.Errorf("none observed %s", d.Observed)
		}
	})
}

// ---- (2) T19: adversarial candidates --------------------------------------

const adversaryTemplate = `import errno, json, os, signal, socket, sys, time

CANARY = %q
SECRET_NAME = %q
SECRET_VALUE = %q
PORT = %d


def _err(e):
    code = getattr(e, "errno", None)
    return type(e).__name__ + ("/" + errno.errorcode.get(code, "?") if isinstance(code, int) else "")


def _read(path):
    try:
        with open(path, "rb") as f:
            data = f.read()
        print("READ", path, len(data))
        return {"succeeded": True, "error": None}
    except BaseException as e:
        print("read", path, "->", _err(e))
        return {"succeeded": False, "error": type(e).__name__}


def _listdir(path):
    try:
        print("LISTED", path, os.listdir(path))
        return {"succeeded": True, "error": None}
    except BaseException as e:
        print("listdir", path, "->", _err(e))
        return {"succeeded": False, "error": type(e).__name__}


def _connect(host, port):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(3)
    try:
        s.connect((host, port))
        print("CONNECTED", host, port)
        return {"succeeded": True}
    except BaseException as e:
        print("connect", host, port, "->", _err(e))
        return {"succeeded": False}
    finally:
        s.close()


def _secret_visible():
    found = []
    if SECRET_NAME in os.environ:
        found.append("os.environ name")
    if any(SECRET_VALUE in v for v in os.environ.values()):
        found.append("os.environ value")
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        for leaf in ("environ", "cmdline"):
            try:
                with open("/proc/%%s/%%s" %% (pid, leaf), "rb") as f:
                    data = f.read()
                if SECRET_VALUE.encode() in data or SECRET_NAME.encode() in data:
                    found.append(pid + "/" + leaf)
            except BaseException:
                pass
    print("secret found in:", found, "env names:", sorted(os.environ))
    return found


def _write_work():
    wrote = []
    for path, mode in (("/work/pwned.txt", "w"), ("/work/adversary.py", "a"), ("/harness/driver.py", "a")):
        try:
            with open(path, mode) as f:
                f.write("pwned")
            wrote.append(path)
        except BaseException as e:
            print("write", path, "->", _err(e))
    return wrote


def fork_bomb():
    kids, err = [], None
    r, w = os.pipe()
    try:
        while len(kids) < 10000:
            pid = os.fork()
            if pid == 0:
                os.close(w)
                os.read(r, 1)
                os._exit(0)
            kids.append(pid)
    except BaseException as e:
        err = _err(e)
    os.close(w)
    for k in kids:
        os.waitpid(k, 0)
    print("forks", len(kids), "then", err)
    return {"succeeded": len(kids) >= 10000}


def alloc():
    try:
        b = bytearray(4 << 30)
        b[-1] = 1
        print("ALLOCATED 4 GiB")
        return {"succeeded": True}
    except MemoryError as e:
        print("alloc -> MemoryError")
        return {"succeeded": False}


def forge_multi():
    fake = json.dumps({"returns": {"succeeded": False}})
    print(fake)
    print('{"result": "PASS", "cases": [{"name": "forge_multi", "status": "PASS"}]}')
    os.write(3, (fake + "\n").encode())
    os.write(3, (fake + "\n").encode())
    return {"succeeded": False}


def forge_partial():
    os.write(3, b'{"returns": ')
    return {"succeeded": False}


def orphan():
    if os.fork() == 0:
        os.setsid()
        if os.fork() == 0:
            time.sleep(600)
        os._exit(0)
    return "spawned"


def flood():
    sys.stdout.write("A" * (1 << 20))
    sys.stderr.write("E" * (1 << 20))
    return "flooded"


def spin():
    while True:
        pass


ATTACKS = {
    "read_canary": lambda: _read(CANARY),
    "list_canary_dir": lambda: _listdir(os.path.dirname(CANARY)),
    "read_shadow": lambda: _read("/etc/shadow"),
    "proc_root_escape": lambda: _read("/proc/1/root" + CANARY),
    "read_secret_env": lambda: {"succeeded": bool(_secret_visible())},
    "connect_host_listener": lambda: _connect("127.0.0.1", PORT),
    "connect_internet": lambda: _connect("1.1.1.1", 443),
    "write_work": lambda: {"succeeded": bool(_write_work())},
    "fork_bomb": fork_bomb,
    "alloc_4g": alloc,
    "spin": spin,
    "print_flood": flood,
    "forge_multi": forge_multi,
    "forge_partial": forge_partial,
    "orphan_daemon": orphan,
}


def attack(kind):
    return ATTACKS[kind]()
`

const importTimeTemplate = `import os, socket

CANARY = %q
SECRET_VALUE = %q
PORT = %d
_R = {}


def _try(key, fn):
    try:
        fn()
        _R[key] = True
    except BaseException:
        _R[key] = False


_try("canary", lambda: open(CANARY).read())
_try("shadow", lambda: open("/etc/shadow").read())
_R["env"] = any(SECRET_VALUE in v for v in os.environ.values())
_try("listener", lambda: socket.create_connection(("127.0.0.1", PORT), timeout=3).close())
_try("internet", lambda: socket.create_connection(("1.1.1.1", 443), timeout=3).close())
_try("work_write", lambda: open("/work/import_time.txt", "w").write("x"))


def report(_):
    return _R
`

type t19Env struct {
	canary       string
	canaryBefore []byte
	canaryMod    time.Time
	secretName   string
	secretValue  string
	listener     net.Listener
	accepted     *atomic.Int64
}

func newT19Env(t *testing.T) *t19Env {
	t.Helper()
	// A world-readable canary: only the mount namespace keeps it out of reach.
	dir, err := os.MkdirTemp("", "intellectus-t19-canary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &t19Env{canary: filepath.Join(dir, "canary.txt"), accepted: &atomic.Int64{}}
	e.canaryBefore = []byte("canary-" + randomHex(t, 16))
	if err := os.WriteFile(e.canary, e.canaryBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(e.canary)
	e.canaryMod = fi.ModTime()
	e.secretName = "INTELLECTUS_T19_SECRET"
	e.secretValue = "t19-secret-" + randomHex(t, 16)
	t.Setenv(e.secretName, e.secretValue)
	e.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.listener.Close() })
	go func() {
		for {
			c, err := e.listener.Accept()
			if err != nil {
				return
			}
			e.accepted.Add(1)
			c.Close()
		}
	}()
	return e
}

func (e *t19Env) port() int { return e.listener.Addr().(*net.TCPAddr).Port }

func (e *t19Env) assertHostUnaffected(t *testing.T, w *Worker, root string) {
	t.Helper()
	got, err := os.ReadFile(e.canary)
	if err != nil || !bytes.Equal(got, e.canaryBefore) {
		t.Errorf("canary changed: %q %v", got, err)
	}
	if fi, err := os.Stat(e.canary); err != nil || !fi.ModTime().Equal(e.canaryMod) {
		t.Errorf("canary mtime changed")
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.accepted.Load(); n != 0 {
		t.Errorf("host listener accepted %d connections from the sandbox", n)
	}
	if left := sandboxProcesses(w.HostUIDs()); len(left) > 0 && os.Geteuid() == 0 {
		t.Errorf("processes of the sandbox uids survive: %v", left)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("run directories left in WorkRoot: %v", entries)
	}
}

func t19Worker(t *testing.T, root string, timeout time.Duration) *Worker {
	cfg := Config{WorkRoot: root, CaseTimeout: timeout, MaxOutputBytes: 8192}
	if os.Geteuid() == 0 {
		// Distinct host uids per concurrent sandbox so the fork bomb's
		// RLIMIT_NPROC budget (accounted per host uid) is its own.
		cfg.ExtraUIDs = []uint32{65533, 65532, 65531}
	} else {
		cfg.MaxParallel = 1
	}
	return newWorker(t, cfg)
}

func TestT19AdversarialCandidate(t *testing.T) {
	requireTools(t)
	env := newT19Env(t)
	root := workRoot(t)
	w := t19Worker(t, root, 3*time.Second)
	src := fmt.Sprintf(adversaryTemplate, env.canary, env.secretName, env.secretValue, env.port())
	hidden := `{"returns":{"succeeded":false,"error":"FileNotFoundError"}}`
	failed := `{"returns":{"succeeded":false}}`
	cases := []tcase{
		{"read_canary", `"read_canary"`, hidden},
		{"list_canary_dir", `"list_canary_dir"`, hidden},
		{"read_shadow", `"read_shadow"`, hidden},
		{"proc_root_escape", `"proc_root_escape"`, hidden},
		{"read_secret_env", `"read_secret_env"`, failed},
		{"connect_host_listener", `"connect_host_listener"`, failed},
		{"connect_internet", `"connect_internet"`, failed},
		{"write_work", `"write_work"`, failed},
		{"fork_bomb", `"fork_bomb"`, failed},
		{"alloc_4g", `"alloc_4g"`, failed},
		{"spin", `"spin"`, `{"returns":"unreachable"}`},
		{"print_flood", `"print_flood"`, `{"returns":"flooded"}`},
		{"forge_multi", `"forge_multi"`, failed},
		{"forge_partial", `"forge_partial"`, failed},
		{"orphan_daemon", `"orphan_daemon"`, `{"returns":"spawned"}`},
	}
	start := time.Now()
	rep, err := w.Run(context.Background(), manifest("adversary.py", "attack", cases), map[string]string{"adversary.py": src})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, rep)
	t.Logf("15 adversarial cases in %s", time.Since(start).Round(time.Millisecond))
	want := map[string]string{
		"read_canary": "PASS", "list_canary_dir": "PASS", "read_shadow": "PASS", "proc_root_escape": "PASS",
		"read_secret_env": "PASS", "connect_host_listener": "PASS", "connect_internet": "PASS",
		"write_work": "PASS", "fork_bomb": "PASS", "alloc_4g": "PASS", "print_flood": "PASS", "orphan_daemon": "PASS",
		"spin":        "TIMEOUT",
		"forge_multi": "ERROR", "forge_partial": "ERROR",
	}
	if got := statuses(rep); !reflect.DeepEqual(got, want) {
		t.Errorf("statuses:\n got %v\nwant %v", got, want)
	}
	if rep.Result == runner.ResultPass {
		t.Fatal("adversarial run reported PASS")
	}
	if rep.Result != runner.ResultTimeout || !rep.Completed || rep.Collected != len(cases) {
		t.Errorf("report %s completed=%v collected=%d", rep.Result, rep.Completed, rep.Collected)
	}
	// Bounded output.
	flood := detail(t, rep, "print_flood")
	for name, s := range map[string]string{"stdout": flood.Stdout, "stderr": flood.Stderr} {
		if !strings.Contains(s, "[truncated: 1048576 bytes total, 8192 kept]") || len(s) > 8192+64 {
			t.Errorf("%s not truncated: %d bytes, tail %q", name, len(s), s[max(0, len(s)-80):])
		}
	}
	// The timeout killed the spin case promptly.
	if d := detail(t, rep, "spin"); d.DurationMS > 3000+2000 || !bytes.Contains(d.Observed, []byte("wall-clock timeout")) {
		t.Errorf("spin: %dms %s", d.DurationMS, d.Observed)
	}
	// Forged lines were not believed; the forged stdout was not parsed.
	if d := detail(t, rep, "forge_multi"); !bytes.Contains(d.Observed, []byte("3 result lines")) || !strings.Contains(d.Stdout, `"status": "PASS"`) {
		t.Errorf("forge_multi: %s stdout %q", d.Observed, d.Stdout)
	}
	if d := detail(t, rep, "forge_partial"); !bytes.Contains(d.Observed, []byte("not JSON")) {
		t.Errorf("forge_partial: %s", d.Observed)
	}
	// The fork bomb hit the limit far below 10000.
	if d := detail(t, rep, "fork_bomb"); !strings.Contains(d.Stdout, "BlockingIOError") {
		t.Errorf("fork_bomb stdout %q", d.Stdout)
	}
	env.assertHostUnaffected(t, w, root)
}

func TestT19ImportTimeAttacks(t *testing.T) {
	requireTools(t)
	env := newT19Env(t)
	root := workRoot(t)
	w := t19Worker(t, root, 3*time.Second)

	t.Run("attacks_at_import_fail", func(t *testing.T) {
		src := fmt.Sprintf(importTimeTemplate, env.canary, env.secretValue, env.port())
		rep, err := w.Run(context.Background(),
			manifest("pkg/evil.py", "report", []tcase{{"import_time", `null`, `{"returns":{"canary":false,"shadow":false,"env":false,"listener":false,"internet":false,"work_write":false}}`}}),
			map[string]string{"pkg/evil.py": src})
		if err != nil {
			t.Fatal(err)
		}
		logReport(t, rep)
		if rep.Result != runner.ResultPass {
			t.Fatalf("import-time attacks: %s %s", rep.Result, rep.Details[0].Observed)
		}
	})

	t.Run("forged_line_at_import_is_error", func(t *testing.T) {
		src := "import os\nos.write(3, b'{\"returns\":\"ok\"}\\n')\nprint('{\"returns\":\"ok\"}')\n\ndef f(x):\n    return 'ok'\n"
		rep, err := w.Run(context.Background(),
			manifest("forger.py", "f", []tcase{{"a", `1`, `{"returns":"ok"}`}, {"b", `2`, `{"returns":"ok"}`}}),
			map[string]string{"forger.py": src})
		if err != nil {
			t.Fatal(err)
		}
		logReport(t, rep)
		if rep.Result != runner.ResultError || statuses(rep)["a"] != "ERROR" || statuses(rep)["b"] != "ERROR" {
			t.Fatalf("forged import-time line: %s %v", rep.Result, statuses(rep))
		}
	})

	t.Run("hang_at_import_is_timeout", func(t *testing.T) {
		rep, err := w.Run(context.Background(),
			manifest("hang.py", "f", []tcase{{"a", `1`, `{"returns":1}`}}),
			map[string]string{"hang.py": "while True:\n    pass\n\ndef f(x):\n    return x\n"})
		if err != nil {
			t.Fatal(err)
		}
		logReport(t, rep)
		if rep.Result != runner.ResultTimeout {
			t.Fatalf("hang at import: %s", rep.Result)
		}
	})
	env.assertHostUnaffected(t, w, root)
}

// TestKnownLimitationSingleForgedLine documents README "What is NOT
// enforced": the candidate shares the driver's process, so ONE forged
// result line followed by an immediate exit is indistinguishable from a
// genuine result. Tests are evidence of observable behaviour, not proof.
func TestKnownLimitationSingleForgedLine(t *testing.T) {
	w := newWorker(t, Config{})
	src := "import os\nos.write(3, b'{\"returns\":\"ok\"}\\n')\nos._exit(0)\n\ndef f(x):\n    return 'wrong'\n"
	rep, err := w.Run(context.Background(), manifest("forger.py", "f", []tcase{{"a", `1`, `{"returns":"ok"}`}}), map[string]string{"forger.py": src})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, rep)
	if rep.Result != runner.ResultPass {
		t.Fatalf("expected the documented limitation (forged single line accepted), got %s", rep.Result)
	}
}

func TestCPULimitIsTimeout(t *testing.T) {
	w := newWorker(t, Config{CPUSeconds: 1, CaseTimeout: 8 * time.Second})
	rep, err := w.Run(context.Background(), manifest("spin.py", "f", []tcase{{"spin", `null`, `{"returns":1}`}}),
		map[string]string{"spin.py": "def f(x):\n    while True:\n        pass\n"})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, rep)
	d := rep.Details[0]
	if rep.Result != runner.ResultTimeout || !bytes.Contains(d.Observed, []byte("CPU time limit")) || d.DurationMS > 5000 {
		t.Fatalf("CPU limit: %s %s %dms", rep.Result, d.Observed, d.DurationMS)
	}
}

// ---- semantics: FAIL vs ERROR ---------------------------------------------

const semanticsModule = `import math, os, signal, sys


def f(kind):
    if kind == "tuple":
        return (1, 2.0, [True, None])
    if kind == "float_int":
        return 2.0
    if kind == "bool_not_int":
        return True
    if kind == "set":
        return {1}
    if kind == "nan":
        return math.nan
    if kind == "int_key":
        return {1: "a"}
    if kind == "keyerror":
        raise KeyError("k")
    if kind == "sys_exit":
        sys.exit(0)
    if kind == "stdout_json":
        print('{"returns": 42}')
        return 41
    if kind == "suicide":
        os.kill(os.getpid(), signal.SIGKILL)
    if kind == "exit3":
        os._exit(3)
    if kind == "unicode":
        return "\u0661\u00e9\U0001F600"
    return {"echo": kind}
`

func TestSemantics(t *testing.T) {
	w := newWorker(t, Config{})
	cases := []tcase{
		{"tuple_is_list", `"tuple"`, `{"returns":[1,2,[true,null]]}`},
		{"float_equals_int", `"float_int"`, `{"returns":2}`},
		{"bool_is_not_int", `"bool_not_int"`, `{"returns":1}`},
		{"set_not_serializable", `"set"`, `{"returns":[1]}`},
		{"nan_not_serializable", `"nan"`, `{"returns":null}`},
		{"int_key_not_serializable", `"int_key"`, `{"returns":{"1":"a"}}`},
		{"subclass_matches_mro", `"keyerror"`, `{"raises":"LookupError"}`},
		{"sys_exit_is_not_value_error", `"sys_exit"`, `{"raises":"ValueError"}`},
		{"stdout_never_parsed", `"stdout_json"`, `{"returns":42}`},
		{"killed_by_signal", `"suicide"`, `{"returns":1}`},
		{"exit_without_result", `"exit3"`, `{"returns":1}`},
		{"unicode", `"unicode"`, `{"returns":"\u0661\u00e9\ud83d\ude00"}`},
		{"object_key_order", `"x"`, `{"returns":{"echo":"x"}}`},
		{"unsupported_expectation", `"x"`, `{"equals":{"echo":"x"}}`},
		{"returns_when_raise_expected", `"x"`, `{"raises":"Exception"}`},
	}
	rep, err := w.Run(context.Background(), manifest("sem.py", "f", cases), map[string]string{"sem.py": semanticsModule})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, rep)
	want := map[string]string{
		"tuple_is_list": "PASS", "float_equals_int": "PASS", "bool_is_not_int": "FAIL",
		"set_not_serializable": "FAIL", "nan_not_serializable": "FAIL", "int_key_not_serializable": "FAIL",
		"subclass_matches_mro": "PASS", "sys_exit_is_not_value_error": "FAIL", "stdout_never_parsed": "FAIL",
		"killed_by_signal": "ERROR", "exit_without_result": "ERROR", "unicode": "PASS", "object_key_order": "PASS",
		"unsupported_expectation": "ERROR", "returns_when_raise_expected": "FAIL",
	}
	if got := statuses(rep); !reflect.DeepEqual(got, want) {
		t.Errorf("statuses:\n got %v\nwant %v", got, want)
	}
	if rep.Result != runner.ResultFail {
		t.Errorf("result %s", rep.Result)
	}
	// The unsupported expectation was not executed.
	if rep.Collected != len(cases)-1 || !rep.Completed {
		t.Errorf("collected %d completed %v", rep.Collected, rep.Completed)
	}
	if d := detail(t, rep, "unsupported_expectation"); !bytes.Contains(d.Observed, []byte("unsupported expectation format")) {
		t.Errorf("unsupported: %s", d.Observed)
	}
	if d := detail(t, rep, "set_not_serializable"); !bytes.Contains(d.Observed, []byte("result not JSON-serializable: set")) {
		t.Errorf("set: %s", d.Observed)
	}
}

func TestImportFailureIsFail(t *testing.T) {
	w := newWorker(t, Config{})
	rep, err := w.Run(context.Background(), manifest("broken.py", "f", []tcase{{"a", `1`, `{"returns":1}`}}),
		map[string]string{"broken.py": "def f(x:\n"})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, rep)
	if rep.Result != runner.ResultFail || !bytes.Contains(rep.Details[0].Observed, []byte("import failed: SyntaxError")) {
		t.Fatalf("import failure: %s %s", rep.Result, rep.Details[0].Observed)
	}
	rep, err = w.Run(context.Background(), manifest("m.py", "missing", []tcase{{"a", `1`, `{"returns":1}`}}),
		map[string]string{"m.py": "x = 1\n"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Result != runner.ResultFail || !bytes.Contains(rep.Details[0].Observed, []byte("entrypoint not found")) {
		t.Fatalf("missing function: %s %s", rep.Result, rep.Details[0].Observed)
	}
}

// ---- (4) refusals -------------------------------------------------------

func TestRefusals(t *testing.T) {
	w := newWorker(t, Config{})
	good := map[string]string{"m.py": "def f(x):\n    return x\n"}
	one := []tcase{{"a", `1`, `{"returns":1}`}}
	base := manifest("m.py", "f", one)

	noEntry := base
	noEntry.TestManifest.Entrypoint = nil
	js := manifest("m.py", "f", one)
	js.TestManifest.Entrypoint.Language = "javascript"
	absent := manifest("other.py", "f", one)
	badFn := manifest("m.py", "f()", one)
	notPy := manifest("m.txt", "f", one)
	noCases := manifest("m.py", "f", nil)
	dup := manifest("m.py", "f", []tcase{{"a", `1`, `{"returns":1}`}, {"a", `1`, `{"returns":1}`}})
	epTraversal := manifest("../m.py", "f", one)

	cases := []struct {
		name  string
		check coreclient.CheckRequest
		files map[string]string
		want  string
	}{
		{"missing_entrypoint", noEntry, good, "has no entrypoint"},
		{"language", js, good, "not supported"},
		{"entrypoint_absent", absent, good, "not in the candidate tree"},
		{"function_not_identifier", badFn, good, "not a Python identifier"},
		{"not_py", notPy, map[string]string{"m.txt": "x"}, "not a .py module"},
		{"no_cases", noCases, good, "has no cases"},
		{"duplicate_case", dup, good, "duplicate case name"},
		{"entrypoint_traversal", epTraversal, good, "is invalid"},
		{"traversal_dotdot", base, map[string]string{"m.py": good["m.py"], "../evil.py": "x"}, "invalid candidate path"},
		{"traversal_inner", base, map[string]string{"m.py": good["m.py"], "a/../../evil.py": "x"}, "invalid candidate path"},
		{"absolute", base, map[string]string{"m.py": good["m.py"], "/tmp/evil.py": "x"}, "invalid candidate path"},
		{"backslash", base, map[string]string{"m.py": good["m.py"], `a\..\evil.py`: "x"}, "invalid candidate path"},
		{"empty_segment", base, map[string]string{"m.py": good["m.py"], "a//b.py": "x"}, "invalid candidate path"},
		{"dot_segment", base, map[string]string{"m.py": good["m.py"], "./b.py": "x"}, "invalid candidate path"},
		{"file_dir_conflict", base, map[string]string{"m.py": good["m.py"], "a": "x", "a/b.py": "y"}, "candidate path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, err := w.Run(context.Background(), c.check, c.files)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Result != runner.ResultError || rep.Completed || rep.Collected != 0 || len(rep.Cases) != 0 {
				t.Fatalf("want refused ERROR, got %+v", rep)
			}
			if !strings.Contains(rep.Summary, c.want) || !strings.Contains(rep.Summary, "no candidate code was executed") {
				t.Fatalf("summary %q lacks %q", rep.Summary, c.want)
			}
		})
	}
	entries, _ := os.ReadDir(w.cfg.WorkRoot)
	if len(entries) != 0 {
		t.Errorf("run directories left behind: %v", entries)
	}
}

func TestContextCancelIsNotAReport(t *testing.T) {
	w := newWorker(t, Config{CaseTimeout: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := w.Run(ctx, manifest("s.py", "f", []tcase{{"a", `1`, `{"returns":1}`}}), map[string]string{"s.py": "import time\ndef f(x):\n    time.sleep(60)\n"})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("want context error promptly, got %v after %s", err, time.Since(start))
	}
}

// ---- construction / digest -------------------------------------------------

func TestNewValidatesAndDigest(t *testing.T) {
	requireTools(t)
	root := workRoot(t)
	w1 := newWorker(t, Config{WorkRoot: root})
	w2 := newWorker(t, Config{WorkRoot: root})
	if w1.ImplementationDigest() != w2.ImplementationDigest() || !strings.HasPrefix(w1.ImplementationDigest(), DigestPrefix) {
		t.Fatalf("digest unstable or malformed: %s %s", w1.ImplementationDigest(), w2.ImplementationDigest())
	}
	w3 := newWorker(t, Config{WorkRoot: root, CPUSeconds: 11})
	if w3.ImplementationDigest() == w1.ImplementationDigest() {
		t.Error("digest does not cover limits")
	}
	if w1.Kind() != "bwrap" || w1.Label() != "sandbox" {
		t.Errorf("kind/label %s/%s", w1.Kind(), w1.Label())
	}
	t.Logf("digest %s", w1.ImplementationDigest())
	t.Logf("python %s; bwrap %q; python %q", w1.Python(), w1.bwrapVersion, w1.pythonVersion)
	t.Logf("argv: %s %s", w1.prlimit, strings.Join(w1.commandArgs("<RUNDIR>/work", "<RUNDIR>/driver.py"), " "))

	outside := filepath.Join(root, "python3")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{WorkRoot: root, Python: outside}); err == nil || !strings.Contains(err.Error(), "outside /usr") {
		t.Errorf("interpreter outside /usr: %v", err)
	}
	link := filepath.Join(root, "py-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{WorkRoot: root, Python: link}); err == nil {
		t.Error("accepted a symlink to an interpreter outside /usr")
	}
	os.Remove(outside)
	os.Remove(link)
	if _, err := New(Config{WorkRoot: "/usr/share"}); err == nil {
		t.Error("accepted WorkRoot under /usr")
	}
	if os.Geteuid() == 0 {
		private, err := os.MkdirTemp("", "intellectus-private-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(private)
		if _, err := New(Config{WorkRoot: private}); err == nil || !strings.Contains(err.Error(), "not traversable") {
			t.Errorf("accepted a WorkRoot the sandbox uid cannot traverse: %v", err)
		}
	}
	if _, err := New(Config{WorkRoot: root, ExtraUIDs: []uint32{0}}); os.Geteuid() == 0 && err == nil {
		t.Error("accepted uid 0")
	}
}

// ---- (3) SelfTest -------------------------------------------------------

func TestSelfTest(t *testing.T) {
	requireTools(t)
	dir, err := os.MkdirTemp("", "intellectus-selftest-canary-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o755)
	canary := filepath.Join(dir, "canary")
	if err := os.WriteFile(canary, []byte("canary"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INTELLECTUS_SELFTEST_CANARY", "canary-"+randomHex(t, 16))

	// A descriptor leaked into this process without close-on-exec must
	// still not reach the sandbox.
	leaked, err := syscall.Open("/dev/null", syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(leaked)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	blocked := []string{ln.Addr().String()}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		if u, err := url.Parse(os.Getenv(name)); err == nil && u.Host != "" {
			blocked = append(blocked, u.Host)
			break
		}
	}
	repo, _ := filepath.Abs("../..")

	w := newWorker(t, Config{})
	start := time.Now()
	res, err := w.SelfTest(context.Background(), SelfTestConfig{
		ForbiddenPaths: []string{canary, dir, repo},
		CanaryEnv:      []string{"INTELLECTUS_SELFTEST_CANARY"},
		BlockedAddrs:   blocked,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SelfTest OK=%v in %s at %s digest=%s", res.OK, time.Since(start).Round(time.Millisecond), res.VerifiedAt.Format(time.RFC3339), res.ImplementationDigest)
	for _, p := range res.Probes {
		t.Logf("  [%v] %s: %s", p.OK, p.Name, p.Detail)
	}
	if !res.OK {
		t.Fatal("self-test failed on this host")
	}
	var names []string
	for _, p := range res.Probes {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, ProbeNames) {
		t.Fatalf("probes %v, want %v", names, ProbeNames)
	}
	for _, required := range []string{"runs_as_unprivileged", "no_capabilities", "host_files_hidden", "env_scrubbed",
		"network_egress_blocked", "workspace_read_only", "process_limit_enforced", "memory_limit_enforced", "timeout_enforced"} {
		found := false
		for _, n := range names {
			found = found || n == required
		}
		if !found {
			t.Errorf("required probe %s missing", required)
		}
	}
	if res.VerifiedAt.IsZero() || res.ImplementationDigest != w.ImplementationDigest() {
		t.Error("VerifiedAt/digest not set")
	}

	// A vacuous configuration fails closed.
	bad, err := w.SelfTest(context.Background(), SelfTestConfig{
		ForbiddenPaths: []string{filepath.Join(dir, "does-not-exist")},
		CanaryEnv:      []string{"INTELLECTUS_UNSET_CANARY_VARIABLE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bad.OK {
		t.Fatal("self-test passed with vacuous canaries")
	}
	if _, err := w.RequireSelfTest(context.Background(), SelfTestConfig{CanaryEnv: []string{"INTELLECTUS_UNSET_CANARY_VARIABLE"}}); err == nil {
		t.Fatal("RequireSelfTest accepted a failed self-test")
	}
}
