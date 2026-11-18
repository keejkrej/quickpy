# QuickPy M0 guest Python surface.
#
# quickpy.run(main()) is submit-and-drive-to-quiescence.  It returns normally
# when the main task is parked on host work.  For M0 the call must be the last
# top-level statement: statements after it run before outstanding tools finish.

import asyncio
import binascii
import json
import _quickpy

class ToolError(Exception):
    # Base class for every catchable tool failure.  `except quickpy.ToolError`
    # catches all subclasses.  `tool` and `call_id` are populated when the
    # failure is tied to a dispatched call; they stay None for failures raised
    # before a call is registered (for example argument validation).
    def __init__(self, code, message, tool=None, call_id=None):
        self.code = code
        self.message = message
        self.tool = tool
        self.call_id = call_id
        super().__init__(message)


class ToolNotFoundError(ToolError):
    # The host reported that the requested tool is not registered.
    pass


class ToolArgumentError(ToolError):
    # The script supplied arguments the guest cannot encode, or the host
    # rejected the argument payload.
    pass


class ToolLimitError(ToolError):
    # The execution's total tool-call cap would be exceeded.
    pass


def _tool_error(code, message, tool, call_id):
    # Map a host error code to the most specific catchable class.  Unknown
    # codes stay the base ToolError so callers can still catch them.
    if code == "tool_not_found":
        return ToolNotFoundError(code, message, tool=tool, call_id=call_id)
    if code == "tool_argument":
        return ToolArgumentError(code, message, tool=tool, call_id=call_id)
    return ToolError(code, message, tool=tool, call_id=call_id)


class StateError(Exception):
    # Structured, catchable state validation failure.  The previous valid
    # state is never replaced when this is raised.
    def __init__(self, code, message):
        self.code = code
        self.message = message
        super().__init__(message)


class _HostFuture:
    def __init__(self, call_id, name, args_json):
        self.call_id = call_id
        self.name = name
        self.args_json = args_json
        self.done = False
        self.result = None
        self.error = None
        self.waiting = None

    def _resolve(self, value):
        if self.done:
            return False
        self.done = True
        self.result = value
        if self.waiting is not None:
            asyncio.core._task_queue.push(self.waiting)
            self.waiting = None
        return True

    def _reject(self, code, message):
        if self.done:
            return False
        self.done = True
        self.error = _tool_error(code, message, self.name, self.call_id)
        if self.waiting is not None:
            asyncio.core._task_queue.push(self.waiting)
            self.waiting = None
        return True

    def remove(self, task):
        if self.waiting is task:
            self.waiting = None


def _wait_future(fut):
    if fut.done:
        if fut.error is not None:
            raise fut.error
        return fut.result
    fut.waiting = asyncio.core.cur_task
    asyncio.core.cur_task.data = fut
    yield
    if fut.error is not None:
        raise fut.error
    return fut.result


async def _call_tool(name, args, kwargs):
    if args:
        raise ToolArgumentError(
            "tool_argument",
            "tool " + str(name) + " accepts keyword arguments only",
            tool=name,
        )
    return await _make_call(name, kwargs)


class _Tools:
    def __getattr__(self, name):
        def call(*args, **kwargs):
            return _call_tool(name, args, kwargs)

        return call


tools = _Tools()

# ---------------------------------------------------------------------------
# Structured output: console diagnostics and images.
#
# quickpy.console.debug/info/warn/error(*args) emits exactly one console event
# per call.  Arguments are stringified and joined with single spaces, matching
# print().  The level is one of the exact strings "debug", "info", "warn",
# "error".
#
# quickpy.image(data, mime="image/png") emits exactly one image event and
# returns None.  data must be bytes or bytearray and is copied at call time, so
# later mutation of a bytearray cannot change the emitted bytes.  The MIME type
# must be a non-empty "token/token" value with no whitespace or control
# characters.  Invalid data, MIME, or an oversized payload raises TypeError or
# ValueError and emits nothing.
# ---------------------------------------------------------------------------

def _join_args(args):
    if not args:
        return ""
    parts = []
    for arg in args:
        parts.append(str(arg))
    return " ".join(parts)


