package wasmguest

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestAsyncMaxToolCallsUnlimited(t *testing.T) {
	rt := newTestRuntime(t, "async_gather.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "unlimited",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(200 * time.Millisecond),
			"b": delayTool(50 * time.Millisecond),
		},
		MaxToolCalls: -1,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted || res.ToolCalls != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestMaxToolCallsZeroLedgerCap(t *testing.T) {
	// The low-level Request zero selects the M0 default, so exercise the
	// inclusive zero cap directly at the ledger boundary.
	o := &executionOwner{
		result:          &Result{ExecutionID: "exec-test"},
		calls:           map[int64]*callState{},
		tombstones:      map[int64]bool{},
		maxToolCalls:    0,
		maxPendingCalls: 64,
	}
	resp := &Response{
		Type:     "suspended",
		Reason:   "awaiting_calls",
		Calls:    []CallRequest{{ID: 1, Name: "a"}},
		Awaiting: []int64{1},
	}
	err := o.validateSuspension(resp)
	if err == nil || err.Kind != KindLimitExceeded {
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
	var obs []Observation
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
		Observer:               func(o Observation) { obs = append(obs, o) },
	})
	if err == nil {
		t.Fatal("expected host fault")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindHostFault {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusGuestFault {
		t.Fatalf("result = %+v", res)
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
	// The finished observation is emitted before the hard error is returned.
	var finished bool
	for _, o := range obs {
		if o.Kind == ObservationTool && o.CallID == 1 && o.Phase == "finished" && o.Error == "handler_panic" {
			finished = true
		}
	}
	if !finished {
		t.Fatalf("observations = %+v", obs)
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
	if err == nil {
		t.Fatal("expected limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusLimitExceeded {
		t.Fatalf("result = %+v", res)
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

func TestAsyncToolInvalidResult(t *testing.T) {
	rt := newTestRuntime(t, "async_serial.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "invalid result",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": func(ctx context.Context, call ToolCall) (any, error) {
				return make(chan int), nil
			},
			"b": delayTool(10 * time.Millisecond),
		},
		MaxConcurrentToolCalls: 1,
	})
	if err == nil {
		t.Fatal("expected protocol error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusProtocolError {
		t.Fatalf("result = %+v", res)
	}
	var found bool
	for _, e := range res.CallLedger {
		if e.ID == 1 {
			found = true
			if e.Error != "invalid_result" {
				t.Fatalf("ledger entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("ledger = %+v", res.CallLedger)
	}
}

func TestAsyncPendingCountExcludesDelivered(t *testing.T) {
	rt := newTestRuntime(t, "async_pending_reuse.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "pending reuse",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
		},
		MaxPendingCalls: 1,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if res.ToolCalls != 2 {
		t.Fatalf("tool calls = %d", res.ToolCalls)
	}
	if len(res.CallLedger) != 2 {
		t.Fatalf("ledger = %+v", res.CallLedger)
	}
	for _, e := range res.CallLedger {
		if e.State != CallCompleted || !e.Delivered || !e.Completed {
			t.Fatalf("ledger entry = %+v", e)
		}
	}
}

func TestOutputClippingBeforeStoreAndObserver(t *testing.T) {
	rt := newTestRuntime(t, "output_deltas.wasm")
	var obs []Observation
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:         "output deltas",
		Filename:       "main.py",
		Timeout:        2 * time.Second,
		MaxOutputBytes: 6,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
			"b": delayTool(10 * time.Millisecond),
		},
		Observer: func(o Observation) { obs = append(obs, o) },
	})
	if err == nil {
		t.Fatal("expected limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusLimitExceeded {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Outputs) != 2 || res.Outputs[0].Text != "AAAA" || res.Outputs[1].Text != "BB" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if !res.OutputTruncated {
		t.Fatal("OutputTruncated = false")
	}
	var texts []string
	for _, o := range obs {
		if o.Kind == ObservationText {
			texts = append(texts, o.Text)
		}
	}
	if !reflect.DeepEqual(texts, []string{"AAAA", "BB"}) {
		t.Fatalf("text observations = %v", texts)
	}
}

func TestObserverToolTransitionsAndDeadline(t *testing.T) {
	rt := newTestRuntime(t, "async_serial.wasm")
	var obs []Observation
	var deadlines []time.Time
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:                 "observer",
		Filename:               "main.py",
		Timeout:                2 * time.Second,
		MaxConcurrentToolCalls: 1,
		Tools: map[string]ToolHandler{
			"a": func(ctx context.Context, call ToolCall) (any, error) {
				deadlines = append(deadlines, call.Deadline)
				return "A", nil
			},
			"b": func(ctx context.Context, call ToolCall) (any, error) {
				deadlines = append(deadlines, call.Deadline)
				return "B", nil
			},
		},
		Observer: func(o Observation) { obs = append(obs, o) },
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	var tools []string
	for _, o := range obs {
		if o.Kind == ObservationTool {
			tools = append(tools, fmt.Sprintf("%s:%d:%s:%s", o.Phase, o.CallID, o.ToolName, o.Error))
		}
	}
	want := []string{"started:1:a:", "finished:1:a:", "started:2:b:", "finished:2:b:"}
	if !reflect.DeepEqual(tools, want) {
		t.Fatalf("tool observations = %v", tools)
	}
	if len(deadlines) != 2 {
		t.Fatalf("deadlines = %v", deadlines)
	}
	for _, d := range deadlines {
		if d.IsZero() {
			t.Fatal("deadline is zero")
		}
	}
	if len(obs) == 0 || obs[len(obs)-1].Kind != ObservationText || obs[len(obs)-1].Text != "A\nB\n" {
		t.Fatalf("last observation = %+v", obs)
	}
}

func TestObserverPanicIsHostFault(t *testing.T) {
	rt := newTestRuntime(t, "completed.wasm")
	res, err := rt.Execute(context.Background(), Request{
		Source:   "observer panic",
		Filename: "main.py",
		Observer: func(o Observation) { panic("observer boom") },
	})
	if err == nil {
		t.Fatal("expected host fault")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindHostFault {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusGuestFault {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Text != "hello from synthetic guest" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
}

func TestTruncateStringUTF8Safe(t *testing.T) {
	// "é" is 2 bytes; a 3-byte cap must not split the second rune.
	got := truncateString("aéé", 3)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateString produced invalid UTF-8: %q", got)
	}
	if got != "aé" {
		t.Fatalf("truncateString = %q, want %q", got, "aé")
	}
	// A cap smaller than the first rune yields an empty valid prefix.
	if got := truncateString("é", 1); got != "" {
		t.Fatalf("truncateString = %q, want empty", got)
	}
	// Short strings are returned unchanged.
	if got := truncateString("abc", 10); got != "abc" {
		t.Fatalf("truncateString = %q", got)
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

func TestStructuredOutputsOrderingAndLegacyPreference(t *testing.T) {
	rt := newTestRuntime(t, "outputs_order.wasm")
	var obs []Observation
	res, err := rt.Execute(context.Background(), Request{
		Source:   "outputs",
		Filename: "main.py",
		Observer: func(o Observation) { obs = append(obs, o) },
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	wantKinds := []ObservationKind{ObservationText, ObservationConsole, ObservationImage, ObservationText}
	if len(obs) != len(wantKinds) {
		t.Fatalf("observations = %+v", obs)
	}
	for i, want := range wantKinds {
		if obs[i].Kind != want {
			t.Fatalf("observation[%d] = %+v, want kind %s", i, obs[i], want)
		}
	}
	if obs[0].Text != "T1" || obs[1].Text != "C1" || obs[1].Level != "info" || obs[3].Text != "T2" {
		t.Fatalf("observations = %+v", obs)
	}
	if obs[2].MIME != "image/png" || !bytes.Equal(obs[2].Data, []byte{0, 1, 2, 3, 4, 0xff}) {
		t.Fatalf("image observation = %+v", obs[2])
	}
	if len(res.Outputs) != 4 {
		t.Fatalf("result outputs = %+v", res.Outputs)
	}
	if res.Outputs[0].Text != "T1" || res.Outputs[1].Level != "info" || res.Outputs[3].Text != "T2" {
		t.Fatalf("result outputs = %+v", res.Outputs)
	}
	if res.Outputs[2].MIME != "image/png" || !bytes.Equal(res.Outputs[2].Data, []byte{0, 1, 2, 3, 4, 0xff}) {
		t.Fatalf("result image = %+v", res.Outputs[2])
	}
	for _, out := range res.Outputs {
		if out.Text == "legacy-text" {
			t.Fatalf("legacy output was double-counted: %+v", res.Outputs)
		}
	}
}

func TestStructuredOutputsPerResumeDeltas(t *testing.T) {
	rt := newTestRuntime(t, "outputs_deltas_array.wasm")
	var obs []Observation
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "deltas",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Observer: func(o Observation) { obs = append(obs, o) },
		Tools: map[string]ToolHandler{
			"a": func(ctx context.Context, call ToolCall) (any, error) { return "A", nil },
			"b": func(ctx context.Context, call ToolCall) (any, error) { return "B", nil },
		},
		MaxConcurrentToolCalls: 1,
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	wantKinds := []ObservationKind{
		ObservationText, ObservationConsole,
		ObservationTool, ObservationTool,
		ObservationImage,
		ObservationTool, ObservationTool,
		ObservationText,
	}
	if len(obs) != len(wantKinds) {
		t.Fatalf("observations = %+v", obs)
	}
	for i, want := range wantKinds {
		if obs[i].Kind != want {
			t.Fatalf("observation[%d] = %+v, want kind %s", i, obs[i], want)
		}
	}
	if obs[0].Text != "A" || obs[1].Text != "a" || obs[1].Level != "debug" {
		t.Fatalf("first delta = %+v", obs[:2])
	}
	if obs[4].MIME != "image/png" || !bytes.Equal(obs[4].Data, []byte{1, 2, 3}) {
		t.Fatalf("image delta = %+v", obs[4])
	}
	if obs[7].Text != "C" {
		t.Fatalf("final delta = %+v", obs[7])
	}
	if len(res.Outputs) != 4 {
		t.Fatalf("result outputs = %+v", res.Outputs)
	}
	for _, out := range res.Outputs {
		if out.Text == "LEGACY1" || out.Text == "LEGACY2" || out.Text == "LEGACY3" {
			t.Fatalf("legacy output was double-counted: %+v", res.Outputs)
		}
	}
}

func TestStructuredImageClippedToRemainingOutput(t *testing.T) {
	rt := newTestRuntime(t, "outputs_image_clip.wasm")
	var obs []Observation
	res, err := rt.Execute(context.Background(), Request{
		Source:         "clip",
		Filename:       "main.py",
		MaxOutputBytes: 10,
		Observer:       func(o Observation) { obs = append(obs, o) },
	})
	if err == nil {
		t.Fatal("expected limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusLimitExceeded {
		t.Fatalf("result = %+v", res)
	}
	if !res.OutputTruncated {
		t.Fatal("OutputTruncated = false")
	}
	if len(res.Outputs) != 2 {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if res.Outputs[0].Text != "12345" {
		t.Fatalf("text = %+v", res.Outputs[0])
	}
	if !bytes.Equal(res.Outputs[1].Data, []byte{0, 1, 2, 3, 4}) {
		t.Fatalf("image prefix = %v", res.Outputs[1].Data)
	}
	if len(obs) != 2 || !bytes.Equal(obs[1].Data, []byte{0, 1, 2, 3, 4}) {
		t.Fatalf("observations = %+v", obs)
	}
}

func TestStructuredImageAloneOverOutputCapClips(t *testing.T) {
	rt := newTestRuntime(t, "outputs_image_oversize.wasm")
	var obs []Observation
	res, err := rt.Execute(context.Background(), Request{
		Source:         "oversize",
		Filename:       "main.py",
		MaxOutputBytes: 10,
		Observer:       func(o Observation) { obs = append(obs, o) },
	})
	if err == nil {
		t.Fatal("expected limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusLimitExceeded {
		t.Fatalf("result = %+v", res)
	}
	if !res.OutputTruncated {
		t.Fatal("OutputTruncated = false")
	}
	want := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if len(res.Outputs) != 1 || !bytes.Equal(res.Outputs[0].Data, want) {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if len(obs) != 1 || obs[0].Kind != ObservationImage || !bytes.Equal(obs[0].Data, want) {
		t.Fatalf("observations = %+v", obs)
	}
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestStructuredImageAloneFitsOutputCap(t *testing.T) {
	rt := newTestRuntime(t, "outputs_image_oversize.wasm")
	res, err := rt.Execute(context.Background(), Request{
		Source:         "fits",
		Filename:       "main.py",
		MaxOutputBytes: 20,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if res.OutputTruncated {
		t.Fatal("OutputTruncated = true")
	}
	want := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	if len(res.Outputs) != 1 || !bytes.Equal(res.Outputs[0].Data, want) {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
}

func TestAccumulateImageOutputCapBoundaries(t *testing.T) {
	image := Output{Type: "image", MIME: "image/png", Data: []byte{1, 2, 3}}
	cases := []struct {
		name      string
		cap       int
		wantData  []byte
		wantLimit bool
	}{
		{name: "zero cap clips empty", cap: 0, wantData: nil, wantLimit: true},
		{name: "one byte prefix", cap: 1, wantData: []byte{1}, wantLimit: true},
		{name: "exact fit", cap: 3, wantData: []byte{1, 2, 3}, wantLimit: false},
		{name: "larger cap", cap: 5, wantData: []byte{1, 2, 3}, wantLimit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &executionOwner{
				result:         &Result{ExecutionID: "exec-test"},
				maxOutputBytes: tc.cap,
			}
			err := o.accumulateOutput(&Response{Outputs: []Output{image}})
			if tc.wantLimit {
				if err == nil || err.Kind != KindLimitExceeded {
					t.Fatalf("error = %v", err)
				}
				if !o.result.OutputTruncated {
					t.Fatal("OutputTruncated = false")
				}
			} else if err != nil {
				t.Fatalf("error = %v", err)
			}
			if len(tc.wantData) == 0 {
				if len(o.result.Outputs) != 0 {
					t.Fatalf("outputs = %+v", o.result.Outputs)
				}
				return
			}
			if len(o.result.Outputs) != 1 || !bytes.Equal(o.result.Outputs[0].Data, tc.wantData) {
				t.Fatalf("outputs = %+v", o.result.Outputs)
			}
		})
	}
}

func TestStructuredImageMessageCapRejection(t *testing.T) {
	// The start message fits, but the 119-byte image response does not.
	rt, err := NewRuntime(context.Background(), Options{
		ArtifactPath:    filepath.Join("testdata", "outputs_image_oversize.wasm"),
		MaxMemoryBytes:  16 << 20,
		MaxMessageBytes: 110,
		MaxOutputBytes:  1 << 20,
		DefaultTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	res, werr := rt.Execute(context.Background(), Request{Source: "x", Filename: "main.py"})
	if werr == nil {
		t.Fatal("expected message cap error")
	}
	we, ok := werr.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", werr)
	}
	if res == nil || res.Status != StatusLimitExceeded {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Outputs) != 0 {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
}

func TestStructuredOutputsFailedRetainsPartial(t *testing.T) {
	rt := newTestRuntime(t, "outputs_failed_partial.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "fail", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected script error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindScriptError {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusFailed {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Outputs) != 2 || res.Outputs[0].Text != "before" || res.Outputs[1].Level != "warn" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestStructuredOutputsStalledRetainsPartial(t *testing.T) {
	rt := newTestRuntime(t, "outputs_stalled_partial.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "stall", Filename: "main.py"})
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
	if len(res.Outputs) != 2 || res.Outputs[0].Text != "stalled-before" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if !bytes.Equal(res.Outputs[1].Data, []byte{1, 2, 3}) {
		t.Fatalf("image = %v", res.Outputs[1].Data)
	}
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestStructuredOutputsInvalidAreProtocolErrors(t *testing.T) {
	cases := []string{
		"outputs_invalid_level.wasm",
		"outputs_invalid_base64.wasm",
		"outputs_invalid_mime.wasm",
		"outputs_unknown_type.wasm",
		"outputs_invalid_utf8.wasm",
	}
	for _, artifact := range cases {
		t.Run(artifact, func(t *testing.T) {
			rt := newTestRuntime(t, artifact)
			res, err := rt.Execute(context.Background(), Request{Source: "bad", Filename: "main.py"})
			if err == nil {
				t.Fatal("expected protocol error")
			}
			we, ok := err.(*Error)
			if !ok || we.Kind != KindProtocolError {
				t.Fatalf("error = %v", err)
			}
			if res == nil || res.Status != StatusProtocolError {
				t.Fatalf("result = %+v", res)
			}
			if len(res.Outputs) != 0 {
				t.Fatalf("outputs = %+v", res.Outputs)
			}
			if res.StateWritten || res.State != nil {
				t.Fatalf("state committed: %+v", res)
			}
		})
	}
}

func TestStructuredOutputsEmptyArrayAndNullFallback(t *testing.T) {
	rt := newTestRuntime(t, "outputs_empty_array.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "empty", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Outputs) != 0 {
		t.Fatalf("outputs = %+v", res.Outputs)
	}

	rt = newTestRuntime(t, "outputs_null_array.wasm")
	res, err = rt.Execute(context.Background(), Request{Source: "null", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Text != "legacy" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
}

func TestABI3Rejected(t *testing.T) {
	rt := newTestRuntime(t, "abi3.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "pass", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(we.Message, "unsupported ABI version 3") {
		t.Fatalf("message = %q", we.Message)
	}
	if res == nil || res.Guest.ABIVersion != 3 {
		t.Fatalf("result = %+v", res)
	}
}

func TestStructuredOutputsCancelledRetainsPartial(t *testing.T) {
	rt := newTestRuntime(t, "outputs_cancel_partial.wasm")
	ctx, cancel := context.WithCancel(context.Background())
	res, err := rt.ExecuteWithTools(ctx, Request{
		Source:   "cancel",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"block": func(ctx context.Context, call ToolCall) (any, error) {
				cancel()
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
	})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindCancelled {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusCancelled {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Outputs) != 2 || res.Outputs[0].Text != "before-cancel" || res.Outputs[1].Level != "info" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}
