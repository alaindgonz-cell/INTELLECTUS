# INTELLECTUS sandbox driver (bwrap-python/v1).
#
# This is the only harness code that runs INSIDE the sandbox. It is embedded
# in the Go binary (go:embed), written read-only to /harness/driver.py and
# covered by the worker's implementation digest.
#
# Protocol
#   stdin : one JSON object {"path": "<module path relative to /work>",
#           "function": "<callable name>", "input": <json>}
#   fd 3  : EXACTLY ONE JSON line, one of
#           {"returns": <json>}
#           {"raises": "<ClassName>", "mro": ["<ClassName>", ...], "message": "<truncated>"}
#           {"error": "import failed: <ExcName>: <msg>"}
#           {"error": "entrypoint not found: <name>"}
#           {"error": "entrypoint not callable: <name>"}
#           {"error": "result not JSON-serializable: <type>"}
#           {"error": "driver: <reason>"}
#   stdout / stderr belong to the candidate; the engine captures them
#   (bounded) and never parses them.
#
# The candidate runs in this same interpreter, so it can write to fd 3 itself.
# The engine therefore accepts only exactly one well-formed line; extra or
# malformed lines make the case ERROR. A candidate that writes one forged line
# and exits before this driver does is NOT detectable (see README.md).
import os
import sys

# Bind what the driver needs before any candidate code runs, so that
# monkeypatching the modules afterwards does not change how results are
# written (hardening only; the candidate controls the process).
_os_write = os.write
_os_exit = os._exit
import importlib.util as _importlib_util  # noqa: E402
import json as _json  # noqa: E402
import math as _math  # noqa: E402

_dumps = _json.dumps
_loads = _json.loads
_isfinite = _math.isfinite

RESULT_FD = 3
WORK = "/work"
HARNESS = "/harness"
MAX_MESSAGE = 1000
MAX_NODES = 1000000
MAX_DEPTH = 500


def _flush_std():
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.flush()
        except BaseException:
            pass


def _write_line_and_exit(line):
    """Write one result line to fd 3 and exit immediately (os._exit: no
    atexit handlers, no waiting for candidate threads)."""
    _flush_std()
    view = memoryview((line + "\n").encode("ascii"))
    try:
        while view:
            n = _os_write(RESULT_FD, view)
            view = view[n:]
    except BaseException:
        _os_exit(3)
    _os_exit(0)


def _emit(obj):
    try:
        line = _dumps(obj, ensure_ascii=True, allow_nan=False, separators=(",", ":"))
    except BaseException as exc:
        line = _dumps({"error": "driver: cannot encode result: " + type(exc).__name__})
    _write_line_and_exit(line)


def _safe_str(exc):
    try:
        s = str(exc)
    except BaseException:
        return "<unprintable " + type(exc).__name__ + ">"
    if not isinstance(s, str):
        return "<unprintable " + type(exc).__name__ + ">"
    if len(s) > MAX_MESSAGE:
        s = s[:MAX_MESSAGE] + "...[truncated]"
    return s


def _describe_exception(exc):
    cls = type(exc)
    name = cls.__name__
    try:
        mro = [c.__name__ for c in cls.__mro__]
    except BaseException:
        mro = [name]
    return {"raises": name, "mro": mro, "message": _safe_str(exc)}


def _json_type_problem(value):
    """Return a description of the first part of value that has no exact JSON
    form (None if the whole value is representable). Dict keys must be str:
    json.dumps would otherwise silently turn 1 into "1"."""
    stack = [(value, 0)]
    nodes = 0
    while stack:
        v, depth = stack.pop()
        nodes += 1
        if nodes > MAX_NODES:
            return "value too large"
        if depth > MAX_DEPTH:
            return "nesting too deep"
        if v is None or v is True or v is False:
            continue
        if isinstance(v, str):
            continue
        if isinstance(v, int):
            continue
        if isinstance(v, float):
            if not _isfinite(v):
                return "float (non-finite)"
            continue
        if isinstance(v, (list, tuple)):
            for item in v:
                stack.append((item, depth + 1))
            continue
        if isinstance(v, dict):
            for k, item in v.items():
                if not isinstance(k, str):
                    return "dict with " + type(k).__name__ + " key"
                stack.append((item, depth + 1))
            continue
        return type(v).__name__
    return None


def _emit_returns(value):
    try:
        problem = _json_type_problem(value)
    except BaseException as exc:
        problem = type(value).__name__ + " (" + type(exc).__name__ + ")"
    if problem is None:
        try:
            line = _dumps({"returns": value}, ensure_ascii=True, allow_nan=False, separators=(",", ":"))
        except BaseException as exc:
            problem = type(value).__name__ + " (" + type(exc).__name__ + ")"
        else:
            _write_line_and_exit(line)
    _emit({"error": "result not JSON-serializable: " + problem})


def main():
    try:
        raw = sys.stdin.buffer.read()
        req = _loads(raw)
        path = req["path"]
        func = req["function"]
        arg = req["input"]
        if not isinstance(path, str) or not isinstance(func, str):
            raise TypeError("path and function must be strings")
    except BaseException as exc:
        _emit({"error": "driver: bad request: " + type(exc).__name__})
        return

    full = os.path.join(WORK, path)
    moddir = os.path.dirname(full)
    # The driver's own directory must never be importable (PYTHONSAFEPATH=1
    # already keeps it off sys.path; this is belt and braces).
    sys.path[:] = [p for p in sys.path if p not in ("", ".", HARNESS)]
    for p in (WORK, moddir):
        if p in sys.path:
            sys.path.remove(p)
    sys.path.insert(0, WORK)
    sys.path.insert(0, moddir)

    name = os.path.splitext(os.path.basename(full))[0]
    try:
        spec = _importlib_util.spec_from_file_location(name, full)
        if spec is None or spec.loader is None:
            raise ImportError("cannot load " + path)
        module = _importlib_util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
    except BaseException as exc:
        _emit({"error": "import failed: " + type(exc).__name__ + ": " + _safe_str(exc)})
        return

    try:
        fn = getattr(module, func)
    except BaseException:
        _emit({"error": "entrypoint not found: " + func})
        return
    if not callable(fn):
        _emit({"error": "entrypoint not callable: " + func})
        return

    try:
        value = fn(arg)
    except BaseException as exc:
        _emit(_describe_exception(exc))
        return
    _emit_returns(value)


main()
_emit({"error": "driver: fell through"})