class _Console:
    def debug(self, *args):
        _quickpy.emit_console("debug", _join_args(args))

    def info(self, *args):
        _quickpy.emit_console("info", _join_args(args))

    def warn(self, *args):
        _quickpy.emit_console("warn", _join_args(args))

    def error(self, *args):
        _quickpy.emit_console("error", _join_args(args))


console = _Console()


def image(data, mime="image/png"):
    if not isinstance(data, (bytes, bytearray)):
        raise TypeError("image data must be bytes or bytearray")
    if not isinstance(mime, str):
        raise TypeError("image mime must be a string")
    _quickpy.emit_image(mime, data)

# ---------------------------------------------------------------------------
# Explicit JSON-like state.
#
# State is a value, never a VM snapshot.  set_state() validates and takes a
# deep snapshot; later mutation of the caller's Python object does not change
# the stored state.  get_state() returns a fresh deep copy for the same reason.
# The guest keeps the pending value across suspensions and only the adapter
# exposes it on a successful completed response.
# ---------------------------------------------------------------------------

_MAX_STATE_BYTES = 1024 * 1024
_MAX_STATE_DEPTH = 64
_INT64_MAX = 9223372036854775807
_INT64_MIN = -9223372036854775807 - 1

_state_value = None
_state_written = False


def _type_name(value):
    try:
        return type(value).__name__
    except Exception:
        return "object"


def _spend(budget, n):
    budget[0] -= n
    if budget[0] < 0:
        raise StateError("size_exceeded", "state exceeds the guest size limit")


def _int_size(value):
    if value == 0:
        return 1
    n = 0
    if value < 0:
        # Do not negate: -INT64_MIN overflows long-long storage.
        n = 1
        while value <= -10:
            n += 1
            value //= 10
        return n + 1
    while value:
        n += 1
        value //= 10
    return n


def _json_str_size(value):
    # Exact-ish JSON/UTF-8 size.  Control characters are counted as \u00XX
    # (6 bytes), which is conservative for the short escapes json.dumps uses.
    n = 2
    for ch in value:
        o = ord(ch)
        if 0xD800 <= o <= 0xDFFF:
            raise StateError("invalid_string", "state strings must not contain lone surrogates")
        if o < 0x20 or o == 0x7F:
            n += 6
        elif o == 0x22 or o == 0x5C:
            n += 2
        elif o < 0x80:
            n += 1
        elif o < 0x800:
            n += 2
        elif o < 0x10000:
            n += 3
        else:
            n += 4
    return n


def _validate_state(value, depth, seen, budget):
    if depth > _MAX_STATE_DEPTH:
        raise StateError("depth_exceeded", "state exceeds the guest depth limit")

    if value is None:
        _spend(budget, 4)
        return
    if isinstance(value, bool):
        _spend(budget, 4 if value else 5)
        return
    if isinstance(value, int):
        if value < _INT64_MIN or value > _INT64_MAX:
            raise StateError("int_out_of_range", "state integers must fit in int64")
        _spend(budget, _int_size(value))
        return
    if isinstance(value, float):
        if value != value:
            raise StateError("non_finite_float", "state floats must be finite (NaN is not allowed)")
        if value != 0 and value == value * 2:
            raise StateError("non_finite_float", "state floats must be finite (Infinity is not allowed)")
        _spend(budget, len(str(value)))
        return
    if isinstance(value, str):
        _spend(budget, _json_str_size(value))
        return
    if isinstance(value, (list, tuple)):
        if id(value) in seen:
            raise StateError("cycle", "state values must not contain cycles")
        _spend(budget, 2)
        seen.append(id(value))
        first = True
        for item in value:
            if not first:
                _spend(budget, 1)
            first = False
            _validate_state(item, depth + 1, seen, budget)
        seen.pop()
        return
    if isinstance(value, dict):
        if id(value) in seen:
            raise StateError("cycle", "state values must not contain cycles")
        _spend(budget, 2)
        seen.append(id(value))
        first = True
        for key, item in value.items():
            if not isinstance(key, str):
                raise StateError("non_string_key", "state object keys must be strings")
            if not first:
                _spend(budget, 1)
            first = False
            _spend(budget, _json_str_size(key) + 1)
            _validate_state(item, depth + 1, seen, budget)
        seen.pop()
        return
    raise StateError("unsupported_type", "unsupported state value type: " + _type_name(value))


