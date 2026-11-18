package integration_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

// TestMaxSourceBytesRejectedBeforeAdmission covers the pre-admission source
// cap. The exact cap is allowed; one byte over is InvalidRequest and must not
// consume MaxExecutions or start a guest instance.
func TestMaxSourceBytesRejectedBeforeAdmission(t *testing.T) {
	const maxSource = 11
	r := newRuntime(t, quickpy.RuntimeOptions{MaxSourceBytes: maxSource, MaxExecutions: 1})

	// Exactly at the cap: print("ok") is 11 bytes.
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("ok")`}))
	requireContains(t, textOf(res.Outputs), "ok")

	// One byte over: print("ok!") is 12 bytes.
	_, err := r.Execute(context.Background(), quickpy.ExecuteRequest{
		Source:   `print("ok!")`,
		Filename: "main.py",
	})
	qe := requireErrorKind(t, err, quickpy.KindInvalidRequest)
	requireContains(t, qe.Message, "source exceeds 11 bytes")

	// The rejected request must not consume the single execution slot.
	res = mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("ok")`}))
	requireContains(t, textOf(res.Outputs), "ok")
}

// TestMaxToolCallsGuestSide covers the ABI3 guest-side total tool-call cap.
// The cap is inclusive: with MaxToolCalls=1 the first call is dispatched and
// the second raises a catchable ToolLimitError before it reaches the host.
func TestMaxToolCallsGuestSide(t *testing.T) {
	requireABI3(t)

	t.Run("caught", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolCalls: 1})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    a = await quickpy.tools.echo(value=1)
    print("first", a)
    try:
        await quickpy.tools.echo(value=2)
    except quickpy.ToolLimitError as e:
        print("caught", str(type(e)), e.code, e.tool)
quickpy.run(main())`}))
		requireStatus(t, res, quickpy.StatusCompleted)
		out := textOf(res.Outputs)
		requireContains(t, out, "first ok")
		requireContains(t, out, "caught <class 'ToolLimitError'> tool_limit echo")
		if res.ToolCalls != 1 {
			t.Fatalf("ToolCalls = %d, want 1 (the second call must not be dispatched)", res.ToolCalls)
		}
	})

	t.Run("uncaught", func(t *testing.T) {
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolCalls: 1, EventSink: rec.sink})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.echo(value=1)
    await quickpy.tools.echo(value=2)
quickpy.run(main())`}))
		qe := requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
		if qe.Script == nil || qe.Script.Type != "ToolLimitError" {
			t.Fatalf("Script = %+v, want type ToolLimitError", qe.Script)
		}
		requireToolCause(t, qe.Tool, "echo", "tool_limit", false)
		if res.ToolCalls != 1 {
			t.Fatalf("ToolCalls = %d, want 1", res.ToolCalls)
		}
		toolEventCallID(t, rec.snapshot(), "echo")
	})

	t.Run("public zero is unlimited", func(t *testing.T) {
		// MaxToolCalls=0 is the public "no cap" value. The host maps it to the
		// ABI3 -1 sentinel; the guest must not fall back to an arbitrary 1000
		// call cap. 1001 sequential calls is deliberately just over the old
		// low-level default.
		const calls = 1001
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolCalls: 0})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: fmt.Sprintf(`import asyncio, quickpy
async def main():
    for i in range(%d):
        await quickpy.tools.echo(value=i)
    print("done", %d)
quickpy.run(main())`, calls, calls)}))
		requireStatus(t, res, quickpy.StatusCompleted)
		requireContains(t, textOf(res.Outputs), fmt.Sprintf("done %d", calls))
		if res.ToolCalls != calls {
			t.Fatalf("ToolCalls = %d, want %d", res.ToolCalls, calls)
		}
	})
}

// TestMaxPendingCallsLimitExceeded covers the host pending-call ledger cap.
// Two valid concurrent calls with MaxPendingCalls=1 must fail hard with
// LimitExceeded before any handler is dispatched.
func TestMaxPendingCallsLimitExceeded(t *testing.T) {
	var called atomic.Int32
	r := newRuntime(t, quickpy.RuntimeOptions{
		MaxConcurrentToolCalls: 1,
		MaxPendingCalls:        1,
	})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		called.Add(1)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await asyncio.gather(quickpy.tools.echo(value=1), quickpy.tools.echo(value=2))
quickpy.run(main())`}))
	requireErrorKind(t, err, quickpy.KindLimitExceeded)
	requireStatus(t, res, quickpy.StatusLimitExceeded)
	requireNoState(t, res)
	if got := called.Load(); got != 0 {
		t.Fatalf("handler called %d times, want 0 (pending cap is checked before dispatch)", got)
	}
}

// TestMaxResumesLimitExceeded covers the host resume cap with a valid async
// flow that needs more than one resume.
func TestMaxResumesLimitExceeded(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{MaxResumes: 1})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.echo(value=1)
    await quickpy.tools.echo(value=2)
quickpy.run(main())`}))
	requireErrorKind(t, err, quickpy.KindLimitExceeded)
	requireStatus(t, res, quickpy.StatusLimitExceeded)
	requireNoState(t, res)
}

// concurrencyProbe records the maximum number of handlers running at once and
// blocks each handler until the test releases it.
type concurrencyProbe struct {
	active    atomic.Int32
	maxActive atomic.Int32
	started   chan struct{}
	release   chan struct{}
}

func newConcurrencyProbe() *concurrencyProbe {
	return &concurrencyProbe{
		started: make(chan struct{}, 8),
		release: make(chan struct{}, 8),
	}
}

