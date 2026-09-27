// Command intellectus is the INTELLECTUS agent harness: it runs the Rust
// core, the Claude-backed modes, the Jev advisor and the bubblewrap
// sandbox, and serves the operator UI (activity + chat).
//
//	intellectus init   --project-dir . [--workdir .intellectus]
//	intellectus serve  [--workdir .intellectus] [--addr 127.0.0.1:7777]
//	intellectus up     (init if needed, then serve)
//	intellectus doctor (check the environment)
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/claude"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/harness"
	"github.com/alaindgonz-cell/intellectus/engine/jev"
	"github.com/alaindgonz-cell/intellectus/engine/sandbox"
	"github.com/alaindgonz-cell/intellectus/engine/web"
)

const usage = `INTELLECTUS - supervised agent harness

usage:
  intellectus init   --project-dir DIR [--workdir DIR] [--project-id ID] [--max-repairs N] [--core BIN]
  intellectus serve  [--workdir DIR] [--addr 127.0.0.1:7777] [--core BIN] [--deductor rust|cross:BIN]
                     [--model ID] [--export-dir DIR]
  intellectus up     same flags as init + serve: initializes on first run, then serves
  intellectus doctor [--workdir DIR] [--core BIN]

environment:
  ANTHROPIC_API_KEY   Claude API key (required for the Planner/Coder/Tester/intake roles)
  TYPESAFE_API_KEY    Jev via TypeSafe (optional)   | OPENROUTER_API_KEY  Jev via OpenRouter alpha (optional)
  JEV_MODEL, JEV_PROVIDER, JEV_BASE_URL             override the Jev client
  INTELLECTUS_MODEL   Claude model for every role (default claude-opus-5)
`

