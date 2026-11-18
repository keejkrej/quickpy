package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/keejkrej/quickpy"
)

// TestStructuredToolErrorClasses covers the ABI3 Python-visible tool error
// classes. Caught failures are ordinary script success, not execution errors.
func TestStructuredToolErrorClasses(t *testing.T) {
	requireABI3(t)

	t.Run("caught classes", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.RegisterTool("fail", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return nil, &quickpy.ToolError{Code: "permission_denied", Message: "no"}
		}); err != nil {
			t.Fatal(err)
		}
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"caught": True})
async def main():
    try:
        await quickpy.tools.missing(x=1)
    except quickpy.ToolNotFoundError as e:
        print("missing", str(type(e)), e.code, e.tool)
    try:
        await quickpy.tools.echo(1)
    except quickpy.ToolArgumentError as e:
        print("argument", str(type(e)), e.code, e.tool)
    try:
        await quickpy.tools.fail(value=1)
    except quickpy.ToolError as e:
        print("handler", str(type(e)), e.code, e.tool)
quickpy.run(main())`}))
		requireStatus(t, res, quickpy.StatusCompleted)
		out := textOf(res.Outputs)
		requireContains(t, out, "missing <class 'ToolNotFoundError'> tool_not_found missing")
		requireContains(t, out, "argument <class 'ToolArgumentError'> tool_argument echo")
		requireContains(t, out, "handler <class 'ToolError'> permission_denied fail")
		// A caught tool failure is not an execution error, so a state write
		// before it is committed normally.
		requireState(t, res, map[string]any{"caught": true})
	})

	t.Run("caught limit class", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolCalls: 1})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await quickpy.tools.echo(value=1)
    try:
        await quickpy.tools.echo(value=2)
    except quickpy.ToolLimitError as e:
        print("limit", str(type(e)), e.code, e.tool)
quickpy.run(main())`}))
		requireStatus(t, res, quickpy.StatusCompleted)
		requireContains(t, textOf(res.Outputs), "limit <class 'ToolLimitError'> tool_limit echo")
	})
}

// TestUncaughtToolErrorCause covers the public ScriptErrorInfo + ToolCause
// shape for uncaught tool failures. A regular Python exception must not carry
// a ToolCause.
func TestUncaughtToolErrorCause(t *testing.T) {
	requireABI3(t)

	t.Run("handler ToolError", func(t *testing.T) {
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{EventSink: rec.sink})
		if err := r.RegisterTool("fail", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return nil, &quickpy.ToolError{Code: "permission_denied", Message: "no"}
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.fail(value=1)
quickpy.run(main())`}))
		qe := requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
		if qe.Script == nil || qe.Script.Type != "ToolError" {
			t.Fatalf("Script = %+v, want type ToolError", qe.Script)
		}
		requireToolCause(t, qe.Tool, "fail", "permission_denied", true)
		eventID := toolEventCallID(t, rec.snapshot(), "fail")
		if qe.Tool.CallID != eventID {
			t.Fatalf("ToolCause.CallID = %q, ToolEvent.CallID = %q; want the same opaque id", qe.Tool.CallID, eventID)
		}
	})

	t.Run("unknown tool", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.missing(x=1)
quickpy.run(main())`}))
		qe := requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
		if qe.Script == nil || qe.Script.Type != "ToolNotFoundError" {
			t.Fatalf("Script = %+v, want type ToolNotFoundError", qe.Script)
		}
		requireToolCause(t, qe.Tool, "missing", "tool_not_found", true)
	})

	t.Run("argument error", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.echo(1)
quickpy.run(main())`}))
		qe := requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
		if qe.Script == nil || qe.Script.Type != "ToolArgumentError" {
			t.Fatalf("Script = %+v, want type ToolArgumentError", qe.Script)
		}
		// Positional arguments are rejected before a call is registered, so
		// there is no call id to report.
		requireToolCause(t, qe.Tool, "echo", "tool_argument", false)
	})

	t.Run("limit error", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolCalls: 1})
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
	})

	t.Run("regular ValueError has no ToolCause", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.set_state({"x": 1})
raise ValueError("boom")`}))
		qe := requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
		if qe.Script == nil || qe.Script.Type != "ValueError" {
			t.Fatalf("Script = %+v, want type ValueError", qe.Script)
		}
		if qe.Tool != nil {
			t.Fatalf("Error.Tool = %+v, want nil for a regular Python exception", qe.Tool)
		}
	})
}

// TestHostFaultAndRecovery covers the three host-side tool failure classes and
// proves the runtime remains healthy for the next execution.
func TestHostFaultAndRecovery(t *testing.T) {
	healthy := func(t *testing.T, r *quickpy.Runtime) {
		t.Helper()
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("healthy")`}))
		requireStatus(t, res, quickpy.StatusCompleted)
		requireContains(t, textOf(res.Outputs), "healthy")
	}

	t.Run("handler panic", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		if err := r.RegisterTool("panic", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			panic("handler boom")
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.panic()
quickpy.run(main())`}))
		requireErrorKind(t, err, quickpy.KindHostFault)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
		healthy(t, r)
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
		healthy(t, r)
	})

	t.Run("oversized result", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxToolResultBytes: 16})
		if err := r.RegisterTool("big", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return strings.Repeat("x", 100), nil
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.big()
quickpy.run(main())`}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
		healthy(t, r)
	})
}
