package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SelfTestConfig names host secrets the self-test proves are out of reach.
type SelfTestConfig struct {
	// ForbiddenPaths are absolute host paths (a canary file, the core DB,
	// the harness workdir, ...) that must be neither visible nor readable
	// inside. Each must exist on the host, or the probe would be vacuous
	// and fails. Well-known sensitive paths that exist are always added.
	ForbiddenPaths []string
	// CanaryEnv are variable names the caller has set in this process's
	// environment; none may be visible inside (by name, or by value in any
	// sandbox process's environment or command line). Each must be set, or
	// the probe would be vacuous and fails.
	CanaryEnv []string
	// BlockedAddrs are host:port TCP endpoints (e.g. the host's proxy, a
	// loopback listener of the engine) that must be unreachable from inside.
	// 1.1.1.1:443 is always added.
	BlockedAddrs []string
	// TimeoutProbe is the wall timeout used for the timeout probe (default
	// min(CaseTimeout, 2s)).
	TimeoutProbe time.Duration
}

// Probe is one self-test attack and whether it failed as required.
type Probe struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// SelfTestResult is the outcome of SelfTest.
type SelfTestResult struct {
	OK                   bool      `json:"ok"`
	Probes               []Probe   `json:"probes"`
	VerifiedAt           time.Time `json:"verified_at"`
	ImplementationDigest string    `json:"implementation_digest"`
}

// ProbeNames lists every probe SelfTest runs, in order. OK requires all.
var ProbeNames = []string{
	"runs_as_unprivileged",
	"no_capabilities",
	"namespaces_unshared",
	"host_files_hidden",
	"env_scrubbed",
	"network_egress_blocked",
	"workspace_read_only",
	"tmp_size_bounded",
	"process_limit_enforced",
	"memory_limit_enforced",
	"timeout_enforced",
	"nested_userns_blocked",
	"no_inherited_fds",
}

// alwaysForbidden are host paths added to ForbiddenPaths when they exist.
var alwaysForbidden = []string{"/etc/shadow", "/etc/passwd", "/root", "/home", "/var", "/run", "/opt", "/srv", "/mnt"}

// alwaysSecretEnv are variable names that must never be visible inside.
var alwaysSecretEnv = []string{
	"ANTHROPIC_API_KEY", "TYPESAFE_API_KEY",
	"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "ALL_PROXY", "all_proxy",
	"GH_TOKEN", "GITHUB_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
}

// SelfTest runs attack probes inside the exact sandbox configuration Run
// uses (same launcher, rlimits, bwrap arguments, interpreter, driver; the
// probes are loaded as the candidate module) and reports, per probe,
// whether the attack failed. The harness must refuse the worker unless
// OK is true. An error is returned only if ctx ends.
func (w *Worker) SelfTest(ctx context.Context, cfg SelfTestConfig) (SelfTestResult, error) {
	res := SelfTestResult{ImplementationDigest: w.digest}
	if cfg.TimeoutProbe <= 0 {
		cfg.TimeoutProbe = min(w.cfg.CaseTimeout, 2*time.Second)
	}
	rundir, workDir, driverFile, err := w.prepareRun(map[string]string{"probe.py": string(probeSource)})
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(rundir)
	pr := &prober{w: w, ctx: ctx, workDir: workDir, driverFile: driverFile, rundir: rundir, cfg: cfg}

	for _, name := range ProbeNames {
		ok, detail := pr.run(name)
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Probes = append(res.Probes, Probe{Name: name, OK: ok, Detail: detail})
	}
	res.OK = len(res.Probes) == len(ProbeNames)
	for _, p := range res.Probes {
		res.OK = res.OK && p.OK
	}
	res.VerifiedAt = time.Now().UTC()
	return res, nil
}

type prober struct {
	w          *Worker
	ctx        context.Context
	workDir    string
	driverFile string
	rundir     string
	cfg        SelfTestConfig
}

