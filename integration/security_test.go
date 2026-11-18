package integration_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

func TestPerExecutionMemoryLowerRejectedBeforeAdmission(t *testing.T) {
	tool := newBlockingTool()
	t.Cleanup(func() { tool.cleanup(t) })
	const runtimeMemory = int64(32 << 20)
	r := newRuntime(t, quickpy.RuntimeOptions{
		MaxMemoryBytes: runtimeMemory,
		MaxExecutions:  1,
	})
	if err := r.RegisterTool("block", tool.cooperative); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await quickpy.tools.block(value=1)
quickpy.run(main())`})
	waitFor(t, tool.started, "tool start")

	_, err := r.Execute(context.Background(), quickpy.ExecuteRequest{
		Source:         `print("x")`,
		Filename:       "main.py",
		MaxMemoryBytes: runtimeMemory / 2,
	})
	qe := requireErrorKind(t, err, quickpy.KindInvalidRequest)
	requireContains(t, qe.Message, "not supported")

	// The invalid request must not consume capacity: the active execution is
	// still the only one, and a valid request after release succeeds.
	tool.releaseTool()
	mustWait(t, e)
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("ok")`}))
	requireContains(t, textOf(res.Outputs), "ok")
}

func TestCapacityMaxExecutions(t *testing.T) {
	tool := newBlockingTool()
	t.Cleanup(func() { tool.cleanup(t) })
	r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 1})
	if err := r.RegisterTool("block", tool.cooperative); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await quickpy.tools.block(value=1)
quickpy.run(main())`})
	waitFor(t, tool.started, "tool start")

	_, err := r.Execute(context.Background(), quickpy.ExecuteRequest{Source: `print("x")`, Filename: "main.py"})
	requireErrorKind(t, err, quickpy.KindCapacityExceeded)

	tool.releaseTool()
	mustWait(t, e)
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("ok")`}))
	requireContains(t, textOf(res.Outputs), "ok")
}

func TestCapacityReleasedAfterWait(t *testing.T) {
	t.Run("normal completion", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 1})
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("one")`}))
		requireContains(t, textOf(res.Outputs), "one")
		// Immediate Execute after Wait must not observe stale capacity.
		res = mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("two")`}))
		requireContains(t, textOf(res.Outputs), "two")
	})

	t.Run("script error", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 1})
		_, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `raise ValueError("boom")`}))
		requireErrorKind(t, err, quickpy.KindScriptError)
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("after error")`}))
		requireContains(t, textOf(res.Outputs), "after error")
	})

	t.Run("cancel", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 1})
		e, resumed := startLoopingExecution(t, r, nil, "", nil)
		waitFor(t, resumed, "tool finished")
		time.Sleep(20 * time.Millisecond)
		e.Cancel()
		res, err := wait(t, e)
		requireErrorKind(t, err, quickpy.KindCancelled)
		requireStatus(t, res, quickpy.StatusCancelled)
		// Immediate Execute after Wait must not observe stale capacity.
		res = mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("after cancel")`}))
		requireContains(t, textOf(res.Outputs), "after cancel")
	})

	t.Run("timeout", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 1})
		_, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: `while True:
    pass`,
			Timeout: 50 * time.Millisecond,
		}))
		requireErrorKind(t, err, quickpy.KindTimedOut)
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("after timeout")`}))
		requireContains(t, textOf(res.Outputs), "after timeout")
	})

	t.Run("close", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 1})
		mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("one")`}))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		_, err := r.Execute(context.Background(), quickpy.ExecuteRequest{Source: `print("two")`, Filename: "main.py"})
		requireErrorKind(t, err, quickpy.KindRuntimeClosed)
	})
}

func TestGuestImportsAndModules(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("ok")`}))
	if len(res.Guest.Imports) != 0 {
		t.Fatalf("guest imports = %+v, want none", res.Guest.Imports)
	}
	// Language-only modules such as math are allowed; this negative list is
	// about ambient OS/process capabilities, not pure computation.
	for _, module := range []string{"os", "uos", "gc"} {
		t.Run(module, func(t *testing.T) {
			res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
				Source: fmt.Sprintf(`try:
    import %s
    print("imported")
except ImportError:
    print("ImportError")`, module),
			}))
			if err != nil {
				t.Fatalf("execution failed: %v", err)
			}
			if got := textOf(res.Outputs); !strings.Contains(got, "ImportError") {
				t.Fatalf("module %s output = %q, want ImportError", module, got)
			}
		})
	}
}

func TestNoStdoutLeak(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	old := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	restored := false
	restore := func() {
		if !restored {
			os.Stdout = old
			restored = true
		}
	}
	defer restore()

	e, execErr := r.Execute(context.Background(), quickpy.ExecuteRequest{
		Source:   `print("LEAK_MARKER_12345")`,
		Filename: "main.py",
	})
	var res quickpy.Result
	var waitErr error
	if execErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, waitErr = e.Wait(ctx)
		cancel()
	}

	restore()
	_ = pw.Close()
	leaked, _ := io.ReadAll(pr)
	_ = pr.Close()

	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}
	if strings.Contains(string(leaked), "LEAK_MARKER_12345") {
		t.Fatalf("guest stdout leaked to process stdout: %q", leaked)
	}
	if got := textOf(res.Outputs); !strings.Contains(got, "LEAK_MARKER_12345") {
		t.Fatalf("marker missing from result outputs: %q", got)
	}
	for _, ev := range res.Outputs {
		switch ev.(type) {
		case quickpy.ConsoleEvent, quickpy.ImageEvent:
			t.Fatalf("unexpected synthetic output event: %#v", ev)
		}
	}
}
