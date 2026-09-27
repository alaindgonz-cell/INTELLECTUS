package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// caseSpec is one sandboxed interpreter run.
type caseSpec struct {
	workDir    string // host path bound read-only at /work
	driverFile string // host path bound read-only at /harness/driver.py
	request    []byte // stdin
	timeout    time.Duration
	// observe, if set, runs concurrently once the process has started; it
	// must return when done is closed.
	observe func(pid int, done <-chan struct{})
}

// caseRun is what the host observed about one run.
type caseRun struct {
	startErr   error
	result     []byte // fd 3 bytes, at most maxResultBytes
	resultOver bool   // fd 3 carried more than maxResultBytes
	resultErr  error
	stdout     capBuf
	stderr     capBuf
	exitCode   int // bwrap exit status (128+N when the payload died of signal N)
	signaled   bool
	signal     syscall.Signal // signal that killed the launcher itself
	timedOut   bool           // wall timeout fired and the group was killed
	aborted    error          // parent context ended the run
	cpu        time.Duration  // user+sys of the whole reaped tree
	duration   time.Duration
	hostUID    uint32
	nproc      int // RLIMIT_NPROC the sandbox was launched with
}

// capBuf keeps the first limit bytes written and counts the rest.
type capBuf struct {
	limit int
	buf   []byte
	total int64
}

func (b *capBuf) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	if room := b.limit - len(b.buf); room > 0 {
		q := p
		if len(q) > room {
			q = q[:room]
		}
		b.buf = append(b.buf, q...)
	}
	return len(p), nil
}

// String returns the kept bytes, marking truncation.
func (b *capBuf) String() string {
	s := string(b.buf)
	if b.total > int64(len(b.buf)) {
		s += fmt.Sprintf("\n[truncated: %d bytes total, %d kept]", b.total, len(b.buf))
	}
	return s
}

// acquire takes a sandbox slot (and its host uid).
func (w *Worker) acquire(ctx context.Context) (uint32, error) {
	select {
	case uid := <-w.slots:
		return uid, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (w *Worker) release(uid uint32) { w.slots <- uid }

// runCase launches one sandboxed interpreter and waits for it.
func (w *Worker) runCase(ctx context.Context, spec caseSpec) caseRun {
	var r caseRun
	r.stdout.limit = w.cfg.MaxOutputBytes
	r.stderr.limit = w.cfg.MaxOutputBytes
	uid, err := w.acquire(ctx)
	if err != nil {
		r.aborted = err
		return r
	}
	defer w.release(uid)
	r.hostUID = uid

	caseCtx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()

	resR, resW, err := os.Pipe()
	if err != nil {
		r.startErr = fmt.Errorf("result pipe: %w", err)
		return r
	}
	defer resR.Close()

	nproc, err := w.nprocLimit()
	if err != nil {
		resW.Close()
		r.startErr = err
		return r
	}
	r.nproc = nproc
	cmd := exec.CommandContext(caseCtx, w.prlimit, w.commandArgs(spec.workDir, spec.driverFile, strconv.Itoa(nproc))...)
	cmd.Env = []string{} // non-nil: nothing inherited (bwrap's own env is readable via /proc/1/environ inside)
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(spec.request)
	cmd.Stdout = &r.stdout
	cmd.Stderr = &r.stderr
	cmd.ExtraFiles = []*os.File{resW} // fd 3 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	if w.asRoot {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: w.gid, Groups: []uint32{}}
	}
	var killed atomic.Bool
	cmd.Cancel = func() error {
		killed.Store(true)
		if cmd.Process == nil {
			return nil
		}
		return killSandbox(cmd.Process.Pid)
	}
	// If the monitor has not exited WaitDelay after Cancel, os/exec kills
	// it; --die-with-parent then takes the sandbox down with it.
	cmd.WaitDelay = waitDelay

	// Pdeathsig is tied to the forking OS thread: keep it alive until Wait.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	markInheritedFDsCloseOnExec()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		resW.Close()
		r.startErr = err
		return r
	}
	resW.Close()

	type readOut struct {
		data []byte
		over bool
		err  error
	}
	resCh := make(chan readOut, 1)
	go func() {
		data, over, err := readCapped(resR, maxResultBytes)
		resCh <- readOut{data, over, err}
	}()
	done := make(chan struct{})
	obsDone := make(chan struct{})
	if spec.observe != nil {
		go func() {
			defer close(obsDone)
			spec.observe(cmd.Process.Pid, done)
		}()
	} else {
		close(obsDone)
	}

	waitErr := cmd.Wait()
	r.duration = time.Since(start)
	close(done)
	<-obsDone
	// Every writer of fd 3 is gone once the pid namespace is torn down; the
	// deadline only guards against the unexpected.
	_ = resR.SetReadDeadline(time.Now().Add(waitDelay))
	ro := <-resCh
	r.result, r.resultOver, r.resultErr = ro.data, ro.over, ro.err

	if ps := cmd.ProcessState; ps != nil {
		r.cpu = ps.UserTime() + ps.SystemTime()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
			switch {
			case ws.Exited():
				r.exitCode = ws.ExitStatus()
			case ws.Signaled():
				r.signaled = true
				r.signal = ws.Signal()
				r.exitCode = -1
			}
		}
	}
	if killed.Load() {
		if ctx.Err() != nil {
			r.aborted = ctx.Err()
		} else {
			r.timedOut = true
		}
	} else if waitErr != nil && cmd.ProcessState == nil {
		r.startErr = waitErr
	}
	return r
}

