package wasmguest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStateValueCommit(t *testing.T) {
	rt := newTestRuntime(t, "state_value.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "state", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.StateWritten {
		t.Fatal("StateWritten = false")
	}
	want := map[string]any{
		"a": int64(1),
		"b": []any{true, float64(1), "x"},
		"c": nil,
	}
	if !reflect.DeepEqual(res.State, want) {
		t.Fatalf("State = %#v", res.State)
	}
}

func TestStateNullCommit(t *testing.T) {
	rt := newTestRuntime(t, "state_null.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "state", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.StateWritten {
		t.Fatal("StateWritten = false")
	}
	if res.State != nil {
		t.Fatalf("State = %#v", res.State)
	}
}

func TestStateNoWrite(t *testing.T) {
	rt := newTestRuntime(t, "state_false.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "state", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.StateWritten {
		t.Fatal("StateWritten = true")
	}
	if res.State != nil {
		t.Fatalf("State = %#v", res.State)
	}
}

func TestStateOmittedNoWrite(t *testing.T) {
	rt := newTestRuntime(t, "completed.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "state", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.StateWritten {
		t.Fatal("StateWritten = true")
	}
	if res.State != nil {
		t.Fatalf("State = %#v", res.State)
	}
}

func TestStateMissingRejected(t *testing.T) {
	rt := newTestRuntime(t, "state_missing.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "state", Filename: "main.py"})
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
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestStateOutOfRangeRejected(t *testing.T) {
	rt := newTestRuntime(t, "state_out_of_range.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "state", Filename: "main.py"})
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
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestStateOversizeRejected(t *testing.T) {
	rt := newTestRuntime(t, "state_oversize.wasm")
	res, err := rt.Execute(context.Background(), Request{
		Source:        "state",
		Filename:      "main.py",
		MaxStateBytes: 10,
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
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestStateOnFailedIgnored(t *testing.T) {
	rt := newTestRuntime(t, "state_on_failed.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "state", Filename: "main.py"})
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
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestStateOnSuspendedIgnored(t *testing.T) {
	rt := newTestRuntime(t, "state_on_suspended.wasm")
	res, err := rt.ExecuteWithTools(context.Background(), Request{
		Source:   "state",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": delayTool(10 * time.Millisecond),
		},
	})
	if err != nil {
		t.Fatalf("ExecuteWithTools: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestCompletedWithErrorRejected(t *testing.T) {
	// A completed response that also carries an error object is a protocol
	// violation: it must never be surfaced as a successful completion, and its
	// state must never be committed.
	rt := newTestRuntime(t, "completed_with_error.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "x", Filename: "main.py"})
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
	if res.StateWritten || res.State != nil {
		t.Fatalf("state committed: %+v", res)
	}
}

func TestInitialStateEncoding(t *testing.T) {
	raw, werr := encodeInitialState(nil, 1024, "exec-test")
	if werr != nil {
		t.Fatalf("encodeInitialState(nil): %v", werr)
	}
	if string(raw) != "null" {
		t.Fatalf("raw = %s", raw)
	}

	raw, werr = encodeInitialState(map[string]any{
		"a": int64(1),
		"b": []any{true, float64(1)},
	}, 1024, "exec-test")
	if werr != nil {
		t.Fatalf("encodeInitialState: %v", werr)
	}
	if string(raw) != `{"a":1,"b":[true,1.0]}` {
		t.Fatalf("raw = %s", raw)
	}

	_, werr = encodeInitialState(make(chan int), 1024, "exec-test")
	if werr == nil || werr.Kind != KindInvalidRequest {
		t.Fatalf("error = %v", werr)
	}

	_, werr = encodeInitialState("0123456789", 4, "exec-test")
	if werr == nil || werr.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", werr)
	}
}

func TestStartMessageInitialState(t *testing.T) {
	raw, werr := encodeInitialState(map[string]any{"a": int64(1)}, 1024, "exec-test")
	if werr != nil {
		t.Fatalf("encodeInitialState: %v", werr)
	}
	b, err := json.Marshal(startMessage{
		V:            0,
		Type:         "start",
		Source:       "s",
		Filename:     "f",
		InitialState: raw,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"initial_state":{"a":1}`) {
		t.Fatalf("start message = %s", b)
	}

	raw, werr = encodeInitialState(nil, 1024, "exec-test")
	if werr != nil {
		t.Fatalf("encodeInitialState(nil): %v", werr)
	}
	b, err = json.Marshal(startMessage{V: 0, Type: "start", Source: "s", Filename: "f", InitialState: raw})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"initial_state":null`) {
		t.Fatalf("start message = %s", b)
	}
}
