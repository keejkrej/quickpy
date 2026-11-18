# QuickPy supported `asyncio` subset (M1, ABI 4)

This is the normative subset for the frozen guest implementation in
`guest/port/modules/asyncio/` and `guest/port/modules/quickpy.py`. It is not a
claim that upstream MicroPython `asyncio` works. The tests in
`integration/async_test.go` are the executable evidence; this file records the
positive and negative matrix.

## Driver and syntax

| Rule | Status | Evidence |
|---|---|---|
| `quickpy.run(coro)` is the only async driver. It is submit-and-drive-to-quiescence, not `asyncio.run`. | Supported | `TestAsyncGatherCreateTaskSleep0`, `TestAsyncRunFinalStatement` |
| `quickpy.run(...)` must be the **final top-level statement**. Statements after it execute before pending tools finish. | Supported baseline, documented ordering | `TestAsyncRunFinalStatement` |
| A second `quickpy.run(...)` in one execution raises `RuntimeError` ("may only be called once"). | Rejected | `TestAsyncUnsupportedPrimitives/second_quickpy.run` |
| Top-level `await` and top-level `return` are not supported. | Rejected by the compiler | SPEC §8.1; not re-tested here |
| Tool callables require an active `quickpy.run` context. Calling one outside a run raises `RuntimeError`. | Rejected | SPEC §8.1; not re-tested here |
| There is exactly one guest thread. `_thread`, `threading`, and `multiprocessing` are not importable. | Rejected | `TestAsyncNoThreadsOrIO` |

## Supported primitives

| Primitive | Behavior | Evidence |
|---|---|---|
| `asyncio.gather(*aws)` | Stock MicroPython gather. Normal completion and tool-call fan-out work. `return_exceptions` is inherited from the stock implementation but is not part of the independently tested subset. | `TestAsyncGatherCreateTaskSleep0`, `TestEventOrderingOutOfOrderGather` |
| `asyncio.create_task(coro)` | Creates and schedules a task. | `TestAsyncGatherCreateTaskSleep0`, `TestAsyncEventWaitWakeup` |
| `await asyncio.sleep(0)` | Zero-delay yield; interleaves tasks without host suspension. | `TestAsyncGatherCreateTaskSleep0` |
| `await asyncio.sleep(negative)` | Treated as zero (the implementation checks `t > 0`). | `TestAsyncPositiveTimersUnsupported/asyncio.sleep_negative_is_treated_as_zero` |
| `asyncio.Lock` | `async with lock` serializes a critical section across a zero-delay yield. | `TestAsyncLockCriticalSection` |
| `asyncio.Event` | `set()` wakes a waiting task; `wait()` on an already-set event returns immediately. | `TestAsyncEventWaitWakeup` |
| `asyncio.Event` with no setter | Terminal `Stalled`; never hangs. | `TestAsyncEventUnsatisfiableStalls` |
| `asyncio.wait_for(aw, None)` | Awaits `aw` directly. | `TestAsyncWaitFor/timeout_None` |
| `asyncio.wait_for(aw, 0)` | Runs the zero-delay timeout path and raises `asyncio.TimeoutError` if `aw` has not completed. | `TestAsyncWaitFor/timeout_zero` |
| `asyncio.CancelledError` | Catchable `BaseException` (not `Exception`). | `TestAsyncCancellationBeforeDispatch` |
| `asyncio.TimeoutError` | Catchable `Exception`. | `TestAsyncWaitFor/timeout_zero` |
| `asyncio.UnsupportedError` | Alias of `NotImplementedError`; catchable. | `TestAsyncPositiveTimersUnsupported` |
| `asyncio.get_event_loop()` | Returns the static `Loop` shim. | `TestAsyncUnsupportedPrimitives/loop_entry_points` |
| `asyncio.current_task()` | Returns the current task inside a run; raises `RuntimeError` outside a run. | `TestAsyncUnsupportedPrimitives/loop_entry_points` |
| `asyncio.Loop.close()` | No-op. | `TestAsyncUnsupportedPrimitives/loop_entry_points` |

## Rejected primitives and positive failure matrix

