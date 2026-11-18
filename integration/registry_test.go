package integration_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/keejkrej/quickpy"
)

func TestRegistrySnapshotStability(t *testing.T) {
	releaseA := make(chan struct{})
	aStarted := make(chan struct{})
	var aOnce sync.Once
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("a", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		aOnce.Do(func() { close(aStarted) })
		<-releaseA
		return "A", nil
	}); err != nil {
		t.Fatal(err)
	}
	source := `import asyncio, quickpy
async def main():
    try:
        r = await quickpy.tools.a(value=1)
        print("a", r)
    except quickpy.ToolError as e:
        print("a", e.code)
    try:
        r = await quickpy.tools.b(value=2)
        print("b", r)
    except quickpy.ToolError as e:
        print("b", e.code)
quickpy.run(main())`

	e1 := execute(t, r, quickpy.ExecuteRequest{Source: source})
	waitFor(t, aStarted, "tool a start")
	if err := r.RegisterTool("b", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "B", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.UnregisterTool("a"); err != nil {
		t.Fatal(err)
	}
	close(releaseA)

	res1 := mustWait(t, e1)
	requireContains(t, textOf(res1.Outputs), "a A")
	requireContains(t, textOf(res1.Outputs), "b tool_not_found")

	// A later execution sees the new registry.
	res2 := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: source}))
	requireContains(t, textOf(res2.Outputs), "a tool_not_found")
	requireContains(t, textOf(res2.Outputs), "b B")
}

func TestToolNamesDeterministic(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	h := func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) { return nil, nil }
	for _, name := range []string{"b", "a", "c"} {
		if err := r.RegisterTool(name, h); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := r.ToolNames(), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolNames = %v, want %v", got, want)
	}
	if err := r.UnregisterTool("b"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.ToolNames(), []string{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolNames = %v, want %v", got, want)
	}
	if err := r.RegisterTool("b", h); err != nil {
		t.Fatal(err)
	}
	if got, want := r.ToolNames(), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolNames = %v, want %v", got, want)
	}
}

func TestToolRetainedAcrossFreshGlobals(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		args, _ := call.Args.(map[string]any)
		return args["value"], nil
	}); err != nil {
		t.Fatal(err)
	}
	res1 := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
x = 1
async def main():
    r = await quickpy.tools.echo(value=x)
    print("echo", r)
quickpy.run(main())`}))
	requireContains(t, textOf(res1.Outputs), "echo 1")

	res2 := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
try:
    print("x", x)
except NameError:
    print("fresh")
async def main():
    r = await quickpy.tools.echo(value=2)
    print("echo", r)
quickpy.run(main())`}))
	requireContains(t, textOf(res2.Outputs), "fresh")
	requireContains(t, textOf(res2.Outputs), "echo 2")
}
