package integration_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

func TestEventOrderingOutOfOrderGather(t *testing.T) {
	var rec eventRecorder
	releaseA := make(chan struct{})
	afterB := make(chan struct{})
	var afterBOnce sync.Once
	sink := func(ev quickpy.Event) {
		rec.sink(ev)
		if te, ok := ev.(quickpy.TextEvent); ok && strings.Contains(te.Text, "after b") {
			afterBOnce.Do(func() { close(afterB) })
		}
	}
	r := newRuntime(t, quickpy.RuntimeOptions{EventSink: sink})
	if err := r.RegisterTool("a", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		<-releaseA
		return "A", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterTool("b", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "B", nil
	}); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
print("before")
async def child(name, tool, value):
    r = await tool(value=value)
    print("after", name, r)
    return r
async def main():
    a, b = await asyncio.gather(child("a", quickpy.tools.a, 1), child("b", quickpy.tools.b, 2))
    print("done", a, b)
quickpy.run(main())`})
	waitFor(t, afterB, "after b output")
	close(releaseA)
	res := mustWait(t, e)

	events := rec.snapshot()
	want := []quickpy.Event{
		quickpy.TextEvent{Text: "before\n"},
		quickpy.ToolEvent{Phase: "started", Name: "a", CallID: "1"},
		quickpy.ToolEvent{Phase: "started", Name: "b", CallID: "2"},
		quickpy.ToolEvent{Phase: "finished", Name: "b", CallID: "2"},
		quickpy.TextEvent{Text: "after b B\n"},
		quickpy.ToolEvent{Phase: "finished", Name: "a", CallID: "1"},
		// The guest may coalesce adjacent output deltas into one response.
		quickpy.TextEvent{Text: "after a A\ndone A B\n"},
	}
	requireEvents(t, events, want)
	requireEvents(t, res.Outputs, textEvents("before\n", "after b B\n", "after a A\ndone A B\n"))
}

func TestEventSinkOverride(t *testing.T) {
	var runtimeRec, execRec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{EventSink: runtimeRec.sink})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{
		Source: `import asyncio, quickpy
print("hello")
async def main():
    r = await quickpy.tools.echo(value=1)
    print("tool", r)
quickpy.run(main())`,
		EventSink: execRec.sink,
	}))
	if got := runtimeRec.snapshot(); len(got) != 0 {
		t.Fatalf("runtime sink received %d events, want 0: %#v", len(got), got)
	}
	execEvents := execRec.snapshot()
	if len(execEvents) == 0 {
		t.Fatal("execution sink received no events")
	}
	requireEvents(t, outputEvents(execEvents), res.Outputs)
}

func TestToolEventOpaqueIDs(t *testing.T) {
	var rec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{EventSink: rec.sink})
	if err := r.RegisterTool("a", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "A", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterTool("b", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "B", nil
	}); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    a = await quickpy.tools.a(value=1)
    b = await quickpy.tools.b(value=2)
    print(a, b)
quickpy.run(main())`})
	res := mustWait(t, e)
	requireContains(t, textOf(res.Outputs), "A B")

	started := map[string]string{}
	finished := map[string]string{}
	for _, ev := range rec.snapshot() {
		te, ok := ev.(quickpy.ToolEvent)
		if !ok {
			continue
		}
		if te.CallID == "" {
			t.Fatalf("tool event has empty CallID: %#v", te)
		}
		if te.CallID == te.Name || te.CallID == e.ID() {
			t.Fatalf("CallID %q is not opaque (name=%q execution=%q)", te.CallID, te.Name, e.ID())
		}
		switch te.Phase {
		case "started":
			started[te.Name] = te.CallID
		case "finished":
			finished[te.Name] = te.CallID
		default:
			t.Fatalf("unexpected tool phase %q", te.Phase)
		}
	}
	for _, name := range []string{"a", "b"} {
		if started[name] == "" || finished[name] == "" {
			t.Fatalf("missing started/finished for %q: started=%v finished=%v", name, started, finished)
		}
		if started[name] != finished[name] {
			t.Fatalf("CallID changed for %q: started=%q finished=%q", name, started[name], finished[name])
		}
	}
	if started["a"] == started["b"] {
		t.Fatalf("distinct calls share CallID %q", started["a"])
	}
}

func TestUnknownToolCatchable(t *testing.T) {
	var rec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{EventSink: rec.sink})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    try:
        await quickpy.tools.missing(x=1)
    except quickpy.ToolError as e:
        print("missing", e.code)
