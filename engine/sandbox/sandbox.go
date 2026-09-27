// Package sandbox is the REAL isolated test runner (milestone M4): a
// runner.Worker that executes each test case of a protected acceptance
// manifest against a candidate Python module in a fresh bubblewrap sandbox.
//
// Every case gets its own interpreter process, launched as
//
//	prlimit <rlimits> -- bwrap <namespaces, read-only /usr, no host files> -- python -s -B /harness/driver.py
//
// as an unprivileged host uid (65534 when the engine runs as root), in its
// own process group, with a scrubbed environment and only fds 0-3. The case
// request goes in on stdin; the embedded driver (driver.py) writes exactly
// one JSON result line to fd 3. See README.md for the threat model, what is
// enforced, what is not, and what was verified on the development host.
package sandbox

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/runner"
)

//go:embed driver.py
var driverSource []byte

//go:embed probe.py
var probeSource []byte

// DigestPrefix prefixes ImplementationDigest.
const DigestPrefix = "bwrap-python/v1:sha256:"

// Paths inside the sandbox.
const (
	sandboxWork   = "/work"
	sandboxDriver = "/harness/driver.py"
	// SandboxUID/SandboxGID are the ids the code sees inside the sandbox.
	SandboxUID = 65534
	SandboxGID = 65534
)

// ReadOnlyRoot is the only host directory tree bound into the sandbox (read
// only). The interpreter must live under it.
const ReadOnlyRoot = "/usr"

// Internal bounds that are not configurable.
const (
	maxResultBytes = 1 << 20 // one result line
	maxTreeFiles   = 10000
	maxTreeBytes   = 64 << 20
	waitDelay      = 2 * time.Second
)

// Config configures a Worker. Zero values take the documented defaults.
type Config struct {
	// BwrapPath, PrlimitPath and Python name the executables (default
	// "bwrap", "prlimit", "python3", looked up in PATH). Each is resolved to
	// an absolute path with symlinks evaluated and must live under /usr
	// (the tree the sandbox binds read-only); otherwise New refuses.
	BwrapPath   string
	PrlimitPath string
	Python      string

	// UID/GID are the host credentials every sandbox runs as when the engine
	// runs as root (default 65534/65534; 0 is refused). When the engine is
	// not root, sandboxes run as the engine's own uid and these are ignored.
	UID, GID uint32
	// ExtraUIDs (root only, optional) are additional host uids. Concurrent
	// sandboxes are assigned distinct uids from {UID} ∪ ExtraUIDs while
	// enough are idle. RLIMIT_NPROC is accounted by the kernel per HOST uid
	// across all namespaces, so sandboxes sharing a uid share one process
	// budget (a fork bomb in one can make a concurrent one fail to start:
	// ERROR, never PASS). Give MaxParallel distinct uids to isolate them.
	ExtraUIDs []uint32

	// Per-process rlimits applied with prlimit (defaults: 10 s CPU, 1 GiB
	// address space, 64 processes, 256 open files, 16 MiB file size; core
	// dumps are always disabled).
	CPUSeconds        int
	AddressSpaceBytes int64
	MaxProcs          int
	MaxOpenFiles      int
	MaxFileSizeBytes  int64
	// TmpfsBytes caps the sandbox's only writable filesystem, /tmp
	// (default 64 MiB).
	TmpfsBytes int64

	// CaseTimeout is the wall-clock limit per case (default 10 s); the whole
	// process group is killed and the case is TIMEOUT.
	CaseTimeout time.Duration
	// MaxParallel bounds concurrently running sandboxes across all Run and
	// SelfTest calls on this Worker (default 4).
	MaxParallel int
	// MaxOutputBytes bounds the stdout and stderr kept per case (default 8192).
	MaxOutputBytes int
	// WorkRoot is where per-run candidate trees are written (default
	// os.TempDir()). It must not be under /usr, and when the engine runs as
	// root every directory on its path must be traversable by others,
	// because bwrap opens bind sources as the unprivileged uid.
	WorkRoot string
}

