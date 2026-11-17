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