type config struct {
	ProjectID  string `json:"project_id"`
	ProjectDir string `json:"project_dir"`
	ExportDir  string `json:"export_dir"`
	CreatedAt  string `json:"created_at"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "intellectus:", err)
		os.Exit(1)
	}
}

type flags struct {
	projectDir, workdir, projectID, core, addr, deductor, model, exportDir string
	maxRepairs                                                            int
}

func parseFlags(name string, args []string) (*flags, error) {
	f := &flags{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&f.projectDir, "project-dir", ".", "project directory whose files form the initial approved tree")
	fs.StringVar(&f.workdir, "workdir", "", "state directory (default: <project-dir>/.intellectus)")
	fs.StringVar(&f.projectID, "project-id", "", "project id (default: project directory name)")
	fs.StringVar(&f.core, "core", "", "path to the intellectus-core binary")
	fs.StringVar(&f.addr, "addr", "127.0.0.1:7777", "UI listen address (keep it on loopback)")
	fs.StringVar(&f.deductor, "deductor", "", "rust | mojo:BIN | cross:BIN (default: cross with the Mojo deductor when built, else rust)")
	fs.StringVar(&f.model, "model", os.Getenv("INTELLECTUS_MODEL"), "Claude model for every role (default claude-opus-5)")
	fs.StringVar(&f.exportDir, "export-dir", "", "export directory (default: <workdir>/exports)")
	fs.IntVar(&f.maxRepairs, "max-repairs", 3, "repair attempts per task (init only)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(f.projectDir)
	if err != nil {
		return nil, err
	}
	f.projectDir = abs
	if f.workdir == "" {
		f.workdir = filepath.Join(f.projectDir, ".intellectus")
	}
	if f.workdir, err = filepath.Abs(f.workdir); err != nil {
		return nil, err
	}
	return f, nil
}

// ---- discovery -------------------------------------------------------------------

func findCore(flagPath string) (string, error) {
	cands := []string{flagPath, os.Getenv("INTELLECTUS_CORE")}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands, filepath.Join(dir, "intellectus-core"),
			filepath.Join(dir, "..", "core", "target", "release", "intellectus-core"),
			filepath.Join(dir, "..", "..", "core", "target", "release", "intellectus-core"),
			filepath.Join(dir, "..", "..", "core", "target", "debug", "intellectus-core"))
	}
	if wd, err := os.Getwd(); err == nil {
		for _, up := range []string{".", "..", "../.."} {
			cands = append(cands, filepath.Join(wd, up, "core", "target", "release", "intellectus-core"),
				filepath.Join(wd, up, "core", "target", "debug", "intellectus-core"))
		}
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return filepath.Abs(c)
		}
	}
	return "", errors.New("intellectus-core not found: build it (cd core && cargo build --release) or pass --core")
}

func defaultDeductor(core string) string {
	cands := []string{os.Getenv("INTELLECTUS_MOJO_DEDUCTOR")}
	dir := filepath.Dir(core)
	cands = append(cands, filepath.Join(dir, "deductor"), filepath.Join(dir, "..", "..", "..", "deductor", "build", "deductor"))
	for _, c := range cands {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			abs, _ := filepath.Abs(c)
			return "cross:" + abs
		}
	}
	return "rust"
}

// ---- init ---------------------------------------------------------------------------

var idRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func cmdUp(args []string) error {
	f, err := parseFlags("up", args)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(f.workdir, "intellectus.db")); errors.Is(err, os.ErrNotExist) {
		if err := doInit(f); err != nil {
			return err
		}
	}
	return doServe(f)
}

func cmdInit(args []string) error {
	f, err := parseFlags("init", args)
	if err != nil {
		return err
	}
	return doInit(f)
}

func doInit(f *flags) error {
	core, err := findCore(f.core)
	if err != nil {
		return err
	}
	db := filepath.Join(f.workdir, "intellectus.db")
	if _, err := os.Stat(db); err == nil {
		return fmt.Errorf("%s already exists (this project is initialized; use serve)", db)
	}
	if err := os.MkdirAll(f.workdir, 0o700); err != nil {
		return err
	}
	pid := f.projectID
	if pid == "" {
		pid = strings.Trim(idRe.ReplaceAllString(filepath.Base(f.projectDir), "-"), "-.")
		if pid == "" {
			pid = "project"
		}
	}
	seed, pub, err := coreclient.GenerateOperatorSeed()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(f.workdir, "operator.key")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		return err
	}
	var trusted []map[string]string
	if w, err := sandbox.New(sandbox.Config{}); err == nil {
		trusted = append(trusted, map[string]string{"principal": "runner:" + w.Label(), "implementation_digest": w.ImplementationDigest()})
	} else {
		fmt.Fprintln(os.Stderr, "warning: sandbox unavailable at init (no trusted test runner registered):", err)
	}
	policy := map[string]any{
		"version": "P1",
		"tools": map[string]any{
			"promote_local": map[string]any{"effect": "internal", "requires_approval": true, "required_checks": []string{"acceptance_tests", "formal_context"}, "idempotent": true},
			"export_view":   map[string]any{"effect": "external", "requires_approval": true, "required_checks": []string{}, "idempotent": true},
		},
		"trusted_issuers": map[string]any{"acceptance_tests": nonNilList(trusted)},
		"protected_paths": []string{"tests/acceptance/", ".intellectus/"},
		"max_repairs":     f.maxRepairs,
		"role_clearance": map[string]string{
			"planner": "internal", "coder": "internal", "tester": "internal", "runner": "internal",
			"gateway": "internal", "intake": "internal", "scheduler": "internal", "advisor": "public",
		},
		"jev": map[string]any{"mode": "SHADOW", "min_confidence_bp": 8000},
	}
	pb, _ := json.MarshalIndent(policy, "", "  ")
	policyPath := filepath.Join(f.workdir, "policy.json")
	if err := os.WriteFile(policyPath, pb, 0o600); err != nil {
		return err
	}
	rootDir, n, err := snapshotProject(f.projectDir, f.workdir)
	if err != nil {
		return err
	}
	defer os.RemoveAll(rootDir)
	out, err := coreclient.RunCLI(context.Background(), core, "init", "--db", db, "--project", pid,
		"--operator-pubkey", pub, "--policy", policyPath, "--root-dir", rootDir)
	if err != nil {
		return err
	}
	cfg := config{ProjectID: pid, ProjectDir: f.projectDir, ExportDir: filepath.Join(f.workdir, "exports"), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	cb, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(f.workdir, "config.json"), cb, 0o600); err != nil {
		return err
	}
	fmt.Printf("Initialized project %q with %d files.\n%s\nOperator key: %s (keep it private)\n", pid, n, strings.TrimSpace(string(out)), keyPath)
	return nil
}

func nonNilList(x []map[string]string) []map[string]string {
	if x == nil {
		return []map[string]string{}
	}
	return x
}

// snapshotProject copies the project's text files (excluding VCS, caches and
// the workdir) into a temporary directory for the genesis tree.
func snapshotProject(src, workdir string) (string, int, error) {
	tmp, err := os.MkdirTemp("", "intellectus-root-")
	if err != nil {
		return "", 0, err
	}
	skipDirs := map[string]bool{".git": true, ".hg": true, ".svn": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true, ".intellectus": true, "target": true, ".mypy_cache": true, ".pytest_cache": true}
	n, total := 0, 0
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != src && (skipDirs[d.Name()] || p == workdir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and devices are not part of the tree
		}
		info, err := d.Info()
		if err != nil || info.Size() > 512<<10 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || !utf8.Valid(b) {
			return nil // binary files are not supported by the text tree
		}
		rel, _ := filepath.Rel(src, p)
		n++
		total += len(b)
		if n > 3000 || total > 32<<20 {
			return errors.New("project too large for an INTELLECTUS tree (max 3000 files / 32 MiB of text)")
		}
		dst := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		os.RemoveAll(tmp)
		return "", 0, err
	}
	return tmp, n, nil
}

// ---- serve -----------------------------------------------------------------------------

func cmdServe(args []string) error {
	f, err := parseFlags("serve", args)
	if err != nil {
		return err
	}
	return doServe(f)
}

func loadConfig(workdir string) (config, []byte, error) {
	var cfg config
	b, err := os.ReadFile(filepath.Join(workdir, "config.json"))
	if err != nil {
		return cfg, nil, fmt.Errorf("%s is not initialized (run intellectus init): %w", workdir, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, nil, err
	}
	seedHex, err := os.ReadFile(filepath.Join(workdir, "operator.key"))
	if err != nil {
		return cfg, nil, err
	}
	seed, err := coreclient.ParseSeedHex(string(seedHex))
	return cfg, seed, err
}

func doServe(f *flags) error {
	cfg, seed, err := loadConfig(f.workdir)
	if err != nil {
		return err
	}
	core, err := findCore(f.core)
	if err != nil {
		return err
	}
	ded := f.deductor
	if ded == "" {
		ded = defaultDeductor(core)
	}
	exportDir := f.exportDir
	if exportDir == "" {
		exportDir = cfg.ExportDir
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
	}
	coreLog, err := os.OpenFile(filepath.Join(f.workdir, "core.stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer coreLog.Close()
	db := filepath.Join(f.workdir, "intellectus.db")
	proc, err := coreclient.Spawn(core, []string{"serve", "--db", db, "--deductor", ded}, coreLog)
	if err != nil {
		return err
	}
	defer proc.Close(5 * time.Second)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hello, err := proc.Client.Hello(ctx)
	if err != nil {
		return fmt.Errorf("core did not start (see %s): %w", coreLog.Name(), err)
	}
	logf("core: project %s, head %d, deductor %s", hello.ProjectID, hello.HeadSequence, hello.Deductor.Kind)
	op, err := coreclient.NewOperator(seed, cfg.ProjectID)
	if err != nil {
		return err
	}

	models := map[string]string{}
	if f.model != "" {
		for _, r := range []string{"intake", "planner", "coder", "tester"} {
			models[r] = f.model
		}
	}
	llmClient := claude.New(claude.Config{Models: models})
	d := llmClient.Describe()
	logf("claude: configured=%v models=%v %s", d.Configured, d.Models, d.Detail)

	jevClient := jev.New(jev.Config{})
	jd := jevClient.Describe()
	var adv advisor.DecisionAdvisor = jevClient
	info := harness.AdvisorInfo{Configured: jd.Configured, Provider: jd.Provider, Model: jd.Model, Detail: jd.Detail}
	logf("jev: configured=%v provider=%s model=%s %s", jd.Configured, jd.Provider, jd.Model, jd.Detail)

	worker, sbInfo := startSandbox(ctx, f.workdir, db, logf)

	h, err := harness.New(harness.Options{
		Core: proc.Client, Operator: op, LLM: llmClient, Advisor: adv, AdvisorInfo: info,
		Worker: worker, Sandbox: sbInfo, Workdir: f.workdir, ExportDir: exportDir,
		CostEstimator: claude.EstimateCostUSD, Logf: logf,
	})
	if err != nil {
		return err
	}
	if err := h.Start(ctx); err != nil {
		return err
	}
	defer h.Close()

	token, err := web.NewToken()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: (&web.Server{H: h, Token: token, Logf: logf}).Handler(), ReadHeaderTimeout: 10 * time.Second}
	link := fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), token)
	_ = os.WriteFile(filepath.Join(f.workdir, "ui-url.txt"), []byte(link+"\n"), 0o600)
	fmt.Printf("\nINTELLECTUS is running. Open:\n\n  %s\n\n(also written to %s; Ctrl-C to stop)\n\n", link, filepath.Join(f.workdir, "ui-url.txt"))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	logf("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}

// startSandbox builds the bubblewrap worker and runs its isolation
// self-test. A worker whose self-test fails is never used.
func startSandbox(ctx context.Context, workdir, db string, logf func(string, ...any)) (*sandbox.Worker, harness.SandboxInfo) {
	info := harness.SandboxInfo{Kind: "bwrap"}
	w, err := sandbox.New(sandbox.Config{})
	if err != nil {
		info.Detail = err.Error()
		logf("sandbox: unavailable: %v", err)
		return nil, info
	}
	info.Available = true
	info.ImplementationDigest = w.ImplementationDigest()
	// Canary secret in this process's environment: must be invisible inside.
	canary := make([]byte, 16)
	_, _ = rand.Read(canary)
	_ = os.Setenv("INTELLECTUS_SANDBOX_CANARY", hex.EncodeToString(canary))
	// A loopback listener on the host: must be unreachable from inside.
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	blocked := []string{"1.1.1.1:443"}
	if lerr == nil {
		blocked = append(blocked, ln.Addr().String())
		defer ln.Close()
	}
	for _, v := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if u, err := url.Parse(os.Getenv(v)); err == nil && u.Host != "" {
			blocked = append(blocked, u.Host)
		}
	}
	home, _ := os.UserHomeDir()
	forbidden := []string{db, workdir, filepath.Join(workdir, "operator.key")}
	if home != "" {
		forbidden = append(forbidden, home)
	}
	res, err := w.SelfTest(ctx, sandbox.SelfTestConfig{ForbiddenPaths: forbidden, CanaryEnv: []string{"INTELLECTUS_SANDBOX_CANARY"}, BlockedAddrs: dedupe(blocked)})
	_ = os.Unsetenv("INTELLECTUS_SANDBOX_CANARY")
	for _, p := range res.Probes {
		info.Probes = append(info.Probes, harness.Probe{Name: p.Name, OK: p.OK, Detail: p.Detail})
	}
	info.VerifiedAt = res.VerifiedAt.UnixMilli()
	if err != nil || !res.OK {
		msg := "isolation self-test failed"
		if err != nil {
			msg += ": " + err.Error()
		}
		info.Detail = msg
		logf("sandbox: %s; code execution DISABLED", msg)
		return nil, info
	}
	info.Verified = true
	logf("sandbox: verified (%d probes passed), %s", len(res.Probes), w.ImplementationDigest())
	return w, info
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// ---- doctor ---------------------------------------------------------------------------

func cmdDoctor(args []string) error {
	f, err := parseFlags("doctor", args)
	if err != nil {
		return err
	}
	ok := true
	check := func(name string, good bool, detail string) {
		mark := "ok  "
		if !good {
			mark, ok = "FAIL", false
		}
		fmt.Printf("[%s] %-22s %s\n", mark, name, detail)
	}
	core, err := findCore(f.core)
	check("core binary", err == nil, firstNonEmpty(core, errString(err)))
	if err == nil {
		check("deductor", true, defaultDeductor(core))
	}
	d := claude.New(claude.Config{}).Describe()
	check("claude", d.Configured, fmt.Sprintf("models=%v %s", d.Models, d.Detail))
	jd := jev.New(jev.Config{}).Describe()
	fmt.Printf("[%s] %-22s provider=%s model=%s %s\n", map[bool]string{true: "ok  ", false: "warn"}[jd.Configured], "jev (optional)", jd.Provider, jd.Model, jd.Detail)
	w, info := startSandbox(context.Background(), f.workdir, filepath.Join(f.workdir, "intellectus.db"), func(string, ...any) {})
	check("sandbox", w != nil, firstNonEmpty(info.Detail, info.ImplementationDigest))
	for _, p := range info.Probes {
		fmt.Printf("       %-5v %-30s %s\n", p.OK, p.Name, p.Detail)
	}
	if _, err := os.Stat(filepath.Join(f.workdir, "config.json")); err != nil {
		fmt.Printf("[info] %-22s %s not initialized (intellectus init)\n", "project", f.workdir)
	}
	if !ok {
		return errors.New("some checks failed")
	}
	return nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ = io.Discard