quickpy.run(main())`}))
	requireContains(t, textOf(res.Outputs), "missing tool_not_found")

	var finished, started bool
	for _, ev := range rec.snapshot() {
		te, ok := ev.(quickpy.ToolEvent)
		if !ok || te.Name != "missing" {
			continue
		}
		switch te.Phase {
		case "started":
			started = true
		case "finished":
			finished = true
			if te.Error != "tool_not_found" {
				t.Fatalf("missing tool finished error = %q, want tool_not_found", te.Error)
			}
		}
	}
	if !finished {
		t.Fatalf("missing tool finished event not observed: %#v", rec.snapshot())
	}
	if started {
		t.Fatalf("unknown tool must not emit a synthetic started event: %#v", rec.snapshot())
	}
}

func TestToolHandlerArgumentsAndDeadline(t *testing.T) {
	type recorded struct {
		call   quickpy.ToolCall
		ctxErr error
	}
	var mu sync.Mutex
	var got recorded
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		mu.Lock()
		got.call = call
		got.ctxErr = ctx.Err()
		mu.Unlock()
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	e := execute(t, r, quickpy.ExecuteRequest{
		Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.echo(value=1, name="x")
    print(r)
quickpy.run(main())`,
		Timeout: 2 * time.Second,
	})
	res := mustWait(t, e)
	requireContains(t, textOf(res.Outputs), "ok")

	mu.Lock()
	call := got.call
	ctxErr := got.ctxErr
	mu.Unlock()
	if call.Name != "echo" {
		t.Fatalf("handler name = %q, want echo", call.Name)
	}
	if call.ExecutionID != e.ID() {
		t.Fatalf("handler execution id = %q, want %q", call.ExecutionID, e.ID())
	}
	if call.Deadline.IsZero() {
		t.Fatal("handler deadline is zero, want the execution deadline")
	}
	if call.Deadline.Before(start) || call.Deadline.After(start.Add(3*time.Second)) {
		t.Fatalf("handler deadline = %v, start = %v", call.Deadline, start)
	}
	if ctxErr != nil {
		t.Fatalf("handler context error = %v, want nil", ctxErr)
	}
	args := argsMap(t, call.Args)
	requireInt64(t, args["value"], 1)
	requireString(t, args["name"], "x")
}

func TestToolExecutionIDAfterValidationFailure(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	// A validation failure must not consume an engine execution ID.
	if _, err := r.Execute(t.Context(), quickpy.ExecuteRequest{Source: "x"}); err == nil {
		t.Fatal("expected validation error")
	}
	var got string
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		got = call.ExecutionID
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await quickpy.tools.echo(value=1)
quickpy.run(main())`})
	mustWait(t, e)
	if got != e.ID() {
		t.Fatalf("handler execution id = %q, public execution id = %q", got, e.ID())
	}
}

func TestToolHandlerErrorCatchable(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("fail", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return nil, &quickpy.ToolError{Code: "permission_denied", Message: "no"}
	}); err != nil {
		t.Fatal(err)
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    try:
        await quickpy.tools.fail(value=1)
    except quickpy.ToolError as e:
        print("caught", e.code, e.message)
quickpy.run(main())`}))
	requireContains(t, textOf(res.Outputs), "caught permission_denied no")
}

func TestToolFloat64Int64Fidelity(t *testing.T) {
	var mu sync.Mutex
	var gotArgs map[string]any
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		args, _ := call.Args.(map[string]any)
		mu.Lock()
		gotArgs = args
		mu.Unlock()
		return map[string]any{
			"i":     int64(1),
			"f":     float64(1.0),
			"n":     1.0000000000000002,
			"small": 0.1,
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.echo(i=1, f=1.0, n=1.0000000000000002, small=0.1)
    print(isinstance(r["i"], int), isinstance(r["f"], float))
    print(r["i"], r["f"], r["n"], r["small"])
    quickpy.set_state(r)
quickpy.run(main())`}))
	requireContains(t, textOf(res.Outputs), "True True")

	mu.Lock()
	args := gotArgs
	mu.Unlock()
	if args == nil {
		t.Fatal("handler did not receive a map[string]any args value")
	}
	requireInt64(t, args["i"], 1)
	requireFloat64(t, args["f"], 1.0)
	requireFloat64(t, args["n"], 1.0000000000000002)
	requireFloat64(t, args["small"], 0.1)
	if args["small"].(float64) == float64(float32(0.1)) {
		t.Fatalf("guest->host small float collapsed to float32: %v", args["small"])
	}

	requireState(t, res, map[string]any{
		"i":     int64(1),
		"f":     float64(1.0),
		"n":     1.0000000000000002,
		"small": 0.1,
	})
	state := res.State.(map[string]any)
	if state["small"].(float64) == float64(float32(0.1)) {
		t.Fatalf("host->guest small float collapsed to float32: %v", state["small"])
	}
}

func TestNoSyntheticConsoleOrImageEvents(t *testing.T) {
	var rec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{EventSink: rec.sink})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
print("hello")
async def main():
    r = await quickpy.tools.echo(value=1)
    print("tool", r)
quickpy.run(main())`}))
	for _, ev := range rec.snapshot() {
		switch ev.(type) {
		case quickpy.ConsoleEvent, quickpy.ImageEvent:
			t.Fatalf("unexpected synthetic event: %#v", ev)
		}
	}
	for _, ev := range res.Outputs {
		switch ev.(type) {
		case quickpy.ConsoleEvent, quickpy.ImageEvent:
			t.Fatalf("unexpected synthetic output: %#v", ev)
		}
	}
	requireContains(t, textOf(res.Outputs), "hello")
	requireContains(t, textOf(res.Outputs), "tool ok")
}