def _copy_state(value):
    if isinstance(value, list) or isinstance(value, tuple):
        out = []
        for item in value:
            out.append(_copy_state(item))
        return out
    if isinstance(value, dict):
        out = {}
        for key, item in value.items():
            out[key] = _copy_state(item)
        return out
    # None, bool, int, float and str are immutable.
    return value


def get_state():
    # Return a deep copy so callers cannot mutate the stored state without an
    # explicit set_state() call.
    return _copy_state(_state_value)


def set_state(value):
    global _state_value, _state_written
    try:
        _validate_state(value, 0, [], [_MAX_STATE_BYTES])
        new_value = _copy_state(value)
    except MemoryError:
        raise StateError("size_exceeded", "state exceeds the guest memory budget")
    _state_value = new_value
    _state_written = True


def _set_initial_state(text):
    global _state_value, _state_written
    if text is None:
        _state_value = None
        _state_written = False
        return
    if len(text) > _MAX_STATE_BYTES:
        raise StateError("size_exceeded", "initial_state exceeds the guest size limit")
    try:
        value = json.loads(text)
    except Exception as e:
        raise StateError("invalid_initial_state", "initial_state is not valid JSON: " + str(e))
    try:
        _validate_state(value, 0, [], [_MAX_STATE_BYTES])
        _state_value = _copy_state(value)
    except MemoryError:
        raise StateError("size_exceeded", "initial_state exceeds the guest memory budget")
    _state_written = False


def _state_json():
    # None means "no set_state() call"; the string "null" means an explicit
    # set_state(None).  The adapter only calls this for a completed response.
    if not _state_written:
        return None
    try:
        text = json.dumps(_state_value, separators=(",", ":"))
    except Exception as e:
        raise StateError("serialization_failed", "state could not be serialized: " + str(e))
    if len(text) > _MAX_STATE_BYTES:
        raise StateError("size_exceeded", "state exceeds the guest size limit")
    return text


_state = "idle"
_main_task = None
_pending = {}
_next_id = 1

# Total tool-call cap.  -1 means unlimited; >= 0 is an inclusive cap on the
# number of host calls this execution may request.  The adapter sets this from
# the ABI3 start message before any source runs.
_max_tool_calls = -1
_tool_calls_made = 0

_MAX_TOOL_ARG_BYTES = 1024 * 1024
_MAX_TOOL_ARG_DEPTH = 64


def _set_max_tool_calls(value):
    global _max_tool_calls
    _max_tool_calls = value


def _tool_arg_error(message, tool):
    return ToolArgumentError("tool_argument", message, tool=tool)


def _tool_str_size(value, tool):
    # _json_str_size is shared with state validation and raises StateError for
    # lone surrogates; tool arguments must surface ToolArgumentError instead.
    try:
        return _json_str_size(value)
    except StateError:
        raise _tool_arg_error("tool argument strings must not contain lone surrogates", tool)


def _spend_tool(budget, n, tool):
    budget[0] -= n
    if budget[0] < 0:
        raise _tool_arg_error("tool arguments exceed the guest size limit", tool)


