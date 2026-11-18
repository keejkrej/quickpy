package integration_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/keejkrej/quickpy"
)

// TestAsyncGatherCreateTaskSleep0 covers the supported scheduling core:
// asyncio.gather, asyncio.create_task, and zero-delay yields interleave
// deterministically without host suspension.
func TestAsyncGatherCreateTaskSleep0(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def worker(name):
    print("start", name)
    await asyncio.sleep(0)
    print("end", name)
async def main():
    t = asyncio.create_task(worker("a"))
    await asyncio.gather(t, worker("b"))
quickpy.run(main())`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	if got, want := textOf(res.Outputs), "start a\nstart b\nend a\nend b\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// TestAsyncLockCriticalSection proves asyncio.Lock serializes a critical
// section across a zero-delay yield.
func TestAsyncLockCriticalSection(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
lock = asyncio.Lock()
async def worker(name):
    async with lock:
        print("enter", name)
        await asyncio.sleep(0)
        print("exit", name)
async def main():
    await asyncio.gather(worker("a"), worker("b"))
quickpy.run(main())`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	if got, want := textOf(res.Outputs), "enter a\nexit a\nenter b\nexit b\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// TestAsyncEventWaitWakeup covers a satisfied asyncio.Event wait.
func TestAsyncEventWaitWakeup(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def waiter(ev):
    print("wait")
    await ev.wait()
    print("woke")
async def main():
    ev = asyncio.Event()
    t = asyncio.create_task(waiter(ev))
    await asyncio.sleep(0)
    ev.set()
    await t
quickpy.run(main())`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	if got, want := textOf(res.Outputs), "wait\nwoke\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// TestAsyncEventUnsatisfiableStalls proves an Event wait with no setter is a
// terminal stall, not a hang.
func TestAsyncEventUnsatisfiableStalls(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    ev = asyncio.Event()
    await ev.wait()
quickpy.run(main())`}))
	requireErrorKind(t, err, quickpy.KindStalled)
	requireStatus(t, res, quickpy.StatusStalled)
	requireNoState(t, res)
}

// TestAsyncPositiveTimersUnsupported covers the positive-timer failure matrix.
// asyncio.sleep(0) is a yield; any positive delay is a catchable
// UnsupportedError. time.sleep is unsupported even at zero because it is a
// blocking sleep, not a scheduler yield.
func TestAsyncPositiveTimersUnsupported(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "asyncio.sleep positive",
			source: `import asyncio, quickpy
async def main():
    try:
        await asyncio.sleep(0.1)
    except asyncio.UnsupportedError as e:
        print("caught", e)
quickpy.run(main())`,
			want: "caught positive timers are not supported\n",
		},
		{
			name: "asyncio.sleep tiny positive is not truncated",
			source: `import asyncio, quickpy
async def main():
    try:
        await asyncio.sleep(0.0001)
    except asyncio.UnsupportedError as e:
        print("caught", e)
quickpy.run(main())`,
			want: "caught positive timers are not supported\n",
		},
		{
			name: "time.sleep positive",
			source: `import asyncio, time
try:
    time.sleep(0.1)
except asyncio.UnsupportedError as e:
    print("caught", e)`,
			want: "caught time.sleep is not supported\n",
		},
		{
			name: "time.sleep zero is still unsupported",
			source: `import asyncio, time
try:
    time.sleep(0)
except asyncio.UnsupportedError as e:
    print("caught", e)`,
			want: "caught time.sleep is not supported\n",
		},
		{
			name: "asyncio.sleep zero is a yield",
			source: `import asyncio, quickpy
async def main():
    await asyncio.sleep(0)
    print("zero ok")
quickpy.run(main())`,
			want: "zero ok\n",
		},
		{
			name: "asyncio.sleep negative is treated as zero",
			source: `import asyncio, quickpy
async def main():
    await asyncio.sleep(-1)
    print("negative ok")
quickpy.run(main())`,
			want: "negative ok\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRuntime(t, quickpy.RuntimeOptions{})
			res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: tc.source}))
			requireStatus(t, res, quickpy.StatusCompleted)
			if got := textOf(res.Outputs); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAsyncCancellationBeforeDispatch documents the supported cancellation
