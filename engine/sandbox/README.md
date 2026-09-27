# engine/sandbox — isolated test runner (milestone M4)

`sandbox.Worker` is the real `runner.Worker`. It runs each case of a
protected acceptance manifest against the candidate's Python module in a
fresh bubblewrap sandbox. It replaces the fail-closed `runner.SandboxWorker`
placeholder. It is Linux-only and needs `bwrap` and util-linux `prlimit`.

| | |
|---|---|
| `Kind()` | `bwrap` (the environment's `worker_kind` must match) |
| `Label()` | `sandbox` (core principal `runner:sandbox`) |
| `ImplementationDigest()` | `bwrap-python/v1:sha256:<hex>`: SHA-256 over the embedded `driver.py`, the canonical sandbox description (every prlimit and bwrap argument, interpreter argv, host uids and gid, timeouts, output bounds; see `Worker.Description()`), `bwrap --version` and `python --version`. It is computed once, in `New`. |

## Using it

```go
w, err := sandbox.New(sandbox.Config{})          // validates tools, computes digest
res, err := w.RequireSelfTest(ctx, sandbox.SelfTestConfig{
    ForbiddenPaths: []string{coreDBPath, harnessWorkdir, canaryFile},
    CanaryEnv:      []string{"SOME_CANARY_SET_IN_THIS_PROCESS"},
    BlockedAddrs:   []string{proxyHostPort, engineLoopbackListener},
})                                               // refuse the worker unless err == nil
runner := &runner.ProtectedRunner{Core: c, Session: s, Worker: w}
```

`New` fails closed in these cases:
- `bwrap`, `prlimit` or the interpreter does not resolve (absolute path, symlinks evaluated) to a regular, executable, non-setuid file under `/usr` that neither group nor others can write (root-owned when the engine is root).
- `WorkRoot` is under `/usr`.
- The engine is root and some directory on the path to `WorkRoot` is not traversable by others. bwrap opens bind sources as the unprivileged uid.
- A limit is out of range.

## How one case runs

The engine is root on the verified host. It runs the following (flags as printed by the tests; `<RUNDIR>` is a fresh `MkdirTemp` under `WorkRoot`):

```
setuid/setgid 65534, setgroups([]), setpgid, PDEATHSIG=SIGKILL, env = {}, cwd = /,
fds: 0 = request JSON, 1/2 = capture pipes, 3 = result pipe, nothing else
/usr/bin/prlimit --cpu=10 --as=1073741824 --nproc=64 --nofile=256 --fsize=16777216 --core=0 --
 /usr/bin/bwrap --unshare-all --unshare-user --unshare-cgroup --disable-userns --assert-userns-disabled
  --as-pid-1 --die-with-parent --new-session --cap-drop ALL --uid 65534 --gid 65534 --hostname sandbox
  --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/sbin /sbin --symlink usr/lib /lib
  --symlink usr/lib64 /lib64 --ro-bind-try /etc/ld.so.cache /etc/ld.so.cache
  --proc /proc --dev /dev --size 67108864 --tmpfs /tmp
  --ro-bind <RUNDIR>/work /work --ro-bind <RUNDIR>/driver.py /harness/driver.py
  --remount-ro /dev --remount-ro / --chdir /work --clearenv
  --setenv PATH /usr/local/bin:/usr/bin:/bin --setenv HOME /tmp --setenv PYTHONDONTWRITEBYTECODE 1
  --setenv PYTHONHASHSEED 0 --setenv PYTHONSAFEPATH 1 --setenv PYTHONNOUSERSITE 1 --setenv PYTHONUTF8 1
  -- /usr/bin/python3.11 -s -B /harness/driver.py
```

- **Candidate tree.** Written to `<RUNDIR>/work` with 0755 directories and 0644 regular files, using `O_EXCL|O_NOFOLLOW`, and removed after the run. Paths must be relative with no `.`, `..` or empty segment, no backslash, NUL or control character, and no file/directory conflicts. The tree is limited to 10 000 files and 64 MiB. Any violation refuses the whole check (an ERROR report with no case run).
- **Driver.** The embedded `driver.py` reads `{"path","function","input"}` from stdin. It imports `/work/<path>` with `importlib`, with the module's directory and `/work` on `sys.path` (`/harness` never is). It calls `function(input)` and writes exactly one JSON line to fd 3, then `os._exit(0)`. The line has one of these shapes:
  - `{"returns": v}`
  - `{"raises": "C", "mro": [...], "message": "..."}` (message truncated to 1000 characters)
  - `{"error": "import failed: …" | "entrypoint not found: …" | "entrypoint not callable: …" | "result not JSON-serializable: <type>" | "driver: …"}`
  
  A result is rejected as not JSON-serializable if it contains any of: sets, other non-JSON types, NaN or Inf, or dict keys that are not strings. `json.dumps` would silently turn `{1: …}` into `{"1": …}`.
- **Cases** run concurrently. At most `MaxParallel` sandboxes run at once across all `Run` and `SelfTest` calls on a Worker. Each case gets a fresh interpreter.

### Verdict rules

| Observation | Case status |
|---|---|
| Exactly one newline-terminated, well-formed line, interpreter exit status 0, `{"returns": v}` JSON-equal to expected `{"returns": e}` | PASS |
| `{"raises": C, mro}` with expected `{"raises": N}` and `N ∈ mro` | PASS |
| Wrong value, wrong exception, returned when a raise was expected (or vice versa), import failure, missing or uncallable entrypoint, non-serializable result | FAIL |
| Wall timeout (`CaseTimeout`): the sandbox is killed | TIMEOUT |
| Killed by SIGKILL with the CPU budget used up (RLIMIT_CPU) | TIMEOUT |
| 0 or ≥2 result lines, partial or malformed line, result over 1 MiB, non-zero exit, death by signal, sandbox setup failure, `driver:` error | ERROR |
| Expectation that is not exactly `{"returns": <json>}` or `{"raises": "<non-empty name>"}` | ERROR (not executed) |

**JSON equality.** Values are decoded with `UseNumber`. Numbers are compared by exact decimal value through a canonical (sign, digits, exponent) form, so `1 == 1.0 == 1e0`, `0.1 != 0.10000000000000001`, and huge exponents are compared in linear time with no big-number expansion. `true != 1`, `null != 0`. Object key order does not matter; array order does.

**Report.** Result is PASS if every case passed. Otherwise it is FAIL if any case failed, else TIMEOUT if any timed out, else ERROR. `Completed` is true when every manifest case got a status. `Collected` counts executed cases. `Cases` and `Details` have one entry per manifest case, in manifest order. `Details` hold input, expected, observed JSON, stdout and stderr (each at most `MaxOutputBytes` plus a truncation marker) and the duration.

**Summary.** For example: `5/11 cases passed in bwrap sandbox (uid 65534, no network, no host files); failing: [zero over plus space arabic_indic none]`.

**Refusals and cancellation.** A missing entrypoint, a language other than `python`, an entrypoint not in the tree, a function that is not an identifier, an empty manifest, duplicate case names or a bad tree all give `Result: ERROR` with `Completed: false`, empty `Cases`, and a summary ending "(no candidate code was executed)". Only context cancellation makes `Run` return an error.

## Flags: what was verified on bubblewrap 0.9.0

Every flag was checked against `bwrap --help` and exercised empirically as uid 65534 with no capabilities (setuid-root bwrap is refused).

- **`--unshare-all`** means user(try), ipc, pid, net, uts and cgroup(try).
  - I added **`--unshare-user`** explicitly because `--disable-userns` requires it ("bwrap: --disable-userns requires --unshare-user" without it).
  - I added **`--unshare-cgroup`** so a missing cgroup namespace fails closed instead of being skipped.
  - The self-test compares all 7 `/proc/self/ns/*` inodes inside against the host's: all differ.
- **`--disable-userns --assert-userns-disabled`**: nested `unshare(CLONE_NEWUSER)` fails with ENOSPC and `unshare(CLONE_NEWNET)` with EPERM. This reduces kernel attack surface. As a side effect, bwrap stacks two user namespaces, so `/proc/self/uid_map` inside reads `65534 0 1` (relative to the intermediate namespace). The host uid is therefore verified from the host side, see `runs_as_unprivileged`.
- **`--as-pid-1`**: the interpreter is the namespace init. Without it, `strace` shows the bwrap monitor reading the payload's exit status from its init over an eventfd and calling `exit_group` without reaping the init. Every case then left a zombie (uid 65534) for the host's pid 1, counted against RLIMIT_NPROC until reaped, and timeout teardown was asynchronous. The first `-count=3` run failed on this.
  - With `--as-pid-1`, the kernel kills the namespace when the interpreter exits and the monitor `wait4`s it before exiting. A monitor sampling `/proc` every 20 ms during the whole suite saw **zero** sandbox processes orphaned to the host init.
  - Side effects:
    - The kernel ignores handler-less signals that the interpreter sends itself: `os.kill(os.getpid(), SIGKILL)` returns (tested).
    - SIGXCPU is not delivered, so RLIMIT_CPU is set with soft = hard and its SIGKILL is classified by rusage.
    - bwrap's own init, whose `/proc/1/environ` exposed the monitor's environment and whose eventfd was writable from inside, no longer exists in the sandbox.
- **`--die-with-parent`** kills the payload if the monitor dies.
- **`--new-session`** calls setsid, which blocks TIOCSTI injection into a terminal.
- **`--cap-drop ALL`** is accepted unprivileged. Inside, CapInh, CapPrm, CapEff, CapBnd and CapAmb are all 0 and NoNewPrivs is 1.
- **`--uid/--gid 65534`, `--hostname sandbox`** were verified inside.
- **`--ro-bind /usr /usr`, `--symlink usr/X /X`.** Merged-usr is detected from `/bin`, `/sbin` and `/lib*` symlinks. On a split-usr host those directories would be read-only bind mounts instead, which is not exercised here. `/etc` contains only `ld.so.cache`.
- **`--proc /proc --dev /dev`**: a fresh procfs for the pid namespace, and a minimal /dev (null, zero, full, random, urandom, tty, pts, shm).
- **`--size N --tmpfs /tmp`**: writing into /tmp stops at exactly 64 MiB with ENOSPC. One file stops at 16 MiB with EFBIG from RLIMIT_FSIZE; Python ignores SIGXFSZ.
- **`--remount-ro /dev --remount-ro /`**: without these, the sandbox root and `/dev` are tmpfs owned by the sandbox uid, writable and unbounded (`wrote /x`, `wrote /dev/x`). With them, writes to `/`, `/dev`, `/dev/shm`, `/etc`, `/usr` and `/work` fail with EROFS. `/dev/null` stays writable because it is a separate bind mount. The remounts come last so every mount point already exists.
- **`--clearenv` plus explicit `--setenv`.** Python runs with `-s -B`, **not `-I`**. `-I` implies `-E`, which ignores `PYTHONHASHSEED`: `hash("abc")` differed run to run under `-I`, and was stable under `-s -B` with `PYTHONHASHSEED=0`. The environment is fully controlled here, so `-E` adds nothing. `PYTHONSAFEPATH=1` and the driver keep `/harness` off `sys.path`.

### Result channel on fd 3

`exec.Cmd.ExtraFiles = [pipe write end]` puts it at fd 3. prlimit execs bwrap without closing it, and bwrap does not close inherited descriptors, so it reaches the interpreter. This was verified with a shell `3>>file` test: `os.write(3, …)` inside landed in the host file and `/proc/self/fd` listed `0 1 2 3`. In Go it is exercised by every test.

Because bwrap passes along **every** inherited descriptor, the launcher sets `FD_CLOEXEC` on all of the engine's descriptors ≥ 3 before each start (`markInheritedFDsCloseOnExec`). This was verified by opening `/dev/null` without `O_CLOEXEC` in the test process: with the hygiene step disabled, the `no_inherited_fds` probe fails with "fd 6 open inside"; with it, only `[0 1 2 3]` are open.

The Go side reads fd 3 to EOF. It keeps at most 1 MiB and drains the rest, so the writer never blocks.

## Threat model

The candidate module is untrusted and adversarial. It runs with the sandbox's full authority, at import time and at call time. The engine, the host tools under `/usr` and the kernel are trusted.

### Enforced

- **Namespaces**: user (nested user namespaces disabled), pid, net, ipc, uts, mount and cgroup are all new.
- **No network.** Only `lo` exists, in its own net namespace.
  - Connects to the host's loopback listeners and proxy (127.0.0.1:37979 here) are refused.
  - 1.1.1.1:443 and UDP give ENETUNREACH.
  - DNS fails.
  - Abstract unix sockets are per net namespace.
- **No host files.** The sandbox sees only:
  - read-only `/usr` and `/etc/ld.so.cache`
  - a read-only copy of the candidate tree at `/work` and the driver at `/harness/driver.py`
  - a fresh `/proc` and minimal `/dev`
  - a size-capped tmpfs `/tmp`
  
  The root and `/dev` are remounted read-only. The run directory is removed after each run.
- **Cleared environment.** The interpreter's environment is exactly `PATH HOME PWD PYTHON{DONTWRITEBYTECODE,HASHSEED,SAFEPATH,NOUSERSITE,UTF8} LC_CTYPE`. prlimit and bwrap themselves start with an empty environment, so nothing is readable via `/proc/*/environ` either. The unscrubbed alternative was tested: `env_scrubbed` then fails listing `AWS_SECRET_ACCESS_KEY`, `GH_TOKEN` and others.
- **Unprivileged host uid.**
  - As root, the engine starts the launcher as uid/gid 65534 with no supplementary groups, or as a uid from the `ExtraUIDs` pool.
  - Inside, the uid is 65534 and no capabilities are held.
  - `/usr` is mounted nosuid and NoNewPrivs is 1.
- **Descriptors**: only 0-3 reach the sandbox.
- **Rlimits per process**: CPU 10 s, address space 1 GiB, 64 processes/threads per host uid, 256 fds, 16 MiB files, no core dumps. /tmp is capped at 64 MiB.
- **Wall timeout** per case (10 s). On expiry the engine SIGKILLs the interpreter (the namespace init). The kernel tears the namespace down and the monitor reaps it, so nothing of the sandbox survives `Run`, which the tests verify with no grace period. `PDEATHSIG` and `--die-with-parent` cover an engine crash.
- **Result integrity against accidents and naive forgery.** Output on stdout/stderr is never parsed. Two or more lines, partial or malformed lines, oversized results and non-zero exits are ERROR, never PASS.

### NOT enforced (residual risk)

- **No seccomp filter.** Every syscall the kernel allows an unprivileged process is available (io_uring, keyctl, perf_event_open and bpf subject to their sysctls, ptrace within the sandbox, and so on). Kernel bugs reachable from an unprivileged user namespace are the main residual risk. A kernel exploit is a host compromise.
- **The candidate controls what it reports about its own behaviour.** It shares the interpreter with the driver.
  - It can write a single forged result line to fd 3 and `os._exit(0)` before the driver runs. The engine cannot distinguish that from a genuine result: `TestKnownLimitationSingleForgedLine` asserts that it PASSes.
  - It can special-case test inputs, detect that it is under test, or define its own class named `ValueError`, since `raises` matches class *names* in the MRO.
  - Tests are evidence of observable behaviour, not proof of correctness.
- **Host information is visible read-only**:
  - everything world-readable under `/usr`, including `/usr/local` and installed Python packages
  - kernel-wide `/proc` data (kernel version, CPU and memory information, sysctls)
  - bind source paths in `/proc/self/mountinfo` (host path names, not contents)
- **Resources are per process, not per sandbox.** There is no cgroup, so:
  - worst-case memory is MaxProcs × RLIMIT_AS (64 × 1 GiB) plus 64 MiB tmpfs per sandbox
  - CPU is bounded only by processes × wall timeout
- **RLIMIT_NPROC is accounted by the kernel per *host* uid across all namespaces.** This was verified: two sandboxes as the same host uid share one budget, and a second sandbox failed with `bwrap: Can't fork for pid 1: Resource temporarily unavailable` while the first held 15 of 20. With distinct host uids both succeeded.
  - A persistent fork bomb can therefore make concurrently running cases of other candidates ERROR. That is a denial of service, never a false PASS. The same applies to any other host process running as that uid.
  - Mitigation: give `ExtraUIDs` so there are ≥ `MaxParallel` distinct, otherwise unused uids.
- **Not root.** When the engine is not root, sandboxes run as the engine's own uid. RLIMIT_NPROC then counts all of that user's processes and will usually make every case ERROR. `SelfTest` fails closed. Run the engine as root, so it drops to a dedicated uid, or as a dedicated service user.
- **Run directories on the host** (0755/0644 under `WorkRoot`) are readable by other local users while a run is in progress.
- **Platform.** Linux only. Requires bwrap ≥ 0.8 (`--disable-userns`; only 0.9.0 was verified), util-linux `prlimit`, and unprivileged user namespaces. AppArmor userns restrictions, as on Ubuntu 24.04, or `max_user_namespaces=0` make every sandbox fail, so `SelfTest` fails closed. Split-usr layouts are handled by bind mounts but were not exercised.

## What the tests verified on this host

Verified 2026-09-27 on kernel `6.18.44-fc-v37` (Firecracker microVM, pid 1 `process_api`), running as root, with bubblewrap 0.9.0, util-linux 2.39.3 prlimit, Python 3.11.15 (`/usr/local/bin/python3` resolved to `/usr/bin/python3.11`) and Go 1.24.7. `go test -race ./sandbox/` is green, including three consecutive runs with `-count=3`. A case takes about 30-60 ms, mostly bwrap setup and interpreter start: the 11 page-size cases took about 120 ms wall at MaxParallel 4. The whole suite takes about 20 s, mostly the deliberate timeouts and three self-tests.

**Page-size example.**
- A1 passes 11/11.
- A0 (`int(raw)`) fails exactly `zero over plus space arabic_indic none`:
  - `int("0")` returns 0, `int("101")` 101, `int("+1")` 1, `int(" 5")` 5 and `int("١")` 1.
  - `int(None)` raises TypeError, which is not a ValueError.
  - `""` and `"1.0"` raise ValueError; `"1"`, `"100"` and `"001"` return 1, 100 and 1.

**T19, call time.** Each attack below failed inside; the module's own report was `succeeded:false`:
- Reading a world-readable host canary (also via `/proc/1/root`), listing its directory, reading `/etc/shadow`: FileNotFoundError, so hidden and not merely denied.
- Finding a secret env var set in the test process in `os.environ`, `/proc/*/environ` or `/proc/*/cmdline`.
- Connecting to a host 127.0.0.1 listener (ECONNREFUSED) and to 1.1.1.1:443 (ENETUNREACH).
- Writing `/work/*` or the driver: EROFS/EACCES.
- A fork bomb: 61-62 children, then EAGAIN.
- A 4 GiB allocation: MemoryError.

The host checks all held: the canary was unchanged (content and mtime), the listener accepted 0 connections, no sandbox process survived `Run`, and the run directory was removed.

Behaviour cases:
- `while True: pass` became TIMEOUT at 3.0 s.
- 1 MiB on each of stdout and stderr was truncated to 8192 bytes plus a marker.
- A daemonized grandchild sleeping 600 s did not survive.
- Stdout JSON that looked like a PASS, plus two extra lines on fd 3, gave ERROR ("3 result lines").
- A partial line on fd 3 gave ERROR.

**T19, import time.** Canary, shadow, env, listener, internet and `/work` writes all failed. A forged fd 3 line at import gave ERROR on every case. A hang at import gave TIMEOUT.

**Other runs.**
- CPU limit: with 1 s of CPU, a spin loop became TIMEOUT at about 1.0 s.
- Semantics:
  - tuple equals list, `2.0` equals `2`, `True` does not equal `1`
  - set, NaN and int-keyed dicts fail as not serializable
  - `KeyError` matches `LookupError`
  - `SystemExit` is not a `ValueError`
  - a segfault is ERROR, as is exit without a result
  - an unsupported expectation is ERROR and not executed
- Import failure and a missing entrypoint are FAIL.
- Refusals: path traversal (`../`, `a/../../`, absolute, backslash, `//`, `./`, file/dir conflict), a missing entrypoint, another language, an absent module, an invalid function name, no cases and duplicate names are all refused, with nothing executed.
- An interpreter outside `/usr` (also via symlink), a WorkRoot under `/usr` or not traversable, and uid 0 are refused by `New`.

**SelfTest.** All 13 probes passed:

| Probe | Observed |
|---|---|
| `runs_as_unprivileged` | uid and euid 65534 inside, no groups. Host view of the process tree: `bwrap=[65534×4] python3.11=[65534×4]`. |
| `no_capabilities` | All five capability sets 0, NoNewPrivs=1, Seccomp=0. |
| `namespaces_unshared` | All 7 namespace inodes differ from the host's. |
| `host_files_hidden` | 16 paths, all ENOENT: the canary and its directory, the repository, `/etc/shadow`, `/etc/passwd`, `/root`, `/home`, `/var`, `/run`, `/opt`, `/srv`, `/mnt`, the run and work directories, the test binary and its cwd. |
| `env_scrubbed` | Only the allowlisted names. No canary or secret found by name or by value hash. |
| `network_egress_blocked` | 1.1.1.1:443 ENETUNREACH. The host listener and the host proxy 127.0.0.1:37979 were ECONNREFUSED inside, although both are reachable from the host. Only `lo`. DNS fails. UDP ENETUNREACH. |
| `workspace_read_only` | `/work`, `/`, `/usr`, `/dev`, `/etc` and `/harness` writes fail (EROFS/EACCES). |
| `tmp_size_bounded` | ENOSPC at 64 MiB; EFBIG at 16 MiB for one file. |
| `process_limit_enforced` | 62 children, then EAGAIN. |
| `memory_limit_enforced` | 2 GiB gives MemoryError. |
| `timeout_enforced` | Killed at 2.003 s for a 2 s limit. Both host processes of the sandbox were gone when `Run` returned. |
| `nested_userns_blocked` | ENOSPC for a user namespace, EPERM for a net namespace. |
| `no_inherited_fds` | `[0 1 2 3]`, although the test process holds a non-CLOEXEC fd. |

A vacuous self-test configuration fails closed: a forbidden path that does not exist on the host, or a canary variable that is not set.