// call runs probe.probe(input) in a sandbox and returns the decoded
// returned object.
func (p *prober) call(input map[string]any, timeout time.Duration, observe func(int, <-chan struct{})) (map[string]any, caseRun, error) {
	req, err := json.Marshal(map[string]any{"path": "probe.py", "function": "probe", "input": input})
	if err != nil {
		return nil, caseRun{}, err
	}
	if timeout == 0 {
		timeout = p.w.cfg.CaseTimeout
	}
	run := p.w.runCase(p.ctx, caseSpec{workDir: p.workDir, driverFile: p.driverFile, request: req, timeout: timeout, observe: observe})
	if run.aborted != nil {
		return nil, run, run.aborted
	}
	status, observed := p.w.classify(run, expectation{kind: "returns"}, timeout)
	var obs struct {
		Returns map[string]any `json:"returns"`
		Error   string         `json:"error"`
		Raises  string         `json:"raises"`
		Message string         `json:"message"`
	}
	_ = json.Unmarshal(observed, &obs)
	if obs.Returns == nil {
		return nil, run, fmt.Errorf("probe did not return an observation (status %s, observed %s, stderr %q)", status, observed, truncate(run.stderr.String(), 300))
	}
	return obs.Returns, run, nil
}

func (p *prober) run(name string) (bool, string) {
	switch name {
	case "runs_as_unprivileged":
		return p.unprivileged()
	case "no_capabilities":
		return p.capabilities()
	case "namespaces_unshared":
		return p.namespaces()
	case "host_files_hidden":
		return p.files()
	case "env_scrubbed":
		return p.env()
	case "network_egress_blocked":
		return p.network()
	case "workspace_read_only":
		return p.readOnly()
	case "tmp_size_bounded":
		return p.tmpBounded()
	case "process_limit_enforced":
		return p.processLimit()
	case "memory_limit_enforced":
		return p.memoryLimit()
	case "timeout_enforced":
		return p.timeout()
	case "nested_userns_blocked":
		return p.userns()
	case "no_inherited_fds":
		return p.fds()
	}
	return false, "unknown probe"
}

