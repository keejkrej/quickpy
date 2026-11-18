package quickpy

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/keejkrej/quickpy/internal/wasmguest"
)

func TestConvertToolsPreservesToolError(t *testing.T) {
	h := func(ctx context.Context, call ToolCall) (Value, error) {
		return nil, &ToolError{Code: "permission_denied", Message: "no"}
	}
	converted := convertTools(map[string]ToolHandler{"t": h})
	_, err := converted["t"](context.Background(), wasmguest.ToolCall{Name: "t"})
	var te *wasmguest.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("got %T %v, want *wasmguest.ToolError", err, err)
	}
	if te.Code != "permission_denied" || te.Message != "no" {
		t.Fatalf("tool error = %+v", te)
	}
}

func TestConvertToolsPassesThroughError(t *testing.T) {
	want := errors.New("boom")
	h := func(ctx context.Context, call ToolCall) (Value, error) { return nil, want }
	converted := convertTools(map[string]ToolHandler{"t": h})
	_, err := converted["t"](context.Background(), wasmguest.ToolCall{Name: "t"})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestConvertToolsMapsCall(t *testing.T) {
	var got ToolCall
	h := func(ctx context.Context, call ToolCall) (Value, error) {
		got = call
		return "ok", nil
	}
	converted := convertTools(map[string]ToolHandler{"t": h})
	deadline := time.Now().Add(time.Second)
	v, err := converted["t"](context.Background(), wasmguest.ToolCall{
		Name:        "t",
		Args:        map[string]any{"x": int64(1)},
		ExecutionID: "exec-1",
		Deadline:    deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != "ok" {
		t.Fatalf("value = %#v", v)
	}
	if got.Name != "t" || got.ExecutionID != "exec-1" || !got.Deadline.Equal(deadline) {
		t.Fatalf("call = %+v", got)
	}
	if !reflect.DeepEqual(got.Args, map[string]any{"x": int64(1)}) {
		t.Fatalf("args = %#v", got.Args)
	}
}

func TestMapStatus(t *testing.T) {
	tests := []struct {
		in   wasmguest.Status
		want Status
	}{
		{wasmguest.StatusCompleted, StatusCompleted},
		{wasmguest.StatusFailed, StatusFailed},
		{wasmguest.StatusCancelled, StatusCancelled},
		{wasmguest.StatusTimedOut, StatusTimedOut},
		{wasmguest.StatusStalled, StatusStalled},
		{wasmguest.StatusLimitExceeded, StatusLimitExceeded},
		{wasmguest.StatusGuestFault, StatusFailed},
		{wasmguest.StatusProtocolError, StatusFailed},
		{"", ""},
	}
	for _, tt := range tests {
		if got := mapStatus(tt.in); got != tt.want {
			t.Fatalf("mapStatus(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMapKind(t *testing.T) {
	tests := []struct {
		in   wasmguest.Kind
		want Kind
	}{
		{wasmguest.KindInvalidRequest, KindInvalidRequest},
		{wasmguest.KindProtocolError, KindProtocolError},
		{wasmguest.KindScriptError, KindScriptError},
		{wasmguest.KindGuestFault, KindGuestFault},
		{wasmguest.KindLimitExceeded, KindLimitExceeded},
		{wasmguest.KindCancelled, KindCancelled},
		{wasmguest.KindTimedOut, KindTimedOut},
		{wasmguest.KindStalled, KindStalled},
		{wasmguest.KindHostFault, KindHostFault},
		{wasmguest.KindRuntimeClosed, KindRuntimeClosed},
		{"unknown", KindHostFault},
	}
	for _, tt := range tests {
		if got := mapKind(tt.in); got != tt.want {
			t.Fatalf("mapKind(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMapWasmErrorScriptInfo(t *testing.T) {
	we := &wasmguest.Error{
		Kind:      wasmguest.KindScriptError,
		Status:    wasmguest.StatusFailed,
		Op:        "execute",
		Message:   "guest reported failure",
		Exception: &wasmguest.Exception{Type: "ValueError", Message: "bad", Traceback: "tb", Filename: "x.py", Line: 3},
	}
	got := mapWasmError(we, "exec-1")
	if got.Kind != KindScriptError || got.Status != StatusFailed {
		t.Fatalf("error = %+v", got)
	}
	if got.Script == nil || got.Script.Type != "ValueError" || got.Script.Line != 3 {
		t.Fatalf("script = %+v", got.Script)
	}
	if got.ExecutionID != "exec-1" {
		t.Fatalf("execution id = %q", got.ExecutionID)
	}
	if got.Tool != nil {
		t.Fatalf("tool cause = %+v, want nil", got.Tool)
	}
}

func TestMapWasmErrorToolCause(t *testing.T) {
	callID := int64(7)
	we := &wasmguest.Error{
		Kind:      wasmguest.KindScriptError,
		Status:    wasmguest.StatusFailed,
		Op:        "execute",
		Message:   "guest reported failure",
		Exception: &wasmguest.Exception{Type: "ToolError", Message: "tool failed"},
		ToolCause: &wasmguest.ToolCause{Code: "permission_denied", Message: "no", Tool: "search", CallID: &callID},
	}
	got := mapWasmError(we, "exec-1")
	if got.Tool == nil {
		t.Fatal("Tool = nil")
	}
	if got.Tool.Code != "permission_denied" || got.Tool.Message != "no" || got.Tool.Tool != "search" || got.Tool.CallID != "7" {
		t.Fatalf("Tool = %+v", got.Tool)
	}
}

func TestMapGuestMetadata(t *testing.T) {
	got := mapGuest(wasmguest.GuestInfo{
		Engine:          "micropython",
		Version:         "1.29.0",
		ProtocolVersion: "0",
		ABIVersion:      4,
	})
	if got.Engine != "micropython" || got.Version != "1.29.0" || got.ProtocolVersion != "0" || got.ABIVersion != 4 {
		t.Fatalf("guest = %+v", got)
	}

	// An artifact that omits identity metadata must not look complete.
	got = mapGuest(wasmguest.GuestInfo{ABIVersion: 4})
	if got.Engine != "micropython" || got.Version != "unknown" || got.ProtocolVersion != "0" {
		t.Fatalf("default guest = %+v", got)
	}
}

func TestWasmEnginePreservesPartialResultOnError(t *testing.T) {
	rt, err := wasmguest.NewRuntime(context.Background(), wasmguest.Options{
		ArtifactPath:    filepath.Join("internal", "wasmguest", "testdata", "async_gather.wasm"),
		MaxMemoryBytes:  16 << 20,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  1 << 20,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background())

	eng := &wasmEngine{rt: rt}
	res, err := eng.Execute(context.Background(), engineRequest{
		Source:   "limit",
		Filename: "main.py",
		Timeout:  2 * time.Second,
		Tools: map[string]ToolHandler{
			"a": func(ctx context.Context, call ToolCall) (Value, error) { return "A", nil },
			"b": func(ctx context.Context, call ToolCall) (Value, error) { return "B", nil },
		},
		MaxConcurrentToolCalls: 2,
		MaxToolCalls:           1,
	})
	if err == nil {
		t.Fatal("expected limit error")
	}
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
	if res.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", res.ToolCalls)
	}
	if res.Guest.ABIVersion != 4 || res.Guest.Engine != "micropython" {
		t.Fatalf("guest = %+v", res.Guest)
	}
}