// boundary. A task cancelled after it requests a host call but before the
// execution suspends drops the call: the handler is never invoked and the
// script continues normally. Cancellation of a call already dispatched to the
// host is not reachable in the timer-free single-threaded model; whole-
// execution Cancel is the supported way to stop an outstanding handler.
func TestAsyncCancellationBeforeDispatch(t *testing.T) {
	var called atomic.Int32
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("block", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		called.Add(1)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    t = asyncio.create_task(quickpy.tools.block(value=1))
    await asyncio.sleep(0)
    t.cancel()
    try:
        await t
    except asyncio.CancelledError:
        print("cancelled")
    print("after")
quickpy.run(main())`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	out := textOf(res.Outputs)
	requireContains(t, out, "cancelled")
	requireContains(t, out, "after")
	if got := called.Load(); got != 0 {
		t.Fatalf("handler called %d times, want 0 (the call was cancelled before host dispatch)", got)
	}
}

// TestAsyncWaitFor covers the supported wait_for paths and the positive-timer
// rejection.
func TestAsyncWaitFor(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "timeout None",
			source: `import asyncio, quickpy
async def child():
    await asyncio.sleep(0)
    return 7
async def main():
    print("r", await asyncio.wait_for(child(), None))
quickpy.run(main())`,
			want: "r 7\n",
		},
		{
			name: "timeout zero",
			source: `import asyncio, quickpy
async def child():
    await asyncio.sleep(0)
    return 7
async def main():
    try:
        await asyncio.wait_for(child(), 0)
    except asyncio.TimeoutError:
        print("timeout")
quickpy.run(main())`,
			want: "timeout\n",
		},
		{
			name: "positive timeout unsupported",
			source: `import asyncio, quickpy
async def child():
    await asyncio.sleep(0)
    return 7
async def main():
    try:
        await asyncio.wait_for(child(), 1)
    except asyncio.UnsupportedError as e:
        print("unsupported", e)
quickpy.run(main())`,
			want: "unsupported positive timers are not supported\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRuntime(t, quickpy.RuntimeOptions{})
			res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: tc.source}))
			requireStatus(t, res, quickpy.StatusCompleted)
			if got := textOf(res.Outputs); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAsyncUnsupportedPrimitives covers primitives that are deliberately
// absent or raise a clear UnsupportedError. None of them may hang.
func TestAsyncUnsupportedPrimitives(t *testing.T) {
	t.Run("missing attributes", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio
for name in ("Queue", "wait", "Condition", "Semaphore"):
    try:
        getattr(asyncio, name)
        print(name, "present")
    except AttributeError:
        print(name, "AttributeError")`}))
		requireStatus(t, res, quickpy.StatusCompleted)
		for _, name := range []string{"Queue", "wait", "Condition", "Semaphore"} {
			requireContains(t, textOf(res.Outputs), name+" AttributeError")
		}
	})

	t.Run("stream IO", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio
for name in ("open_connection", "start_server", "StreamReader", "StreamWriter", "ThreadSafeFlag"):
    try:
        getattr(asyncio, name)()
        print(name, "ok")
    except asyncio.UnsupportedError as e:
        print(name, "unsupported", e)`}))
		requireStatus(t, res, quickpy.StatusCompleted)
		out := textOf(res.Outputs)
		for _, name := range []string{"open_connection", "start_server", "StreamReader", "StreamWriter"} {
			requireContains(t, out, name+" unsupported stream I/O is not supported")
		}
		requireContains(t, out, "ThreadSafeFlag unsupported ThreadSafeFlag is not supported")
	})

	t.Run("loop entry points", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio
for name, call in (
    ("run", lambda: asyncio.run(None)),
    ("new_event_loop", lambda: asyncio.new_event_loop()),
    ("run_forever", lambda: asyncio.Loop.run_forever()),
    ("run_until_complete", lambda: asyncio.Loop.run_until_complete(None)),
    ("stop", lambda: asyncio.Loop.stop()),
):
    try:
        call()
        print(name, "ok")
    except asyncio.UnsupportedError as e:
        print(name, "unsupported", e)
asyncio.Loop.close()
print("close ok")
print("get_event_loop", asyncio.get_event_loop() is not None)
try:
    asyncio.current_task()
except RuntimeError as e:
    print("current_task", e)`}))
		requireStatus(t, res, quickpy.StatusCompleted)
		out := textOf(res.Outputs)
		requireContains(t, out, "run unsupported use quickpy.run(main())")
		requireContains(t, out, "new_event_loop unsupported new_event_loop is not supported")
		requireContains(t, out, "run_forever unsupported run_forever is not supported")
		requireContains(t, out, "run_until_complete unsupported use quickpy.run(main())")
		requireContains(t, out, "stop unsupported Loop.stop is not supported")
		requireContains(t, out, "close ok")
		requireContains(t, out, "get_event_loop True")
		requireContains(t, out, "current_task no running event loop")
	})

	t.Run("second quickpy.run", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    pass
quickpy.run(main())
quickpy.run(main())`}))
		qe := requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		if qe.Script == nil || qe.Script.Type != "RuntimeError" {
			t.Fatalf("Script = %+v, want RuntimeError", qe.Script)
		}
		requireContains(t, qe.Script.Message, "may only be called once")
	})
}

// TestAsyncRunFinalStatement documents the baseline syntax rule: quickpy.run
// must be the final top-level statement. Statements after it execute before
// pending tools finish.
func TestAsyncRunFinalStatement(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.echo(value=1)
    print("tool", r)
quickpy.run(main())
print("after run")`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	if got, want := textOf(res.Outputs), "after run\ntool ok\n"; got != want {
		t.Fatalf("output = %q, want %q (statements after quickpy.run run before pending tools finish)", got, want)
	}
}

// TestAsyncNoThreadsOrIO covers the module boundary: threads, multiprocessing,
// sockets, TLS, and select are not importable. io is a basic MicroPython
// module, but async stream I/O is rejected by asyncio (see above).
func TestAsyncNoThreadsOrIO(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	for _, module := range []string{"_thread", "threading", "multiprocessing", "socket", "ssl", "select"} {
		t.Run(module, func(t *testing.T) {
			res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{
				Source: fmt.Sprintf(`try:
    import %s
    print("imported")
except ImportError:
    print("ImportError")`, module),
			}))
			requireStatus(t, res, quickpy.StatusCompleted)
			requireContains(t, textOf(res.Outputs), "ImportError")
		})
	}
}
