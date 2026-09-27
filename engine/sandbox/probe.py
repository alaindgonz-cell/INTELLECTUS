# INTELLECTUS sandbox self-test probes (bwrap-python/v1).
#
# Loaded by the driver exactly like a candidate module (as /work/probe.py)
# and run in the exact sandbox configuration candidates get. Each probe
# ATTEMPTS an escape or a resource abuse and reports what it observed; the Go
# side decides whether the attempt failed. Nothing here is trusted as a
# verdict on its own: the Go side also checks host-side facts (process
# credentials, namespaces, timeouts, leftover processes).
import errno
import hashlib
import os
import socket
import time

CLONE_NEWUSER = 0x10000000
CLONE_NEWNET = 0x40000000


def _err(exc):
    name = type(exc).__name__
    code = getattr(exc, "errno", None)
    if isinstance(code, int) and code in errno.errorcode:
        name += "/" + errno.errorcode[code]
    return name


def _read(path, limit=65536):
    try:
        with open(path, "rb") as f:
            return f.read(limit).decode("utf-8", "replace")
    except BaseException as exc:
        return "<" + _err(exc) + ">"


def identity(req):
    hold = req.get("hold_ms", 0)
    if hold:
        time.sleep(hold / 1000.0)
    return {
        "uid": os.getuid(),
        "euid": os.geteuid(),
        "gid": os.getgid(),
        "egid": os.getegid(),
        "groups": os.getgroups(),
        "uid_map": _read("/proc/self/uid_map"),
        "gid_map": _read("/proc/self/gid_map"),
        "hostname": os.uname().nodename,
    }


def capabilities(req):
    out = {}
    for line in _read("/proc/self/status").splitlines():
        key, _, value = line.partition(":")
        if key.startswith("Cap") or key in ("NoNewPrivs", "Seccomp"):
            out[key] = value.strip()
    return out


def namespaces(req):
    out = {}
    for ns in ("user", "pid", "net", "ipc", "uts", "mnt", "cgroup"):
        try:
            out[ns] = os.readlink("/proc/self/ns/" + ns)
        except BaseException as exc:
            out[ns] = "<" + _err(exc) + ">"
    return out


def files(req):
    results = []
    for path in req.get("paths", []):
        entry = {"path": path, "exists": os.path.lexists(path), "readable": False, "error": None}
        try:
            if os.path.isdir(path):
                os.listdir(path)
            else:
                with open(path, "rb") as f:
                    f.read(1)
            entry["readable"] = True
        except BaseException as exc:
            entry["error"] = _err(exc)
        results.append(entry)
    return {"results": results}


def env(req):
    names = sorted(os.environ.keys())
    hashes = set()
    for value in os.environ.values():
        hashes.add(hashlib.sha256(value.encode("utf-8", "surrogateescape")).hexdigest())
    proc_names = set()
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        for leaf in ("environ", "cmdline"):
            try:
                with open("/proc/" + pid + "/" + leaf, "rb") as f:
                    data = f.read()
            except BaseException:
                continue
            for item in data.split(b"\0"):
                if not item:
                    continue
                if leaf == "environ" and b"=" in item:
                    k, _, v = item.partition(b"=")
                    proc_names.add(k.decode("utf-8", "replace"))
                    hashes.add(hashlib.sha256(v).hexdigest())
                hashes.add(hashlib.sha256(item).hexdigest())
    return {"names": names, "proc_names": sorted(proc_names), "value_hashes": sorted(hashes)}


def _split_addr(addr):
    host, _, port = addr.rpartition(":")
    if host.startswith("[") and host.endswith("]"):
        host = host[1:-1]
    return host, int(port)


def network(req):
    results = []
    for addr in req.get("addrs", []):
        entry = {"addr": addr, "connected": False, "error": None}
        try:
            host, port = _split_addr(addr)
            family = socket.AF_INET6 if ":" in host else socket.AF_INET
            s = socket.socket(family, socket.SOCK_STREAM)
            s.settimeout(3)
            try:
                s.connect((host, port))
                entry["connected"] = True
            finally:
                s.close()
        except BaseException as exc:
            entry["error"] = _err(exc)
        results.append(entry)
    try:
        interfaces = sorted(name for _, name in socket.if_nameindex())
    except BaseException as exc:
        interfaces = ["<" + _err(exc) + ">"]
    try:
        socket.getaddrinfo("example.com", 443)
        dns = "resolved"
    except BaseException as exc:
        dns = _err(exc)
    try:
        u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            u.sendto(b"x", ("1.1.1.1", 53))
            udp = "sent"
        finally:
            u.close()
    except BaseException as exc:
        udp = _err(exc)
    return {"results": results, "interfaces": interfaces, "dns": dns, "udp": udp}


