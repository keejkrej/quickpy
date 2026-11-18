# QuickPy M0 guest Python surface.
#
# quickpy.run(main()) is submit-and-drive-to-quiescence.  It returns normally
# when the main task is parked on host work.  For M0 the call must be the last
# top-level statement: statements after it run before outstanding tools finish.

import asyncio
import json
import _quickpy

class ToolError(Exception):
    def __init__(self, code, message):
        self.code = code
        self.message = message
        super().__init__(message)


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
        self.error = ToolError(code, message)
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


async def _call_tool(name, kwargs):
    return await _make_call(name, kwargs)


class _Tools:
    def __getattr__(self, name):
        def call(**kwargs):
            return _call_tool(name, kwargs)

        return call


tools = _Tools()

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


def _make_call(name, kwargs):
    global _next_id
    call_id = _next_id
    _next_id += 1
    args_json = json.dumps(kwargs)
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