// Worker runs manifest cases in bubblewrap sandboxes. It implements
// runner.Worker. It is safe for concurrent use.
type Worker struct {
	cfg     Config
	bwrap   string
	prlimit string
	python  string

	asRoot    bool
	uids      []uint32
	gid       uint32
	slots     chan uint32
	rootLinks []string // bwrap args reproducing /bin, /lib, ... (merged-usr aware)

	bwrapVersion  string
	pythonVersion string
	description   []byte
	digest        string
}

var _ runner.Worker = (*Worker)(nil)

// New validates the host tools and configuration and computes the
// implementation digest. It does not run a sandbox; call SelfTest before
// trusting the worker.
func New(cfg Config) (*Worker, error) {
	if cfg.UID == 0 {
		cfg.UID = 65534
	}
	if cfg.GID == 0 {
		cfg.GID = 65534
	}
	if cfg.CPUSeconds == 0 {
		cfg.CPUSeconds = 10
	}
	if cfg.AddressSpaceBytes == 0 {
		cfg.AddressSpaceBytes = 1 << 30
	}
	if cfg.MaxProcs == 0 {
		cfg.MaxProcs = 64
	}
	if cfg.MaxOpenFiles == 0 {
		cfg.MaxOpenFiles = 256
	}
	if cfg.MaxFileSizeBytes == 0 {
		cfg.MaxFileSizeBytes = 16 << 20
	}
	if cfg.TmpfsBytes == 0 {
		cfg.TmpfsBytes = 64 << 20
	}
	if cfg.CaseTimeout == 0 {
		cfg.CaseTimeout = 10 * time.Second
	}
	if cfg.MaxParallel == 0 {
		cfg.MaxParallel = 4
	}
	if cfg.MaxOutputBytes == 0 {
		cfg.MaxOutputBytes = 8192
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = os.TempDir()
	}
	switch {
	case cfg.CPUSeconds < 1:
		return nil, errors.New("sandbox: CPUSeconds must be >= 1")
	case cfg.AddressSpaceBytes < 64<<20:
		return nil, errors.New("sandbox: AddressSpaceBytes must be >= 64 MiB (the interpreter needs it)")
	case cfg.MaxProcs < 4:
		return nil, errors.New("sandbox: MaxProcs must be >= 4 (bwrap monitor, bwrap init, interpreter)")
	case cfg.MaxOpenFiles < 16:
		return nil, errors.New("sandbox: MaxOpenFiles must be >= 16")
	case cfg.MaxFileSizeBytes < 1:
		return nil, errors.New("sandbox: MaxFileSizeBytes must be >= 1")
	case cfg.TmpfsBytes < 1<<20:
		return nil, errors.New("sandbox: TmpfsBytes must be >= 1 MiB")
	case cfg.CaseTimeout < 100*time.Millisecond:
		return nil, errors.New("sandbox: CaseTimeout must be >= 100ms")
	case cfg.MaxParallel < 1:
		return nil, errors.New("sandbox: MaxParallel must be >= 1")
	case cfg.MaxOutputBytes < 1:
		return nil, errors.New("sandbox: MaxOutputBytes must be >= 1")
	}

	w := &Worker{asRoot: os.Geteuid() == 0}
	var err error
	if w.bwrap, err = resolveTool(cfg.BwrapPath, "bwrap", w.asRoot); err != nil {
		return nil, err
	}
	if w.prlimit, err = resolveTool(cfg.PrlimitPath, "prlimit", w.asRoot); err != nil {
		return nil, err
	}
	if w.python, err = resolveTool(cfg.Python, "python3", w.asRoot); err != nil {
		return nil, err
	}

	if w.asRoot {
		w.uids = []uint32{cfg.UID}
		seen := map[uint32]bool{cfg.UID: true}
		for _, u := range cfg.ExtraUIDs {
			if u == 0 {
				return nil, errors.New("sandbox: ExtraUIDs must not contain 0")
			}
			if !seen[u] {
				seen[u] = true
				w.uids = append(w.uids, u)
			}
		}
		w.gid = cfg.GID
	} else {
		w.uids = []uint32{uint32(os.Geteuid())}
		w.gid = uint32(os.Getegid())
		cfg.ExtraUIDs = nil
	}
	w.slots = make(chan uint32, cfg.MaxParallel)
	for i := 0; i < cfg.MaxParallel; i++ {
		w.slots <- w.uids[i%len(w.uids)]
	}

	root, err := filepath.Abs(cfg.WorkRoot)
	if err != nil {
		return nil, fmt.Errorf("sandbox: WorkRoot: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, fmt.Errorf("sandbox: WorkRoot: %w", err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("sandbox: WorkRoot %s is not a directory", root)
	}
	if underDir(root, ReadOnlyRoot) {
		return nil, fmt.Errorf("sandbox: WorkRoot %s is under %s, which every sandbox can read", root, ReadOnlyRoot)
	}
	if w.asRoot {
		if err := checkTraversableByOthers(root); err != nil {
			return nil, fmt.Errorf("sandbox: WorkRoot %s: %w", root, err)
		}
	}
	cfg.WorkRoot = root

	if w.rootLinks, err = rootLinkArgs(); err != nil {
		return nil, err
	}
	w.cfg = cfg

	if w.bwrapVersion, err = toolOutput(w.bwrap, "--version"); err != nil {
		return nil, fmt.Errorf("sandbox: %s --version: %w", w.bwrap, err)
	}
	if w.pythonVersion, err = toolOutput(w.python, "--version"); err != nil {
		return nil, fmt.Errorf("sandbox: %s --version: %w", w.python, err)
	}
	if w.description, err = w.describe(); err != nil {
		return nil, err
	}
	w.digest = computeDigest(driverSource, w.description, w.bwrapVersion, w.pythonVersion)
	return w, nil
}

// Kind implements runner.Worker.
func (w *Worker) Kind() string { return "bwrap" }

// Label implements runner.Worker (core principal "runner:sandbox").
func (w *Worker) Label() string { return "sandbox" }

// ImplementationDigest implements runner.Worker: "bwrap-python/v1:sha256:"
// + hex SHA-256 over the embedded driver, the canonical sandbox description
// (arguments and limits), `bwrap --version` and `python --version`.
func (w *Worker) ImplementationDigest() string { return w.digest }

// Description returns the canonical sandbox description covered by the
// digest (JSON).
func (w *Worker) Description() []byte { return append([]byte(nil), w.description...) }

// Python returns the resolved interpreter path executed inside the sandbox.
func (w *Worker) Python() string { return w.python }

// HostUIDs returns the host uids sandboxes run as.
func (w *Worker) HostUIDs() []uint32 { return append([]uint32(nil), w.uids...) }

func computeDigest(driver, description []byte, bwrapVersion, pythonVersion string) string {
	h := sha256.New()
	field := func(name string, b []byte) {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(b))
		h.Write(b)
	}
	field("driver.py", driver)
	field("sandbox", description)
	field("bwrap --version", []byte(bwrapVersion))
	field("python --version", []byte(pythonVersion))
	return DigestPrefix + hex.EncodeToString(h.Sum(nil))
}

