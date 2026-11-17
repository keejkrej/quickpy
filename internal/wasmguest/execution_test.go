package wasmguest

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func delayTool(delay time.Duration) ToolHandler {
	return func(ctx context.Context, call ToolCall) (any, error) {
		select {
		case <-time.After(delay):
			return call.Args["value"], nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func TestAsyncGatherOutOfOrderPartialResume(t *testing.T) {
	rt := newTestRuntime(t, "async_gather.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "async gather",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(200 * time.Millisecond),
			"b": delayTool(50 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 2,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Text != "A\nB\n" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if len(res.Requests) != 2 || res.Requests[0].ID != 1 || res.Requests[1].ID != 2 {
		t.Fatalf("requests = %+v", res.Requests)
	}
	if !reflect.DeepEqual(res.DispatchOrder, []int64{1, 2}) {
		t.Fatalf("dispatch order = %v", res.DispatchOrder)
	}
	if !reflect.DeepEqual(res.CompletionOrder, []int64{2, 1}) {
		t.Fatalf("completion order = %v", res.CompletionOrder)
	}
	if !reflect.DeepEqual(res.DeliveredOrder, []int64{2, 1}) {
		t.Fatalf("delivered order = %v", res.DeliveredOrder)
	}
	if res.ResumeCount != 2 {
		t.Fatalf("resume count = %d", res.ResumeCount)
	}
	if res.ToolCalls != 2 {
		t.Fatalf("tool calls = %d", res.ToolCalls)
	}
	if res.ExportCalls.Invoke != 3 || res.ExportCalls.Free != 6 {
		t.Fatalf("export calls = %+v", res.ExportCalls)
	}
	if len(res.CallLedger) != 2 {
		t.Fatalf("ledger = %+v", res.CallLedger)
	}
	for _, e := range res.CallLedger {
		if e.State != CallCompleted || !e.Dispatched || !e.Delivered || !e.Completed {
			t.Fatalf("ledger entry = %+v", e)
		}
	}
}

func TestAsyncRunnableResume(t *testing.T) {
	rt := newTestRuntime(t, "async_runnable.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "runnable",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Text != "runnable done\n" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if res.ResumeCount != 1 {
		t.Fatalf("resume count = %d", res.ResumeCount)
	}
	if res.ExportCalls.Invoke != 2 || res.ExportCalls.Free != 4 {
		t.Fatalf("export calls = %+v", res.ExportCalls)
	}
}

func TestAsyncCancelWhileSuspended(t *testing.T) {
	rt := newTestRuntime(t, "async_cancel.wasm")
	cancelled := make(chan struct{})
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "cancel suspended",
		Filename: "main.py",
		Timeout:  100 * time.Millisecond,
		Tools: map[string]ToolHandler{
			"block": func(ctx context.Context, call ToolCall) (any, error) {
				<-ctx.Done()
				close(cancelled)
				return nil, ctx.Err()
			},
		},
	})
	if err == nil {
		t.Fatal("expected timeout")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindTimedOut {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusTimedOut {
		t.Fatalf("result = %+v", res)
	}
	if res.ExportCalls.Invoke != 1 || res.ExportCalls.Free != 2 {
		t.Fatalf("export calls = %+v", res.ExportCalls)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("handler context was not cancelled")
	}
}

func TestAsyncUnknownTool(t *testing.T) {
	rt := newTestRuntime(t, "async_gather.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "unknown tool",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 2,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	var found bool
	for _, e := range res.CallLedger {
		if e.ID == 2 {
			found = true
			if e.Error != "tool_not_found" {
				t.Fatalf("ledger entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("ledger = %+v", res.CallLedger)
	}
}

func TestAsyncDuplicateCallID(t *testing.T) {
	rt := newTestRuntime(t, "async_duplicate.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "duplicate",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncDroppedCall(t *testing.T) {
	rt := newTestRuntime(t, "async_drop.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "drop",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(200 * time.Millisecond),
			"b": delayTool(10 * time.Millisecond),
			"c": delayTool(200 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 3,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncBadRunnable(t *testing.T) {
	rt := newTestRuntime(t, "async_bad_runnable.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "bad runnable",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncRunnableWithPendingCall(t *testing.T) {
	rt := newTestRuntime(t, "async_runnable_pending.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "runnable pending",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(200 * time.Millisecond),
			"b": delayTool(10 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 2,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncStalledTerminal(t *testing.T) {
	rt := newTestRuntime(t, "async_stalled.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "stalled",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected stalled error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindStalled || we.Status != StatusStalled {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusStalled {
		t.Fatalf("result = %+v", res)
	}
	if !res.Stalled {
		t.Fatal("Stalled = false")
	}
	if res.ToolCalls != 0 {
		t.Fatalf("tool calls = %d", res.ToolCalls)
	}
	if res.ExportCalls.Invoke != 1 {
		t.Fatalf("invoke count = %d (want 1; no resume after stall)", res.ExportCalls.Invoke)
	}
	if res.ResumeCount != 0 {
		t.Fatalf("resume count = %d (want 0)", res.ResumeCount)
	}
	if !res.InvokeReturned {
		t.Fatal("InvokeReturned = false")
	}
}

func TestAsyncStalledAfterDelivery(t *testing.T) {
	rt := newTestRuntime(t, "async_stalled_after_delivery.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "stalled after delivery",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
		},
	})
	if err == nil {
		t.Fatal("expected stalled error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindStalled {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusStalled {
		t.Fatalf("result = %+v", res)
	}
	if res.ToolCalls != 1 {
		t.Fatalf("tool calls = %d", res.ToolCalls)
	}
	if res.ExportCalls.Invoke != 2 {
		t.Fatalf("invoke count = %d (want 2)", res.ExportCalls.Invoke)
	}
	if res.ResumeCount != 1 {
		t.Fatalf("resume count = %d (want 1)", res.ResumeCount)
	}
	if len(res.CallLedger) != 1 || res.CallLedger[0].State != CallCompleted || !res.CallLedger[0].Delivered {
		t.Fatalf("ledger = %+v", res.CallLedger)
	}
}

func TestAsyncStalledWithCallsRejected(t *testing.T) {
	rt := newTestRuntime(t, "async_stalled_with_calls.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "bad stalled",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncStalledFalseRejected(t *testing.T) {
	rt := newTestRuntime(t, "async_stalled_false.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "bad stalled",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncAwaitingStalledRejected(t *testing.T) {
	rt := newTestRuntime(t, "async_awaiting_stalled.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "bad awaiting",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncRunnableStalledRejected(t *testing.T) {
	rt := newTestRuntime(t, "async_runnable_stalled.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "bad runnable",
		Filename: "main.py",
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncStalledPendingRejected(t *testing.T) {
	rt := newTestRuntime(t, "async_stalled_pending.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "stalled pending",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(200 * time.Millisecond),
			"b": delayTool(10 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 2,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncMaxConcurrentSerial(t *testing.T) {
	rt := newTestRuntime(t, "async_serial.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "serial",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
			"b": delayTool(10 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 1,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if !reflect.DeepEqual(res.DispatchOrder, []int64{1, 2}) {
		t.Fatalf("dispatch order = %v", res.DispatchOrder)
	}
	if !reflect.DeepEqual(res.CompletionOrder, []int64{1, 2}) {
		t.Fatalf("completion order = %v", res.CompletionOrder)
	}
	if !reflect.DeepEqual(res.DeliveredOrder, []int64{1, 2}) {
		t.Fatalf("delivered order = %v", res.DeliveredOrder)
	}
}

func TestAsyncMaxToolCalls(t *testing.T) {
	rt := newTestRuntime(t, "async_gather.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "limit",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
			"b": delayTool(10 * time.Millisecond),
		},
		MaxToolCalls: 1,
	})
	if err == nil {
		t.Fatal("expected limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncMaxPendingCalls(t *testing.T) {
	rt := newTestRuntime(t, "async_gather.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "limit",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
			"b": delayTool(10 * time.Millisecond),
		},
		MaxPendingCalls: 1,
	})
	if err == nil {
		t.Fatal("expected limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncMaxResumes(t *testing.T) {
	rt := newTestRuntime(t, "async_runnable_loop.wasm")
	_, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:     "resume limit",
		Filename:   "main.py",
		Timeout:    2 * time.Second,
		MaxResumes: 1,
	})
	if err == nil {
		t.Fatal("expected limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncToolPanic(t *testing.T) {
	rt := newTestRuntime(t, "async_serial.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "panic",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": func(ctx context.Context, call ToolCall) (any, error) {
				panic("boom")
			},
			"b": delayTool(10 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 1,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	var found bool
	for _, e := range res.CallLedger {
		if e.ID == 1 {
			found = true
			if e.Error != "handler_panic" {
				t.Fatalf("ledger entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("ledger = %+v", res.CallLedger)
	}
}

func TestAsyncToolResultTooLarge(t *testing.T) {
	rt := newTestRuntime(t, "async_serial.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "large result",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": func(ctx context.Context, call ToolCall) (any, error) {
				return "0123456789ABCDEF", nil
			},
			"b": delayTool(10 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 1,
		MaxToolResultBytes:     10,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	var found bool
	for _, e := range res.CallLedger {
		if e.ID == 1 {
			found = true
			if e.Error != "result_too_large" {
				t.Fatalf("ledger entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("ledger = %+v", res.CallLedger)
	}
}

func TestAsyncDuplicateResultRejected(t *testing.T) {
	o := &executionOwner{
		result: &Result{ExecutionID: "exec-test"},
		calls: map[int64]*callState{
			1: {id: 1, state: CallResponseReady},
		},
	}
	err := o.processResult(toolResult{id: 1, value: "x"})
	if err == nil {
		t.Fatal("expected host fault")
	}
	if err.Kind != KindHostFault {
		t.Fatalf("error = %+v", err)
	}
}