// killSandbox kills a running sandbox whose launcher (prlimit, exec'd into
// the bwrap monitor) has host pid pid. The monitor's only child is the
// interpreter, which is the sandbox's pid-namespace init (--as-pid-1; it
// runs in its own session, --new-session). Killing it first makes the
// kernel kill every other process in the namespace before the interpreter
// is reaped by the monitor, which then exits: when Wait returns, nothing of
// the sandbox is left. Killing the monitor first would orphan the
// interpreter (--die-with-parent would still kill it, but asynchronously,
// leaving the reaping to the host's pid 1). With no child yet (still in
// bwrap setup) the monitor's process group is killed instead.
func killSandbox(pid int) error {
	kids := childrenOf(pid)
	for _, k := range kids {
		_ = syscall.Kill(k, syscall.SIGKILL)
	}
	if len(kids) == 0 {
		return syscall.Kill(-pid, syscall.SIGKILL)
	}
	return nil
}

// childrenOf lists the host pids whose parent is pid.
func childrenOf(pid int) []int {
	var out []int
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)); err == nil {
		for _, f := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(f); err == nil {
				out = append(out, c)
			}
		}
		return out
	}
	for c, ppid := range processTable() {
		if ppid == pid {
			out = append(out, c)
		}
	}
	return out
}

// readCapped reads until EOF, keeping at most limit bytes and draining the
// rest so the writer never blocks.
func readCapped(rd io.Reader, limit int) ([]byte, bool, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	over := false
	for {
		n, err := rd.Read(chunk)
		if n > 0 {
			if room := limit - buf.Len(); room > 0 {
				if n <= room {
					buf.Write(chunk[:n])
				} else {
					buf.Write(chunk[:room])
					over = true
				}
			} else {
				over = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return buf.Bytes(), over, err
		}
	}
}

// markInheritedFDsCloseOnExec sets FD_CLOEXEC on every descriptor >= 3 of
// this process. bwrap does not close inherited descriptors, so a descriptor
// the engine inherited without close-on-exec (from its own launcher, or
// opened by non-Go code) would otherwise reach the candidate; only the
// explicitly passed fds 0-3 must. Go itself always opens with O_CLOEXEC and
// passes descriptors to children only via dup2, so this changes nothing for
// the rest of the engine.
func markInheritedFDsCloseOnExec() {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < 3 {
			continue
		}
		syscall.CloseOnExec(fd)
	}
}

// ---- host-side process observation ---------------------------------------

type procInfo struct {
	pid   int
	ppid  int
	uids  []uint32 // real, effective, saved, fs (host view)
	argv0 string
}

// processTable reads pid -> ppid for every process visible on the host.
func processTable() map[int]int {
	out := map[int]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		out[pid] = ppid
	}
	return out
}

// descendants returns root and all its descendants with host-view uids.
func descendants(root int) []procInfo {
	table := processTable()
	children := map[int][]int{}
	for pid, ppid := range table {
		children[ppid] = append(children[ppid], pid)
	}
	var out []procInfo
	queue := []int{root}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if _, ok := table[pid]; !ok {
			continue
		}
		info := procInfo{pid: pid, ppid: table[pid]}
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
					for _, f := range strings.Fields(rest) {
						if u, err := strconv.ParseUint(f, 10, 32); err == nil {
							info.uids = append(info.uids, uint32(u))
						}
					}
				}
			}
		}
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			info.argv0, _, _ = strings.Cut(string(b), "\x00")
		}
		out = append(out, info)
		queue = append(queue, children[pid]...)
	}
	return out
}

func processExists(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}