// describe renders the canonical, deterministic description of everything
// that determines how a case executes (host-specific run paths replaced by
// placeholders).
func (w *Worker) describe() ([]byte, error) {
	d := struct {
		Launcher       map[string]any `json:"launcher"`
		Prlimit        []string       `json:"prlimit"`
		Bwrap          []string       `json:"bwrap"`
		Interpreter    []string       `json:"interpreter"`
		CaseTimeoutMS  int64          `json:"case_timeout_ms"`
		MaxOutputBytes int            `json:"max_output_bytes"`
		MaxResultBytes int            `json:"max_result_bytes"`
		ResultFD       int            `json:"result_fd"`
	}{
		Launcher: map[string]any{
			"engine_is_root":        w.asRoot,
			"host_uids":             w.uids,
			"host_gid":              w.gid,
			"supplementary_groups":  []int{},
			"setpgid":               true,
			"pdeathsig":             "SIGKILL",
			"env":                   []string{},
			"inherited_fds":         []int{0, 1, 2, 3},
			"kill_on_timeout":       "SIGKILL to process group",
			"cpu_limit_status":      "TIMEOUT",
			"exit_status_by_signal": "ERROR",
		},
		Prlimit:        w.prlimitArgs(),
		Bwrap:          w.bwrapArgs("<RUNDIR>/work", "<RUNDIR>/driver.py"),
		Interpreter:    w.interpreterArgs(),
		CaseTimeoutMS:  w.cfg.CaseTimeout.Milliseconds(),
		MaxOutputBytes: w.cfg.MaxOutputBytes,
		MaxResultBytes: maxResultBytes,
		ResultFD:       3,
	}
	return json.Marshal(d)
}