func (p *concurrencyProbe) handler(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
	n := p.active.Add(1)
	for {
		m := p.maxActive.Load()
		if n <= m || p.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	p.started <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	p.active.Add(-1)
	return "ok", nil
}

func (p *concurrencyProbe) waitStarted(t *testing.T, what string) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func (p *concurrencyProbe) requireNoStart(t *testing.T, d time.Duration, what string) {
	t.Helper()
	select {
	case <-p.started:
		t.Fatalf("%s started while the concurrency cap should have serialized it", what)
	case <-time.After(d):
	}
}

// TestMaxConcurrentToolCalls covers both sides of the concurrency cap: 1
// serializes real handlers, and >=2 allows observed overlap.
func TestMaxConcurrentToolCalls(t *testing.T) {
	t.Run("cap1 serializes", func(t *testing.T) {
		p := newConcurrencyProbe()
		r := newRuntime(t, quickpy.RuntimeOptions{MaxConcurrentToolCalls: 1})
		if err := r.RegisterTool("probe", p.handler); err != nil {
			t.Fatal(err)
		}
		e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await asyncio.gather(
        quickpy.tools.probe(value=1),
        quickpy.tools.probe(value=2),
        quickpy.tools.probe(value=3),
    )
quickpy.run(main())`})
		p.waitStarted(t, "first handler")
		p.requireNoStart(t, 100*time.Millisecond, "second handler")
		p.release <- struct{}{}
		p.waitStarted(t, "second handler")
		p.requireNoStart(t, 100*time.Millisecond, "third handler")
		p.release <- struct{}{}
		p.waitStarted(t, "third handler")
		p.release <- struct{}{}
		res := mustWait(t, e)
		requireStatus(t, res, quickpy.StatusCompleted)
		if got := p.maxActive.Load(); got != 1 {
			t.Fatalf("max concurrent handlers = %d, want 1", got)
		}
	})

	t.Run("cap2 overlaps", func(t *testing.T) {
		p := newConcurrencyProbe()
		r := newRuntime(t, quickpy.RuntimeOptions{MaxConcurrentToolCalls: 2})
		if err := r.RegisterTool("probe", p.handler); err != nil {
			t.Fatal(err)
		}
		e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await asyncio.gather(
        quickpy.tools.probe(value=1),
        quickpy.tools.probe(value=2),
    )
quickpy.run(main())`})
		p.waitStarted(t, "first handler")
		p.waitStarted(t, "second handler")
		if got := p.maxActive.Load(); got < 2 {
			t.Fatalf("max concurrent handlers = %d, want >= 2", got)
		}
		p.release <- struct{}{}
		p.release <- struct{}{}
		res := mustWait(t, e)
		requireStatus(t, res, quickpy.StatusCompleted)
	})
}

// TestMaxToolResultBytes covers the host tool-result cap and the invalid-value
// path. The cap is on the JSON-encoded result, so a 10-byte string is 12 bytes
// on the wire.
func TestMaxToolResultBytes(t *testing.T) {
	t.Run("exact allowed", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolResultBytes: 12})
		if err := r.RegisterTool("value", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return strings.Repeat("x", 10), nil
		}); err != nil {
			t.Fatal(err)
		}
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.value()
    print(len(r))
quickpy.run(main())`}))
		requireContains(t, textOf(res.Outputs), "10")
	})

	t.Run("one byte over", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolResultBytes: 12})
		if err := r.RegisterTool("value", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return strings.Repeat("x", 11), nil
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.value()
quickpy.run(main())`}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
	})

	t.Run("invalid Go shape", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		if err := r.RegisterTool("bad", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return make(chan int), nil
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.bad()
quickpy.run(main())`}))
		requireErrorKind(t, err, quickpy.KindProtocolError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
	})
}

// TestMaxMessageBytes covers the protocol message cap for the request, the
// initial state, and the response. The response boundary is found by binary
// search so the test asserts the exact allowed size rather than a guessed
// envelope size.
func TestMaxMessageBytes(t *testing.T) {
	t.Run("request cap", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 64})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("ok")`}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
	})

	t.Run("source over message cap is pre-execution", func(t *testing.T) {
		// The source is validated before the guest starts. The public contract
		// classifies an oversized source as InvalidRequest; MaxMessageBytes is
		// an additional pre-execution ceiling on the same field.
		r := newRuntime(t, quickpy.RuntimeOptions{MaxSourceBytes: 1 << 20, MaxMessageBytes: 64})
		_, err := r.Execute(context.Background(), quickpy.ExecuteRequest{
			Source:   strings.Repeat("#", 100),
			Filename: "main.py",
		})
		requireErrorKind(t, err, quickpy.KindInvalidRequest)
	})

	t.Run("initial state cap", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 128, MaxStateBytes: 1 << 20})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source:       `print("ok")`,
			InitialState: strings.Repeat("s", 200),
		}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
	})

	t.Run("response exact boundary", func(t *testing.T) {
		source := `print("A" * 100)`
		// Find the smallest MaxMessageBytes that lets the response through.
		lo, hi := 1, 4096
		for lo < hi {
			mid := (lo + hi) / 2
			r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: mid})
			_, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: source}))
			if err == nil {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		threshold := lo
		t.Logf("response message threshold = %d bytes", threshold)

		r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: threshold})
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: source}))
		requireContains(t, textOf(res.Outputs), strings.Repeat("A", 100))

		r = newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: threshold - 1})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: source}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
	})
}