| Primitive / input | Result | Evidence |
|---|---|---|
| `await asyncio.sleep(t)` with `t > 0` | Catchable `asyncio.UnsupportedError` ("positive timers are not supported"). Tiny positive values are not truncated to zero. | `TestAsyncPositiveTimersUnsupported` |
| `time.sleep(t)` with `t > 0` | Catchable `asyncio.UnsupportedError` ("time.sleep is not supported"). | `TestAsyncPositiveTimersUnsupported/time.sleep_positive` |
| `time.sleep(0)` | Also rejected: it is a blocking sleep, not a scheduler yield. | `TestAsyncPositiveTimersUnsupported/time.sleep_zero_is_still_unsupported` |
| `asyncio.wait_for(aw, t)` with `t > 0` | Catchable `asyncio.UnsupportedError` from the positive-timer policy. | `TestAsyncWaitFor/positive_timeout_unsupported` |
| `asyncio.Queue` | Absent; `AttributeError`. | `TestAsyncUnsupportedPrimitives/missing_attributes` |
| `asyncio.wait` | Absent; `AttributeError`. | `TestAsyncUnsupportedPrimitives/missing_attributes` |
| `asyncio.Condition` | Absent; `AttributeError`. | `TestAsyncUnsupportedPrimitives/missing_attributes` |
| `asyncio.Semaphore` | Absent; `AttributeError`. | `TestAsyncUnsupportedPrimitives/missing_attributes` |
| `asyncio.open_connection`, `asyncio.start_server` | Catchable `asyncio.UnsupportedError` ("stream I/O is not supported"). | `TestAsyncUnsupportedPrimitives/stream_IO` |
| `asyncio.StreamReader`, `asyncio.StreamWriter` | Catchable `asyncio.UnsupportedError` ("stream I/O is not supported"). | `TestAsyncUnsupportedPrimitives/stream_IO` |
| `asyncio.ThreadSafeFlag` | Catchable `asyncio.UnsupportedError` ("ThreadSafeFlag is not supported"). | `TestAsyncUnsupportedPrimitives/stream_IO` |
| `asyncio.run(coro)` | Catchable `asyncio.UnsupportedError` ("use quickpy.run(main())"). | `TestAsyncUnsupportedPrimitives/loop_entry_points` |
| `asyncio.new_event_loop()` | Catchable `asyncio.UnsupportedError`. | `TestAsyncUnsupportedPrimitives/loop_entry_points` |
| `asyncio.Loop.run_forever()` | Catchable `asyncio.UnsupportedError`. | `TestAsyncUnsupportedPrimitives/loop_entry_points` |
| `asyncio.Loop.run_until_complete(aw)` | Catchable `asyncio.UnsupportedError` ("use quickpy.run(main())"). | `TestAsyncUnsupportedPrimitives/loop_entry_points` |
| `asyncio.Loop.stop()` | Catchable `asyncio.UnsupportedError`. | `TestAsyncUnsupportedPrimitives/loop_entry_points` |
| `import socket`, `ssl`, `select` | `ImportError`. | `TestAsyncNoThreadsOrIO` |
| `import _thread`, `threading`, `multiprocessing` | `ImportError`. | `TestAsyncNoThreadsOrIO` |

`io` is importable as a basic MicroPython module, but async stream I/O is not
supported (see the stream-I/O rows above). There is no host clock, no
`awaiting_timer` suspension, and no arbitrary module I/O.

## Cancellation

| Case | Behavior | Evidence |
|---|---|---|
| Cancel a task after it requests a host call but before the execution suspends | The call is dropped; the handler is never invoked; the script continues normally. | `TestAsyncCancellationBeforeDispatch` |
| Cancel a task already dispatched to the host | Not reachable in the timer-free single-threaded model. Whole-execution `Execution.Cancel()` is the supported way to stop an outstanding handler. | `TestCloseSuspendedExecution`, `TestCloseNoncooperativeTool` |
| Whole-execution cancel while a handler is outstanding | Host-led forced termination; the handler context is cancelled; a noncooperative handler is not awaited. | `TestCloseSuspendedExecution`, `TestCloseNoncooperativeTool` |

## Known limitation: one waiter per host future

`quickpy._HostFuture` stores a single `waiting` task. `_wait_future` assigns
`fut.waiting = asyncio.core.cur_task`. If two tasks await the same host future,
the second assignment overwrites the first and the first task may never wake.
The supported pattern is one host call per waiter:

```python
# Supported: one call per waiter.
a, b = await asyncio.gather(
    quickpy.tools.search(query="a"),
    quickpy.tools.search(query="b"),
)
```

Do not share one tool coroutine/future between multiple awaiters. This is a
guest implementation limit, not a claim that upstream `asyncio` future
semantics are available.

## Not part of the subset

- Full upstream MicroPython/CPython `asyncio` (streams, queues, conditions,
  semaphores, subprocesses, signals, thread-safe flags).
- Positive timers, host clocks, and `awaiting_timer` suspensions.
- Top-level `await`/`return`.
- Threads, multiprocessing, sockets, TLS, and `select`.
- Re-entering `quickpy.run` after a suspension.
- Cooperative abort while `Running` (host-led forced termination is the
  supported path).
