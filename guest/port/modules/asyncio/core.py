# QuickPy M0 asyncio core.
#
# Adapted from MicroPython v1.29.0:
#   extmod/asyncio/core.py
#   ports/webassembly/asyncio/core.py
#
# The M0 loop is submit-and-drive-to-quiescence: it runs tasks until there is
# no runnable work, then returns to the adapter.  It never blocks on host I/O
# and never uses positive timers.

import sys
from _asyncio import TaskQueue, Task


class CancelledError(BaseException):
    pass


class TimeoutError(Exception):
    pass


# M0 uses NotImplementedError as the concrete UnsupportedError so that both
# asyncio.sleep(t>0) and time.sleep(t>0) (which raises NotImplementedError from
# the C HAL) are catchable under the same name.
UnsupportedError = NotImplementedError


_exc_context = {"message": "Task exception wasn't retrieved", "exception": None, "future": None}

# Sentinel returned by run_until_complete() when the main task is still alive
# but there is no runnable work left.
_QUIESCENT = object()


class SingletonGenerator:
    def __init__(self):
        self.state = 0
        self.exc = StopIteration()

    def __iter__(self):
        return self

    def __next__(self):
        if self.state is not None:
            _task_queue.push(cur_task, 0)
            self.state = None
            return None
        self.exc.__traceback__ = None
        raise self.exc


def sleep_ms(t):
    if t > 0:
        raise UnsupportedError("positive timers are not supported")
    return SingletonGenerator()


def sleep(t):
    # Check before truncation: sleep(0.0001) must not silently become sleep(0).
    if t > 0:
        raise UnsupportedError("positive timers are not supported")
    return sleep_ms(0)


def _promote_to_task(aw):
    return aw if isinstance(aw, Task) else create_task(aw)


def create_task(coro):
    if not hasattr(coro, "send"):
        raise TypeError("coroutine expected")
    t = Task(coro, globals())
    _task_queue.push(t)
    return t


def run_until_complete(main_task=None):
    global cur_task
    excs_all = (CancelledError, Exception)
    excs_stop = (CancelledError, StopIteration)
    while True:
        t = _task_queue.peek()
        if not t:
            cur_task = None
            if not main_task or not main_task.state:
                if main_task is None:
                    return None
                if isinstance(main_task.data, StopIteration):
                    return main_task.data.value
                raise main_task.data
            # Main task is still alive but nothing can run.  Return to the
            # adapter instead of blocking.
            return _QUIESCENT

        # Positive timers are not supported.  A task with a future key would
        # otherwise be run immediately, which would silently change semantics.
        if t.ph_key > 0:
            raise UnsupportedError("timers are not supported")

        t = _task_queue.pop()
        cur_task = t
        try:
            exc = t.data
            if not exc:
                t.coro.send(None)
            else:
                t.data = None
                t.coro.throw(exc)
        except excs_all as er:
            assert t.data is None
            awaited = t is main_task
            if awaited:
                cur_task = None
                if not isinstance(er, StopIteration):
                    t.state = False
                    raise er
                if t.state is None:
                    t.state = False
            if t.state:
                if t.state is True:
                    t.state = False if awaited else None
                elif callable(t.state):
                    t.state(t, er)
                    t.state = False
                    awaited = True
                else:
                    while t.state.peek():
                        _task_queue.push(t.state.pop())
                        awaited = True
                    t.state = False
                if not awaited and not isinstance(er, excs_stop):
                    _task_queue.push(t)
                t.data = er
            elif t.state is None:
                t.data = exc
                _exc_context["exception"] = exc
                _exc_context["future"] = t
                Loop.call_exception_handler(_exc_context)
            if t is main_task:
                if isinstance(er, StopIteration):
                    return er.value
                raise er


def run(coro):
    raise UnsupportedError("use quickpy.run(main())")


class Loop:
    _exc_handler = None

    def create_task(coro):
        return create_task(coro)

    def run_forever():
        raise UnsupportedError("run_forever is not supported")

    def run_until_complete(aw):
        raise UnsupportedError("use quickpy.run(main())")

    def stop():
        raise UnsupportedError("Loop.stop is not supported")

    def close():
        pass

    def set_exception_handler(handler):
        Loop._exc_handler = handler

    def get_exception_handler():
        return Loop._exc_handler

    def default_exception_handler(loop, context):
        print(context["message"])
        print("future:", context["future"], "coro=", context["future"].coro)
        sys.print_exception(context["exception"])

    def call_exception_handler(context):
        (Loop._exc_handler or Loop.default_exception_handler)(Loop, context)


def get_event_loop(runq_len=0, waitq_len=0):
    return Loop


def current_task():
    if cur_task is None:
        raise RuntimeError("no running event loop")
    return cur_task


def _init_event_loop():
    global _task_queue
    _task_queue = TaskQueue()
    return Loop


def new_event_loop():
    raise UnsupportedError("new_event_loop is not supported")


cur_task = None
_task_queue = None
_init_event_loop()