func (p *prober) unprivileged() (bool, string) {
	var tree []procInfo
	observe := func(pid int, done <-chan struct{}) {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			t := descendants(pid)
			for _, pi := range t {
				if pi.argv0 == p.w.python {
					tree = t
					return
				}
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}
	obs, _, err := p.call(map[string]any{"name": "identity", "hold_ms": 1000}, 0, observe)
	if err != nil {
		return false, err.Error()
	}
	uid, _ := obs["uid"].(float64)
	euid, _ := obs["euid"].(float64)
	var problems []string
	if uid == 0 || euid == 0 {
		problems = append(problems, "uid 0 inside")
	}
	if tree == nil {
		problems = append(problems, "interpreter process not observed on the host")
	}
	var hostUIDs []string
	for _, pi := range tree {
		if len(pi.uids) == 0 {
			problems = append(problems, fmt.Sprintf("pid %d: no Uid line", pi.pid))
		}
		for _, u := range pi.uids {
			if u == 0 {
				problems = append(problems, fmt.Sprintf("host pid %d (%s) has uid 0", pi.pid, filepath.Base(pi.argv0)))
			}
			if p.w.asRoot && !containsUID(p.w.uids, u) {
				problems = append(problems, fmt.Sprintf("host pid %d has unexpected uid %d", pi.pid, u))
			}
		}
		hostUIDs = append(hostUIDs, fmt.Sprintf("%s=%v", filepath.Base(pi.argv0), pi.uids))
	}
	detail := fmt.Sprintf("inside uid=%v euid=%v groups=%v; host view of the sandbox process tree: %s",
		obs["uid"], obs["euid"], obs["groups"], strings.Join(hostUIDs, " "))
	return verdict(problems, detail)
}

func containsUID(list []uint32, u uint32) bool {
	for _, x := range list {
		if x == u {
			return true
		}
	}
	return false
}

func (p *prober) capabilities() (bool, string) {
	obs, _, err := p.call(map[string]any{"name": "capabilities"}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	var problems, parts []string
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		v, _ := obs[k].(string)
		n, err := strconv.ParseUint(v, 16, 64)
		if err != nil || n != 0 {
			problems = append(problems, fmt.Sprintf("%s=%q", k, v))
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	parts = append(parts, fmt.Sprintf("NoNewPrivs=%v Seccomp=%v", obs["NoNewPrivs"], obs["Seccomp"]))
	return verdict(problems, strings.Join(parts, " "))
}

func (p *prober) namespaces() (bool, string) {
	obs, _, err := p.call(map[string]any{"name": "namespaces"}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	var problems, parts []string
	for _, ns := range []string{"user", "pid", "net", "ipc", "uts", "mnt", "cgroup"} {
		host, herr := os.Readlink("/proc/self/ns/" + ns)
		inside, _ := obs[ns].(string)
		switch {
		case herr != nil:
			problems = append(problems, fmt.Sprintf("%s: host namespace unreadable: %v", ns, herr))
		case inside == "" || strings.HasPrefix(inside, "<"):
			problems = append(problems, fmt.Sprintf("%s: inside unreadable (%s)", ns, inside))
		case inside == host:
			problems = append(problems, fmt.Sprintf("%s namespace shared with the host (%s)", ns, host))
		}
		parts = append(parts, fmt.Sprintf("%s %s->%s", ns, host, inside))
	}
	return verdict(problems, strings.Join(parts, "; "))
}

func (p *prober) files() (bool, string) {
	var problems []string
	var paths []string
	seen := map[string]bool{}
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	for _, fp := range p.cfg.ForbiddenPaths {
		if !filepath.IsAbs(fp) {
			problems = append(problems, fmt.Sprintf("%q is not absolute", fp))
			continue
		}
		if _, err := os.Lstat(fp); err != nil {
			problems = append(problems, fmt.Sprintf("%s does not exist on the host (probe would be vacuous)", fp))
			continue
		}
		add(fp)
	}
	extra := append([]string{}, alwaysForbidden...)
	extra = append(extra, p.rundir, p.workDir)
	if exe, err := os.Executable(); err == nil {
		extra = append(extra, exe)
	}
	if wd, err := os.Getwd(); err == nil && wd != "/" {
		extra = append(extra, wd)
	}
	for _, fp := range extra {
		if _, err := os.Lstat(fp); err == nil {
			add(fp)
		}
	}
	obs, _, err := p.call(map[string]any{"name": "files", "paths": paths}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	results, _ := obs["results"].([]any)
	if len(results) != len(paths) {
		return false, fmt.Sprintf("probe returned %d results for %d paths", len(results), len(paths))
	}
	var parts []string
	for _, r := range results {
		e, _ := r.(map[string]any)
		path, _ := e["path"].(string)
		exists, _ := e["exists"].(bool)
		readable, _ := e["readable"].(bool)
		switch {
		case readable:
			problems = append(problems, fmt.Sprintf("%s is readable inside", path))
		case exists:
			parts = append(parts, fmt.Sprintf("%s: present but unreadable (%v)", path, e["error"]))
		default:
			parts = append(parts, fmt.Sprintf("%s: %v", path, e["error"]))
		}
	}
	return verdict(problems, fmt.Sprintf("%d host paths checked: %s", len(paths), strings.Join(parts, "; ")))
}

func (p *prober) env() (bool, string) {
	var problems []string
	forbidden := map[string]bool{}
	hostHashes := map[string]string{}
	hash := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	for _, name := range p.cfg.CanaryEnv {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			problems = append(problems, fmt.Sprintf("canary %s is not set in the engine environment (probe would be vacuous)", name))
			continue
		}
		forbidden[name] = true
		hostHashes[hash(v)] = name
	}
	for _, name := range alwaysSecretEnv {
		forbidden[name] = true
		if v, ok := os.LookupEnv(name); ok && len(v) >= 8 {
			hostHashes[hash(v)] = name
		}
	}
	obs, _, err := p.call(map[string]any{"name": "env"}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	allowed := sandboxEnvAllowed()
	names := stringList(obs["names"])
	procNames := stringList(obs["proc_names"])
	for _, n := range append(append([]string{}, names...), procNames...) {
		if forbidden[n] {
			problems = append(problems, fmt.Sprintf("%s visible inside", n))
		} else if !allowed[n] {
			problems = append(problems, fmt.Sprintf("unexpected variable %s inside", n))
		}
	}
	for _, h := range stringList(obs["value_hashes"]) {
		if name, ok := hostHashes[h]; ok {
			problems = append(problems, fmt.Sprintf("value of %s visible inside", name))
		}
	}
	detail := fmt.Sprintf("interpreter env names %v; other sandbox processes' env names %v; %d host secret values checked by hash, %d names checked",
		names, procNames, len(hostHashes), len(forbidden))
	return verdict(problems, detail)
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (p *prober) network() (bool, string) {
	addrs := []string{"1.1.1.1:443"}
	var hostSide []string
	for _, a := range p.cfg.BlockedAddrs {
		if _, _, err := net.SplitHostPort(a); err != nil {
			return false, fmt.Sprintf("BlockedAddrs entry %q: %v", a, err)
		}
		addrs = append(addrs, a)
		c, err := net.DialTimeout("tcp", a, time.Second)
		if err == nil {
			c.Close()
			hostSide = append(hostSide, a+" reachable from host")
		} else {
			hostSide = append(hostSide, a+" NOT reachable from host")
		}
	}
	obs, _, err := p.call(map[string]any{"name": "network", "addrs": addrs}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	var problems, parts []string
	results, _ := obs["results"].([]any)
	if len(results) != len(addrs) {
		return false, fmt.Sprintf("probe returned %d results for %d addresses", len(results), len(addrs))
	}
	for _, r := range results {
		e, _ := r.(map[string]any)
		if c, _ := e["connected"].(bool); c {
			problems = append(problems, fmt.Sprintf("connected to %v", e["addr"]))
		}
		parts = append(parts, fmt.Sprintf("%v: %v", e["addr"], e["error"]))
	}
	ifaces := stringList(obs["interfaces"])
	for _, i := range ifaces {
		if i != "lo" {
			problems = append(problems, "network interface "+i+" present")
		}
	}
	if obs["dns"] == "resolved" {
		problems = append(problems, "DNS resolution succeeded")
	}
	if obs["udp"] == "sent" {
		problems = append(problems, "UDP datagram to 1.1.1.1:53 was sent")
	}
	detail := fmt.Sprintf("connects: %s; interfaces %v; dns %v; udp %v; %s",
		strings.Join(parts, ", "), ifaces, obs["dns"], obs["udp"], strings.Join(hostSide, ", "))
	return verdict(problems, detail)
}

func (p *prober) readOnly() (bool, string) {
	obs, _, err := p.call(map[string]any{"name": "write"}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	var problems, parts []string
	attempts, _ := obs["attempts"].([]any)
	if len(attempts) == 0 {
		return false, "no write attempts reported"
	}
	for _, a := range attempts {
		e, _ := a.(map[string]any)
		if s, _ := e["succeeded"].(bool); s {
			problems = append(problems, fmt.Sprintf("write to %v succeeded", e["target"]))
		}
		parts = append(parts, fmt.Sprintf("%v: %v", e["target"], e["error"]))
	}
	return verdict(problems, fmt.Sprintf("%s; /tmp (sandbox tmpfs) writable: %v", strings.Join(parts, ", "), obs["tmp_writable"]))
}

func (p *prober) tmpBounded() (bool, string) {
	c := p.w.cfg
	obs, _, err := p.call(map[string]any{"name": "tmpfill", "cap": c.TmpfsBytes + 8<<20, "file_cap": c.MaxFileSizeBytes}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	var problems []string
	written, _ := obs["written"].(float64)
	if obs["error"] == nil {
		problems = append(problems, "filling /tmp never failed")
	}
	if int64(written) > c.TmpfsBytes {
		problems = append(problems, fmt.Sprintf("wrote %d bytes to /tmp, over the %d byte cap", int64(written), c.TmpfsBytes))
	}
	single, _ := obs["single_file"].(map[string]any)
	sb, _ := single["bytes"].(float64)
	if single["error"] == nil || int64(sb) > c.MaxFileSizeBytes {
		problems = append(problems, fmt.Sprintf("single file grew to %d bytes (limit %d) error=%v", int64(sb), c.MaxFileSizeBytes, single["error"]))
	}
	return verdict(problems, fmt.Sprintf("/tmp filled to %d bytes then %v (cap %d); one file stopped at %d bytes with %v (RLIMIT_FSIZE %d)",
		int64(written), obs["error"], c.TmpfsBytes, int64(sb), single["error"], c.MaxFileSizeBytes))
}

func (p *prober) processLimit() (bool, string) {
	const bomb = 10000
	obs, _, err := p.call(map[string]any{"name": "fork", "cap": bomb}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	forks, _ := obs["forks"].(float64)
	var problems []string
	if obs["error"] == nil {
		problems = append(problems, "fork loop never failed")
	}
	if int(forks) >= p.w.cfg.MaxProcs {
		problems = append(problems, fmt.Sprintf("%d forks succeeded (MaxProcs %d)", int(forks), p.w.cfg.MaxProcs))
	}
	return verdict(problems, fmt.Sprintf("fork loop stopped after %d children with %v (RLIMIT_NPROC %d per host uid; loop cap %d)",
		int(forks), obs["error"], p.w.cfg.MaxProcs, bomb))
}

func (p *prober) memoryLimit() (bool, string) {
	n := 2 * p.w.cfg.AddressSpaceBytes
	obs, _, err := p.call(map[string]any{"name": "memory", "bytes": n}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	var problems []string
	if a, _ := obs["allocated"].(bool); a {
		problems = append(problems, fmt.Sprintf("allocated %d bytes", n))
	}
	if e, _ := obs["error"].(string); e != "MemoryError" {
		problems = append(problems, fmt.Sprintf("expected MemoryError, got %v", obs["error"]))
	}
	return verdict(problems, fmt.Sprintf("allocating %d bytes (2x RLIMIT_AS) -> %v", n, obs["error"]))
}

func (p *prober) timeout() (bool, string) {
	// seen is written only by observe, which finishes before runCase
	// returns.
	var seen []int
	observe := func(pid int, done <-chan struct{}) {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			var pids []int
			for _, pi := range descendants(pid) {
				pids = append(pids, pi.pid)
			}
			if len(pids) > len(seen) {
				seen = pids
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}
	limit := p.cfg.TimeoutProbe
	_, run, err := p.call(map[string]any{"name": "spin"}, limit, observe)
	if err != nil && run.aborted != nil {
		return false, err.Error()
	}
	var problems []string
	if !run.timedOut {
		problems = append(problems, fmt.Sprintf("spin loop was not killed by the wall timeout (exit %d, %v)", run.exitCode, err))
	}
	status, _ := p.w.classify(run, expectation{kind: "returns"}, limit)
	if status != statusTimeout {
		problems = append(problems, "case status "+status)
	}
	if run.duration > limit+3*time.Second {
		problems = append(problems, fmt.Sprintf("took %s for a %s timeout", run.duration.Round(time.Millisecond), limit))
	}
	if len(seen) < 2 {
		problems = append(problems, "sandbox process tree not observed")
	}
	deadline := time.Now().Add(2 * time.Second)
	var left []int
	for {
		left = left[:0]
		for _, pid := range seen {
			if processExists(pid) {
				left = append(left, pid)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(left) > 0 {
		problems = append(problems, fmt.Sprintf("processes %v survived the kill", left))
	}
	return verdict(problems, fmt.Sprintf("`while True: pass` killed after %s (timeout %s), status %s; %d host processes of the sandbox observed, none left",
		run.duration.Round(time.Millisecond), limit, status, len(seen)))
}

func (p *prober) userns() (bool, string) {
	obs, _, err := p.call(map[string]any{"name": "userns"}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	var problems []string
	if obs["ctypes"] != "ok" {
		return false, fmt.Sprintf("ctypes unavailable inside: %v", obs["ctypes"])
	}
	for _, k := range []string{"user", "net"} {
		e, _ := obs[k].(map[string]any)
		if rc, _ := e["rc"].(float64); rc == 0 {
			problems = append(problems, "unshare("+k+") succeeded")
		}
	}
	return verdict(problems, fmt.Sprintf("unshare(CLONE_NEWUSER) -> %v; unshare(CLONE_NEWNET) -> %v", obs["user"], obs["net"]))
}

func (p *prober) fds() (bool, string) {
	obs, _, err := p.call(map[string]any{"name": "fds"}, 0, nil)
	if err != nil {
		return false, err.Error()
	}
	list, _ := obs["open"].([]any)
	var open []int
	var problems []string
	for _, x := range list {
		f, _ := x.(float64)
		open = append(open, int(f))
		if f > 3 {
			problems = append(problems, fmt.Sprintf("fd %d open inside", int(f)))
		}
	}
	sort.Ints(open)
	return verdict(problems, fmt.Sprintf("open descriptors inside: %v (0-2 stdio, 3 result pipe)", open))
}

func verdict(problems []string, detail string) (bool, string) {
	if len(problems) > 0 {
		return false, "ATTACK SUCCEEDED OR CHECK INCONCLUSIVE: " + strings.Join(problems, "; ") + " | " + detail
	}
	return true, detail
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ErrSelfTestFailed is returned by RequireSelfTest when a probe failed.
var ErrSelfTestFailed = errors.New("sandbox self-test failed")

// RequireSelfTest runs SelfTest and returns ErrSelfTestFailed (wrapping the
// failed probes) unless every probe passed.
func (w *Worker) RequireSelfTest(ctx context.Context, cfg SelfTestConfig) (SelfTestResult, error) {
	res, err := w.SelfTest(ctx, cfg)
	if err != nil {
		return res, err
	}
	if !res.OK {
		var failed []string
		for _, p := range res.Probes {
			if !p.OK {
				failed = append(failed, p.Name+": "+p.Detail)
			}
		}
		return res, fmt.Errorf("%w: %s", ErrSelfTestFailed, strings.Join(failed, " || "))
	}
	return res, nil
}
