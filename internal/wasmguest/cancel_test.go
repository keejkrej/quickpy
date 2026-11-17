package wasmguest

import (
	"context"
	"testing"
	"time"
)

type execResult struct {
	res *Result
	err error
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestExplicitCancelRunningLoopWithOtherExecution(t *testing.T) {
	rt := newTestRuntime(t, "cancel_mixed.wasm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstCh := make(chan execResult, 1)
	go func() {
		res, err := rt.Execute(ctx, Request{Source: "loop", Filename: "main.py"})
		firstCh <- execResult{res: res, err: err}
	}()

	// Let the first instance enter the running loop before starting the other
	// execution on the same compiled artifact/runtime.
	time.Sleep(50 * time.Millisecond)

	other, otherErr := rt.Execute(context.Background(), Request{Source: "other", Filename: "main.py"})
	if otherErr != nil {
		t.Fatalf("other execution: %v", otherErr)
	}
	if other.Status != StatusCompleted {
		t.Fatalf("other status = %q", other.Status)
	}
	if len(other.Outputs) != 1 || other.Outputs[0].Text != "unaffected\n" {
		t.Fatalf("other outputs = %+v", other.Outputs)
	}

	cancelRequested := time.Now()
	cancel()
	var first execResult
	select {
	case first = <-firstCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first execution did not return after cancel")
	}
	latency := time.Since(cancelRequested)

	we, ok := first.err.(*Error)
	if !ok || we.Kind != KindCancelled || we.Status != StatusCancelled {
		t.Fatalf("first error = %v", first.err)
	}
	if first.res == nil || first.res.Status != StatusCancelled {
		t.Fatalf("first result = %+v", first.res)
	}
	if !first.res.InvokeReturned {
		t.Fatal("first InvokeReturned = false")
	}
	if !first.res.SkippedFreeOnCancel {
		t.Fatal("first SkippedFreeOnCancel = false")
	}
	if first.res.ExportCalls.Invoke != 1 {
		t.Fatalf("first invoke count = %d (want 1)", first.res.ExportCalls.Invoke)
	}
	if first.res.ExportCalls.Free != 0 {
		t.Fatalf("first free count = %d (want 0; no post-cancel free)", first.res.ExportCalls.Free)
	}
	if first.res.ExecutionID == other.ExecutionID || first.res.InstanceID == other.InstanceID {
		t.Fatalf("executions not distinct: first=%s/%s other=%s/%s",
			first.res.ExecutionID, first.res.InstanceID, other.ExecutionID, other.InstanceID)
	}
	if latency >= 250*time.Millisecond {
		t.Fatalf("cancel latency = %v (want < 250ms)", latency)
	}
}

func TestExplicitCancelSuspendedToolWithOtherExecution(t *testing.T) {
	rt := newTestRuntime(t, "cancel_mixed.wasm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlerStarted := make(chan struct{})
	handlerCancelled := make(chan struct{})
	tools := map[string]ToolHandler{
		"block": func(ctx context.Context, call ToolCall) (any, error) {
			close(handlerStarted)
			<-ctx.Done()
			close(handlerCancelled)
			return nil, ctx.Err()
		},
	}

	firstCh := make(chan execResult, 1)
	go func() {
		res, err := rt.ExecuteWithTools(ctx, Request{
			Source:   "block",
			Filename: "main.py",
			Tools:    tools,
		})
		firstCh <- execResult{res: res, err: err}
	}()

	waitFor(t, handlerStarted, "block handler start")

	other, otherErr := rt.Execute(context.Background(), Request{Source: "other", Filename: "main.py"})
	if otherErr != nil {
		t.Fatalf("other execution: %v", otherErr)
	}
	if other.Status != StatusCompleted {
		t.Fatalf("other status = %q", other.Status)
	}
	if len(other.Outputs) != 1 || other.Outputs[0].Text != "unaffected\n" {
		t.Fatalf("other outputs = %+v", other.Outputs)
	}

	cancelRequested := time.Now()
	cancel()
	var first execResult
	select {
	case first = <-firstCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first execution did not return after cancel")
	}
	latency := time.Since(cancelRequested)
	waitFor(t, handlerCancelled, "block handler cancellation")

	we, ok := first.err.(*Error)
	if !ok || we.Kind != KindCancelled || we.Status != StatusCancelled {
		t.Fatalf("first error = %v", first.err)
	}
	if first.res == nil || first.res.Status != StatusCancelled {
		t.Fatalf("first result = %+v", first.res)
	}
	if !first.res.InvokeReturned {
		t.Fatal("first InvokeReturned = false")
	}
	if first.res.SkippedFreeOnCancel {
		t.Fatal("first SkippedFreeOnCancel = true; expected cancellation while suspended")
	}
	if first.res.ExportCalls.Invoke != 1 {
		t.Fatalf("first invoke count = %d (want 1)", first.res.ExportCalls.Invoke)
	}
	if first.res.ExportCalls.Free != 2 {
		t.Fatalf("first free count = %d (want 2 for the start invoke)", first.res.ExportCalls.Free)
	}
	if first.res.ToolCalls != 1 {
		t.Fatalf("first tool calls = %d (want 1)", first.res.ToolCalls)
	}
	if len(first.res.CallLedger) != 1 {
		t.Fatalf("first ledger = %+v", first.res.CallLedger)
	}
	entry := first.res.CallLedger[0]
	if entry.State != CallDispatched || !entry.Dispatched || entry.Completed || entry.Delivered {
		t.Fatalf("first ledger entry = %+v", entry)
	}
	if first.res.ExecutionID == other.ExecutionID || first.res.InstanceID == other.InstanceID {
		t.Fatalf("executions not distinct: first=%s/%s other=%s/%s",
			first.res.ExecutionID, first.res.InstanceID, other.ExecutionID, other.InstanceID)
	}
	if latency >= 250*time.Millisecond {
		t.Fatalf("cancel latency = %v (want < 250ms)", latency)
	}
}