def _validate_tool_value(value, depth, seen, budget, tool):
    # Tool arguments use the same JSON-like value rules as state, but failures
    # are ToolArgumentError so the script can catch them.  Validating before
    # json.dumps() keeps unsupported types and cycles from reaching the host as
    # malformed JSON.
    if depth > _MAX_TOOL_ARG_DEPTH:
        raise _tool_arg_error("tool arguments exceed the depth limit", tool)
    if value is None:
        _spend_tool(budget, 4, tool)
        return
    if isinstance(value, bool):
        _spend_tool(budget, 4 if value else 5, tool)
        return
    if isinstance(value, int):
        if value < _INT64_MIN or value > _INT64_MAX:
            raise _tool_arg_error("tool argument integers must fit in int64", tool)
        _spend_tool(budget, _int_size(value), tool)
        return
    if isinstance(value, float):
        if value != value:
            raise _tool_arg_error("tool argument floats must be finite", tool)
        if value != 0 and value == value * 2:
            raise _tool_arg_error("tool argument floats must be finite", tool)
        _spend_tool(budget, len(str(value)), tool)
        return
    if isinstance(value, str):
        _spend_tool(budget, _tool_str_size(value, tool), tool)
        return
    if isinstance(value, (list, tuple)):
        if id(value) in seen:
            raise _tool_arg_error("tool arguments must not contain cycles", tool)
        _spend_tool(budget, 2, tool)
        seen.append(id(value))
        first = True
        for item in value:
            if not first:
                _spend_tool(budget, 1, tool)
            first = False
            _validate_tool_value(item, depth + 1, seen, budget, tool)
        seen.pop()
        return
    if isinstance(value, dict):
        if id(value) in seen:
            raise _tool_arg_error("tool arguments must not contain cycles", tool)
        _spend_tool(budget, 2, tool)
        seen.append(id(value))
        first = True
        for key, item in value.items():
            if not isinstance(key, str):
                raise _tool_arg_error("tool argument keys must be strings", tool)
            if not first:
                _spend_tool(budget, 1, tool)
            first = False
            _spend_tool(budget, _tool_str_size(key, tool) + 1, tool)
            _validate_tool_value(item, depth + 1, seen, budget, tool)
        seen.pop()
        return
    raise _tool_arg_error("unsupported tool argument type: " + _type_name(value), tool)


def _make_call(name, kwargs):
    global _next_id, _tool_calls_made
    # Enforce the total-call cap before allocating or registering anything, so
    # a blocked call never reaches the host.
    if _max_tool_calls >= 0 and _tool_calls_made >= _max_tool_calls:
        raise ToolLimitError(
            "tool_limit",
            "tool call limit of " + str(_max_tool_calls) + " exceeded",
            tool=name,
        )
    try:
        _validate_tool_value(kwargs, 0, [], [_MAX_TOOL_ARG_BYTES], name)
        args_json = json.dumps(kwargs)
    except MemoryError:
        raise
    except ToolError:
        raise
    except Exception as e:
        raise ToolArgumentError(
            "tool_argument",
            "invalid tool arguments: " + str(e),
            tool=name,
        )
    call_id = _next_id
    _next_id += 1
    _tool_calls_made += 1
    fut = _HostFuture(call_id, name, args_json)
    _pending[call_id] = fut
    _quickpy.register_call(call_id, name, args_json)
    return _wait_future(fut)


def _deliver(call_id, ok, value_json, error_json):
    fut = _pending.get(call_id)
    if fut is None:
        return False
    if ok:
        value = json.loads(value_json) if value_json is not None else None
        del _pending[call_id]
        return fut._resolve(value)
    else:
        err = json.loads(error_json) if error_json is not None else {}
        del _pending[call_id]
        return fut._reject(err.get("code", "tool_error"), err.get("message", "tool failed"))


# ---------------------------------------------------------------------------
# Optional filesystem facade.
#
# quickpy.fs is a thin async wrapper over the host tools fs_read_text,
# fs_write_text, fs_read_bytes, fs_write_bytes, fs_exists, fs_listdir,
# fs_mkdir, and fs_remove.  The host decides whether those tools exist and
# enforces every path and permission rule; the guest only forwards arguments.
# If the host did not register a tool, the call raises ToolNotFoundError.
#
# Methods return plain Python values, not the host's wrapper objects:
#   read_text(path, encoding="utf-8") -> str
#   write_text(path, text) -> int (bytes written)
#   read_bytes(path) -> bytes
#   write_bytes(path, data) -> int (bytes written)
#   (write_bytes sends canonical base64 without b2a_base64's trailing newline)
#   exists(path) -> bool
#   listdir(path=".") -> list of {"name": str, "type": str, "size": int}
#   mkdir(path, parents=False) -> bool
#   remove(path) -> bool
#
# Local type errors raise TypeError.  Unexpected keyword arguments raise
# ToolArgumentError, matching the tool facade.  Host failures stay structured:
# ToolError and its subclasses propagate unchanged, except that the common
# filesystem codes are translated for ergonomic use:
#   not_found -> OSError(2, message)          (.errno == 2)
#   permission_denied -> OSError(13, message) (.errno == 13)
#   invalid_path -> ValueError("invalid_path: " + message)
# This MicroPython ROM level does not define FileNotFoundError or
# PermissionError, so the facade uses OSError with the conventional errno
# numbers.  This is not a CPython os emulation layer and does not claim errno
# parity beyond those two codes.
# ---------------------------------------------------------------------------