// prlimitArgs are the prlimit options (without the program). CPU uses a
// soft limit one second below the hard limit so an exhausted CPU budget
// shows up as SIGXCPU (reported as TIMEOUT) rather than a bare SIGKILL.
func (w *Worker) prlimitArgs() []string {
	c := w.cfg
	return []string{
		fmt.Sprintf("--cpu=%d:%d", c.CPUSeconds, c.CPUSeconds+1),
		fmt.Sprintf("--as=%d", c.AddressSpaceBytes),
		fmt.Sprintf("--nproc=%d", c.MaxProcs),
		fmt.Sprintf("--nofile=%d", c.MaxOpenFiles),
		fmt.Sprintf("--fsize=%d", c.MaxFileSizeBytes),
		"--core=0",
	}
}

// bwrapArgs are the bwrap options (without the program). Every flag was
// checked against bubblewrap 0.9.0 (see README.md). Order matters: the
// read-only remounts of / and /dev come after every mount point exists.
func (w *Worker) bwrapArgs(workDir, driverFile string) []string {
	a := []string{
		"--unshare-all", // user(try), ipc, pid, net, uts, cgroup(try)
		"--unshare-user",
		"--unshare-cgroup",
		"--disable-userns", "--assert-userns-disabled",
		"--die-with-parent",
		"--new-session",
		"--cap-drop", "ALL",
		"--uid", strconv.Itoa(SandboxUID), "--gid", strconv.Itoa(SandboxGID),
		"--hostname", "sandbox",
		"--ro-bind", ReadOnlyRoot, ReadOnlyRoot,
	}
	a = append(a, w.rootLinks...)
	a = append(a,
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--proc", "/proc",
		"--dev", "/dev",
		"--size", strconv.FormatInt(w.cfg.TmpfsBytes, 10), "--tmpfs", "/tmp",
		"--ro-bind", workDir, sandboxWork,
		"--ro-bind", driverFile, sandboxDriver,
		"--remount-ro", "/dev",
		"--remount-ro", "/",
		"--chdir", sandboxWork,
		"--clearenv",
	)
	for _, kv := range sandboxEnv {
		a = append(a, "--setenv", kv[0], kv[1])
	}
	return a
}

// sandboxEnv is the complete environment of the interpreter. Python is not
// run with -I/-E because those make it ignore PYTHONHASHSEED; the
// environment is fully controlled here instead (--clearenv).
var sandboxEnv = [][2]string{
	{"PATH", "/usr/local/bin:/usr/bin:/bin"},
	{"HOME", "/tmp"},
	{"PYTHONDONTWRITEBYTECODE", "1"},
	{"PYTHONHASHSEED", "0"},
	{"PYTHONSAFEPATH", "1"},
	{"PYTHONNOUSERSITE", "1"},
	{"PYTHONUTF8", "1"},
}

