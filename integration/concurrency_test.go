package integration_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

func TestWaitRepeatedAndConcurrent(t *testing.T) {
	tool := newBlockingTool()
	t.Cleanup(func() { tool.cleanup(t) })
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("block", tool.cooperative); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.block(value=1)
    print("done", r)
quickpy.run(main())`})
	waitFor(t, tool.started, "tool start")

	const n = 8
	results := make([]quickpy.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			results[i], errs[i] = e.Wait(ctx)
		}(i)
	}
	tool.releaseTool()
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Wait[%d]: %v", i, errs[i])
		}
		if !reflect.DeepEqual(results[i], results[0]) {
			t.Fatalf("Wait[%d] result differs: %+v vs %+v", i, results[i], results[0])
		}
	}
	res, err := wait(t, e)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, results[0]) {
		t.Fatalf("later Wait result differs: %+v vs %+v", res, results[0])
	}
}

func TestWaitLocalTimeoutDoesNotCancel(t *testing.T) {
	tool := newBlockingTool()
	t.Cleanup(func() { tool.cleanup(t) })
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("block", tool.cooperative); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.block(value=1)
    print("done", r)
quickpy.run(main())`})
	waitFor(t, tool.started, "tool start")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := e.Wait(ctx)
	if !errors.Is(err, quickpy.ErrWaitTimeout) {
		t.Fatalf("Wait = %v, want ErrWaitTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want context.DeadlineExceeded", err)
	}
	if !reflect.DeepEqual(res, quickpy.Result{}) {
		t.Fatalf("wait-local timeout result = %+v, want zero", res)
	}

	tool.releaseTool()
	ctxErr := waitForError(t, tool.ctxErr, "tool context error")
	if ctxErr != nil {
		t.Fatalf("tool context error = %v, want nil (wait-local timeout must not cancel)", ctxErr)
	}
	res, err = wait(t, e)
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, textOf(res.Outputs), "done released")
}

func TestCancelIdempotent(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	e, resumed := startLoopingExecution(t, r, nil, "", nil)
	waitFor(t, resumed, "tool finished")
	time.Sleep(20 * time.Millisecond)
	e.Cancel()
	e.Cancel()
	e.Cancel()
	res, err := wait(t, e)
	requireErrorKind(t, err, quickpy.KindCancelled)
	requireStatus(t, res, quickpy.StatusCancelled)
	e.Cancel() // no-op after completion
}

func TestParentCancel(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	parent, cancel := context.WithCancel(context.Background())
	e, resumed := startLoopingExecution(t, r, parent, "", nil)
	waitFor(t, resumed, "tool finished")
	time.Sleep(20 * time.Millisecond)
	cancel()
	res, err := wait(t, e)
	requireErrorKind(t, err, quickpy.KindCancelled)
	requireStatus(t, res, quickpy.StatusCancelled)
}

func TestSequentialExecutions(t *testing.T) {
	before := runtime.NumGoroutine()
	r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 1})
	const n = 100
	start := time.Now()
	for i := 0; i < n; i++ {
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: fmt.Sprintf(`import quickpy
quickpy.set_state({"i": %d})
print(%d)`, i, i),
		}))
		if got, want := textOf(res.Outputs), fmt.Sprintf("%d\n", i); got != want {
			t.Fatalf("execution %d output = %q, want %q", i, got, want)
		}
		requireState(t, res, map[string]any{"i": int64(i)})
	}
	elapsed := time.Since(start)
	t.Logf("%d sequential executions in %v", n, elapsed)
	if elapsed > 60*time.Second {
		t.Fatalf("%d sequential executions took %v", n, elapsed)
	}
	requireNoGoroutineLeak(t, before)
}

func TestConcurrentDistinctGlobals(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 10})
	const n = 10
	results := make([]quickpy.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := r.Execute(context.Background(), quickpy.ExecuteRequest{
				Source: fmt.Sprintf(`import quickpy
x = %d
quickpy.set_state({"i": %d})
print(x)`, i, i),
				Filename: "main.py",
			})
			if err != nil {
				errs[i] = err
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			results[i], errs[i] = e.Wait(ctx)
		}(i)
	}
	wg.Wait()

	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("execution %d: %v", i, errs[i])
		}
		if got, want := textOf(results[i].Outputs), fmt.Sprintf("%d\n", i); got != want {
			t.Fatalf("execution %d output = %q, want %q", i, got, want)
		}
		requireState(t, results[i], map[string]any{"i": int64(i)})
		if ids[results[i].ExecutionID] {
			t.Fatalf("duplicate execution ID %q", results[i].ExecutionID)
		}
		ids[results[i].ExecutionID] = true
	}
}

func TestRegistryAndContextRace(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 4})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Dynamic registrations and unregistrations.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			name := fmt.Sprintf("dyn_%d", i)
			_ = r.RegisterTool(name, func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
				return nil, nil
			})
			_ = r.UnregisterTool(name)
		}
	}()

	// Concurrent ToolNames readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			names := r.ToolNames()
			found := false
			for _, name := range names {
				if name == "echo" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("stable tool echo missing from ToolNames: %v", names)
				return
			}
		}
	}()

	// Executions with contexts that are cancelled or time out.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				e, err := r.Execute(ctx, quickpy.ExecuteRequest{
					Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.echo(value=1)
    print(r)
quickpy.run(main())`,
					Filename: "main.py",
				})
				if err == nil {
					_, _ = e.Wait(ctx)
				}
				cancel()
			}
		}(i)
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