def _fs_no_kwargs(kwargs, tool):
    for key in kwargs:
        raise ToolArgumentError(
            "tool_argument",
            "unexpected keyword argument: " + str(key),
            tool=tool,
        )


def _fs_path(path):
    if not isinstance(path, str):
        raise TypeError("path must be a string")


async def _fs_call(name, **kwargs):
    try:
        return await getattr(tools, name)(**kwargs)
    except ToolError as e:
        if e.code == "not_found":
            raise OSError(2, e.message)
        if e.code == "permission_denied":
            raise OSError(13, e.message)
        if e.code == "invalid_path":
            raise ValueError("invalid_path: " + e.message)
        raise


class _FS:
    async def read_text(self, path, encoding="utf-8", **kwargs):
        _fs_no_kwargs(kwargs, "fs_read_text")
        _fs_path(path)
        if not isinstance(encoding, str):
            raise TypeError("encoding must be a string")
        result = await _fs_call("fs_read_text", path=path, encoding=encoding)
        return result["text"]

    async def write_text(self, path, text, **kwargs):
        _fs_no_kwargs(kwargs, "fs_write_text")
        _fs_path(path)
        if not isinstance(text, str):
            raise TypeError("text must be a string")
        result = await _fs_call("fs_write_text", path=path, text=text)
        return result["written"]

    async def read_bytes(self, path, **kwargs):
        _fs_no_kwargs(kwargs, "fs_read_bytes")
        _fs_path(path)
        result = await _fs_call("fs_read_bytes", path=path)
        return binascii.a2b_base64(bytes(result["base64"], "utf-8"))

    async def write_bytes(self, path, data, **kwargs):
        _fs_no_kwargs(kwargs, "fs_write_bytes")
        _fs_path(path)
        if not isinstance(data, (bytes, bytearray)):
            raise TypeError("data must be bytes or bytearray")
        # b2a_base64 appends a newline; the host decoder is strict about
        # whitespace, so send canonical base64 without it.
        encoded = str(binascii.b2a_base64(data), "utf-8").rstrip("\n")
        result = await _fs_call("fs_write_bytes", path=path, base64=encoded)
        return result["written"]

    async def exists(self, path, **kwargs):
        _fs_no_kwargs(kwargs, "fs_exists")
        _fs_path(path)
        result = await _fs_call("fs_exists", path=path)
        return result["exists"]

    async def listdir(self, path=".", **kwargs):
        _fs_no_kwargs(kwargs, "fs_listdir")
        _fs_path(path)
        result = await _fs_call("fs_listdir", path=path)
        return result["entries"]

    async def mkdir(self, path, parents=False, **kwargs):
        _fs_no_kwargs(kwargs, "fs_mkdir")
        _fs_path(path)
        if not isinstance(parents, bool):
            raise TypeError("parents must be a bool")
        result = await _fs_call("fs_mkdir", path=path, parents=parents)
        return result["created"]

    async def remove(self, path, **kwargs):
        _fs_no_kwargs(kwargs, "fs_remove")
        _fs_path(path)
        result = await _fs_call("fs_remove", path=path)
        return result["removed"]


fs = _FS()


def run(aw):
    global _state, _main_task
    if _state != "idle":
        raise RuntimeError("quickpy.run may only be called once")
    _state = "running"
    _main_task = asyncio.core._promote_to_task(aw)
    try:
        result = asyncio.core.run_until_complete(_main_task)
    except BaseException:
        _state = "failed"
        raise
    if result is asyncio.core._QUIESCENT:
        _state = "suspended" if _quickpy.has_pending_calls() else "stalled"
        return None
    _state = "completed"
    return result


def _resume():
    global _state
    if _state not in ("suspended", "stalled"):
        raise RuntimeError("no suspended run")
    _state = "running"
    try:
        result = asyncio.core.run_until_complete(_main_task)
    except BaseException:
        _state = "failed"
        raise
    if result is asyncio.core._QUIESCENT:
        _state = "suspended" if _quickpy.has_pending_calls() else "stalled"
        return None
    _state = "completed"
    return result