def write(req):
    attempts = []

    def attempt(label, fn):
        try:
            fn()
            attempts.append({"target": label, "succeeded": True, "error": None})
        except BaseException as exc:
            attempts.append({"target": label, "succeeded": False, "error": _err(exc)})

    def create(path):
        with open(path, "w") as f:
            f.write("x")

    def append(path):
        with open(path, "a") as f:
            f.write("x")

    attempt("/work/new_file", lambda: create("/work/new_file"))
    attempt("/work/probe.py (append)", lambda: append("/work/probe.py"))
    attempt("/work/new_dir", lambda: os.mkdir("/work/new_dir"))
    attempt("/work/probe.py (chmod)", lambda: os.chmod("/work/probe.py", 0o777))
    attempt("/harness/driver.py (append)", lambda: append("/harness/driver.py"))
    attempt("/new_file", lambda: create("/new_file"))
    attempt("/usr/new_file", lambda: create("/usr/new_file"))
    attempt("/dev/new_file", lambda: create("/dev/new_file"))
    attempt("/etc/new_file", lambda: create("/etc/new_file"))
    tmp_ok = True
    try:
        create("/tmp/probe_scratch")
        os.unlink("/tmp/probe_scratch")
    except BaseException:
        tmp_ok = False
    return {"attempts": attempts, "tmp_writable": tmp_ok}


def fork(req):
    cap = req.get("cap", 10000)
    r, w = os.pipe()
    kids = []
    error = None
    try:
        while len(kids) < cap:
            pid = os.fork()
            if pid == 0:
                try:
                    os.close(w)
                    os.read(r, 1)
                finally:
                    os._exit(0)
            kids.append(pid)
    except BaseException as exc:
        error = _err(exc)
    finally:
        os.close(w)
        for pid in kids:
            try:
                os.waitpid(pid, 0)
            except BaseException:
                pass
    return {"forks": len(kids), "error": error}


def memory(req):
    try:
        block = bytearray(req["bytes"])
        block[-1] = 1
        return {"allocated": True, "error": None}
    except BaseException as exc:
        return {"allocated": False, "error": _err(exc)}


def spin(req):
    while True:
        pass


def userns(req):
    try:
        import ctypes

        libc = ctypes.CDLL(None, use_errno=True)
    except BaseException as exc:
        return {"ctypes": _err(exc)}
    out = {"ctypes": "ok"}
    for label, flag in (("user", CLONE_NEWUSER), ("net", CLONE_NEWNET)):
        rc = libc.unshare(flag)
        out[label] = {"rc": rc, "errno": errno.errorcode.get(ctypes.get_errno(), "?") if rc != 0 else None}
    return out


def fds(req):
    import resource

    soft, _ = resource.getrlimit(resource.RLIMIT_NOFILE)
    open_fds = []
    for fd in range(0, min(soft, 65536)):
        try:
            os.fstat(fd)
            open_fds.append(fd)
        except OSError:
            pass
    return {"open": open_fds}


def tmpfill(req):
    chunk = b"\0" * (1 << 20)
    written = 0
    error = None
    names = []
    try:
        # One file up to the per-file limit, then more files until the
        # filesystem refuses.
        i = 0
        while written < req["cap"]:
            name = "/tmp/fill_%d" % i
            names.append(name)
            per_file = 0
            with open(name, "wb") as f:
                while per_file < req["file_cap"] and written < req["cap"]:
                    f.write(chunk)
                    f.flush()
                    per_file += len(chunk)
                    written += len(chunk)
            i += 1
    except BaseException as exc:
        error = _err(exc)
    single = {"bytes": 0, "error": None}
    for name in names:
        try:
            os.unlink(name)
        except BaseException:
            pass
    try:
        with open("/tmp/single", "wb") as f:
            while single["bytes"] <= req["file_cap"]:
                f.write(chunk)
                f.flush()
                single["bytes"] += len(chunk)
    except BaseException as exc:
        single["error"] = _err(exc)
    try:
        os.unlink("/tmp/single")
    except BaseException:
        pass
    return {"written": written, "error": error, "single_file": single}


PROBES = {
    "identity": identity,
    "capabilities": capabilities,
    "namespaces": namespaces,
    "files": files,
    "env": env,
    "network": network,
    "write": write,
    "fork": fork,
    "memory": memory,
    "spin": spin,
    "userns": userns,
    "fds": fds,
    "tmpfill": tmpfill,
}


def probe(req):
    return PROBES[req["name"]](req)