// sandboxEnvAllowed are the only variable names that may appear inside
// (sandboxEnv plus PWD set by bwrap and LC_CTYPE set by Python's locale
// coercion).
func sandboxEnvAllowed() map[string]bool {
	m := map[string]bool{"PWD": true, "LC_CTYPE": true}
	for _, kv := range sandboxEnv {
		m[kv[0]] = true
	}
	return m
}

func (w *Worker) interpreterArgs() []string {
	return []string{w.python, "-s", "-B", sandboxDriver}
}

// commandArgs is the full argv after prlimit.
func (w *Worker) commandArgs(workDir, driverFile string) []string {
	a := w.prlimitArgs()
	a = append(a, "--", w.bwrap)
	a = append(a, w.bwrapArgs(workDir, driverFile)...)
	a = append(a, "--")
	a = append(a, w.interpreterArgs()...)
	return a
}

// ---- host checks -------------------------------------------------------

func underDir(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// resolveTool resolves name (or def) to an absolute, symlink-free path under
// /usr and checks it is a plain executable nobody but root can replace.
func resolveTool(name, def string, asRoot bool) (string, error) {
	p := name
	if p == "" {
		p = def
	}
	if !filepath.IsAbs(p) {
		lp, err := exec.LookPath(p)
		if err != nil {
			return "", fmt.Errorf("sandbox: %s not found: %w", p, err)
		}
		p = lp
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("sandbox: %s: %w", p, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("sandbox: %s: %w", p, err)
	}
	if !underDir(real, ReadOnlyRoot) {
		return "", fmt.Errorf("sandbox: %s resolves to %s, outside %s (the only host tree the sandbox binds); refusing", p, real, ReadOnlyRoot)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("sandbox: %s: %w", real, err)
	}
	mode := fi.Mode()
	switch {
	case !mode.IsRegular():
		return "", fmt.Errorf("sandbox: %s is not a regular file", real)
	case mode.Perm()&0o111 == 0:
		return "", fmt.Errorf("sandbox: %s is not executable", real)
	case mode.Perm()&0o022 != 0:
		return "", fmt.Errorf("sandbox: %s is group- or world-writable; refusing", real)
	case mode&(os.ModeSetuid|os.ModeSetgid) != 0:
		return "", fmt.Errorf("sandbox: %s is setuid/setgid; only the verified unprivileged mode is supported", real)
	}
	if asRoot {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
			return "", fmt.Errorf("sandbox: %s is owned by uid %d, not root; refusing", real, st.Uid)
		}
	}
	return real, nil
}

// checkTraversableByOthers requires the "x" bit for others on every
// directory from / to dir: bwrap resolves bind sources as the sandbox uid.
func checkTraversableByOthers(dir string) error {
	p := dir
	for {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if fi.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("%s is not traversable by the unprivileged sandbox uid (mode %v)", p, fi.Mode().Perm())
		}
		if p == "/" {
			return nil
		}
		p = filepath.Dir(p)
	}
}

// rootLinkArgs reproduces the top-level /bin, /sbin, /lib* entries inside
// the sandbox: on merged-usr hosts they are symlinks into /usr and are
// recreated as symlinks; on split hosts the directories are bound read-only.
func rootLinkArgs() ([]string, error) {
	var a []string
	for _, name := range []string{"bin", "sbin", "lib", "lib32", "lib64", "libx32"} {
		p := "/" + name
		fi, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("sandbox: %s: %w", p, err)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return nil, fmt.Errorf("sandbox: %s: %w", p, err)
			}
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join("/", resolved)
			}
			resolved = filepath.Clean(resolved)
			if !underDir(resolved, ReadOnlyRoot) {
				continue // points outside /usr: not reproduced
			}
			a = append(a, "--symlink", target, p)
		case fi.IsDir():
			a = append(a, "--ro-bind", p, p)
		}
	}
	return a, nil
}

// toolOutput runs a version query with an empty environment.
func toolOutput(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
