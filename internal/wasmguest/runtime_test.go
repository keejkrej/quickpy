package wasmguest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestRuntime(t *testing.T, artifact string) *Runtime {
	t.Helper()
	rt, err := NewRuntime(context.Background(), Options{
		ArtifactPath:    filepath.Join("testdata", artifact),
		MaxMemoryBytes:  16 << 20,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  1 << 20,
		DefaultTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRuntime(%s): %v", artifact, err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

func TestExecuteCompleted(t *testing.T) {
	rt := newTestRuntime(t, "completed.wasm")
	if got := rt.Imports(); len(got) != 0 {
		t.Fatalf("imports = %v", got)
	}

	a, err := rt.Execute(context.Background(), Request{Source: `print("a")`, Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute a: %v", err)
	}
	b, err := rt.Execute(context.Background(), Request{Source: `print("b")`, Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute b: %v", err)
	}

	if a.Status != StatusCompleted || b.Status != StatusCompleted {
		t.Fatalf("statuses = %q, %q", a.Status, b.Status)
	}
	if a.ExecutionID == b.ExecutionID {
		t.Fatalf("execution IDs reused: %q", a.ExecutionID)
	}
	if a.InstanceID == b.InstanceID {
		t.Fatalf("instance IDs reused: %q", a.InstanceID)
	}
	if a.Guest.ABIVersion != 4 {
		t.Fatalf("ABI version = %d", a.Guest.ABIVersion)
	}
	if a.Guest.Engine != "micropython" || a.Guest.Version != "1.29.0" || a.Guest.ProtocolVersion != "0" {
		t.Fatalf("guest identity = %+v", a.Guest)
	}
	if a.StateWritten || a.State != nil {
		t.Fatalf("state committed without a write: written=%v state=%#v", a.StateWritten, a.State)
	}
	if a.MemoryPages == 0 {
		t.Fatal("memory pages = 0")
	}
	if len(a.Outputs) != 1 || a.Outputs[0].Text != "hello from synthetic guest" {
		t.Fatalf("outputs = %+v", a.Outputs)
	}
	if !a.InvokeReturned {
		t.Fatal("InvokeReturned = false")
	}
	if a.ExportCalls.ABI != 1 || a.ExportCalls.Alloc != 1 || a.ExportCalls.Invoke != 1 || a.ExportCalls.Free != 2 {
		t.Fatalf("export calls = %+v", a.ExportCalls)
	}
}

func TestExecuteConcurrent(t *testing.T) {
	rt := newTestRuntime(t, "completed.wasm")

	var wg sync.WaitGroup
	results := make([]*Result, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = rt.Execute(context.Background(), Request{
				Source:   `print("concurrent")`,
				Filename: "main.py",
			})
		}(i)
	}
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("Execute %d: %v", i, errs[i])
		}
		if results[i].Status != StatusCompleted {
			t.Fatalf("status %d = %q", i, results[i].Status)
		}
	}
	if results[0].ExecutionID == results[1].ExecutionID {
		t.Fatal("concurrent executions reused an execution ID")
	}
	if results[0].InstanceID == results[1].InstanceID {
		t.Fatal("concurrent executions reused an instance ID")
	}
}

func TestExecuteFailed(t *testing.T) {
	rt := newTestRuntime(t, "failed.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: `raise ValueError("boom")`, Filename: "main.py"})
	if err == nil {
		t.Fatal("expected error")
	}
	we, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if we.Kind != KindScriptError || we.Status != StatusFailed {
		t.Fatalf("error = %+v", we)
	}
	if res == nil || res.Status != StatusFailed {
		t.Fatalf("result = %+v", res)
	}
	if res.Exception == nil || res.Exception.Type != "ValueError" || res.Exception.Line != 3 {
		t.Fatalf("exception = %+v", res.Exception)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Text != "before failure" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
}

func TestExecuteTimeoutInfiniteLoop(t *testing.T) {
	rt := newTestRuntime(t, "loop.wasm")
	start := time.Now()
	res, err := rt.Execute(context.Background(), Request{
		Source:   "while True:\n    pass\n",
		Filename: "main.py",
		Timeout:  100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout")
	}
	we, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if we.Kind != KindTimedOut || we.Status != StatusTimedOut {
		t.Fatalf("error = %+v", we)
	}
	if res == nil || res.Status != StatusTimedOut {
		t.Fatalf("result = %+v", res)
	}
	if !res.InvokeReturned {
		t.Fatal("active entry did not return")
	}
	if !res.SkippedFreeOnCancel {
		t.Fatal("host did not record skipped guest exports after force-close")
	}
	if res.ExportCalls.Invoke != 1 || res.ExportCalls.Free != 0 {
		t.Fatalf("export calls = %+v", res.ExportCalls)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestExecuteCancelled(t *testing.T) {
	rt := newTestRuntime(t, "loop.wasm")
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel(errors.New("test cancel"))
	}()

	res, err := rt.Execute(ctx, Request{
		Source:   "while True:\n    pass\n",
		Filename: "main.py",
		Timeout:  5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	we, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if we.Kind != KindCancelled || we.Status != StatusCancelled {
		t.Fatalf("error = %+v", we)
	}
	if res == nil || res.Status != StatusCancelled {
		t.Fatalf("result = %+v", res)
	}
	if !res.InvokeReturned || !res.SkippedFreeOnCancel {
		t.Fatalf("cancellation evidence = invoke_returned:%v skipped_free:%v", res.InvokeReturned, res.SkippedFreeOnCancel)
	}
	if res.ExportCalls.Invoke != 1 || res.ExportCalls.Free != 0 {
		t.Fatalf("export calls = %+v", res.ExportCalls)
	}
}

func TestABIVersionMismatch(t *testing.T) {
	rt := newTestRuntime(t, "badversion.wasm")
	_, err := rt.Execute(context.Background(), Request{Source: "pass", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(we.Message, "unsupported ABI version 5") {
		t.Fatalf("message = %q", we.Message)
	}
}

func TestABIOldVersionRejected(t *testing.T) {
	rt := newTestRuntime(t, "oldversion.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "pass", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(we.Message, "unsupported ABI version 2") {
		t.Fatalf("message = %q", we.Message)
	}
	if res == nil || res.Guest.ABIVersion != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestCompileMemoryLimitIsLimitExceeded(t *testing.T) {
	// The fixture declares two initial pages. A one-page host cap is a
	// host-configured memory limit that wazero reports at compile time, so it
	// is safe to classify as LimitExceeded.
	_, err := NewRuntime(context.Background(), Options{
		ArtifactPath:    filepath.Join("testdata", "memory_limit.wasm"),
		MaxMemoryBytes:  65536,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  1 << 20,
	})
	if err == nil {
		t.Fatal("expected memory limit error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindLimitExceeded {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(we.Message, "over limit of") {
		t.Fatalf("message = %q", we.Message)
	}
}

func TestMemoryGrowFailureIsScriptError(t *testing.T) {
	// wazero returns -1 from memory.grow instead of trapping. The guest then
	// reports MemoryError. The host must not fake a LimitExceeded
	// classification for a guest-visible allocation failure.
	rt := newTestRuntime(t, "memory_grow_fail.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "grow", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected script error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindScriptError {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Exception == nil || res.Exception.Type != "MemoryError" {
		t.Fatalf("result = %+v", res)
	}
}

func TestArbitraryMemoryTrapIsGuestFault(t *testing.T) {
	rt := newTestRuntime(t, "trap_oob.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "oob", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected guest fault")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindGuestFault {
		t.Fatalf("error = %v", err)
	}
	if res == nil || res.Status != StatusGuestFault {
		t.Fatalf("result = %+v", res)
	}
}

func TestFailedToolCause(t *testing.T) {
	rt := newTestRuntime(t, "failed_tool_cause.wasm")
	res, err := rt.Execute(context.Background(), Request{Source: "tool", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected script error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindScriptError {
		t.Fatalf("error = %v", err)
	}
	if we.ToolCause == nil {
		t.Fatal("ToolCause = nil")
	}
	if we.ToolCause.Code != "permission_denied" || we.ToolCause.Message != "no" || we.ToolCause.Tool != "search" {
		t.Fatalf("ToolCause = %+v", we.ToolCause)
	}
	if we.ToolCause.CallID == nil || *we.ToolCause.CallID != 7 {
		t.Fatalf("ToolCause.CallID = %v", we.ToolCause.CallID)
	}
	if res == nil || res.Exception == nil || res.Exception.ToolCause == nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestFailedWithoutToolCause(t *testing.T) {
	rt := newTestRuntime(t, "failed.wasm")
	_, err := rt.Execute(context.Background(), Request{Source: "raise", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected script error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindScriptError {
		t.Fatalf("error = %v", err)
	}
	if we.ToolCause != nil {
		t.Fatalf("ToolCause = %+v, want nil", we.ToolCause)
	}
}

func TestMalformedResponse(t *testing.T) {
	rt := newTestRuntime(t, "malformed.wasm")
	_, err := rt.Execute(context.Background(), Request{Source: "pass", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
}

func TestImportAllowlistIsEmpty(t *testing.T) {
	if len(allowedWASIFuncs) != 0 {
		t.Fatalf("allowedWASIFuncs = %v (want empty)", allowedWASIFuncs)
	}
}

func TestImportAllowlistRejectsWASIImports(t *testing.T) {
	cases := []struct {
		artifact string
		imported string
	}{
		{"import_path_open.wasm", "path_open"},
		{"import_environ_get.wasm", "environ_get"},
		{"import_args_get.wasm", "args_get"},
		{"import_poll_oneoff.wasm", "poll_oneoff"},
	}
	for _, tc := range cases {
		t.Run(tc.imported, func(t *testing.T) {
			_, err := NewRuntime(context.Background(), Options{
				ArtifactPath:    filepath.Join("testdata", tc.artifact),
				MaxMemoryBytes:  16 << 20,
				MaxMessageBytes: 1 << 20,
				MaxOutputBytes:  1 << 20,
			})
			if err == nil {
				t.Fatal("expected import rejection")
			}
			we, ok := err.(*Error)
			if !ok || we.Kind != KindProtocolError {
				t.Fatalf("error = %v", err)
			}
			if !strings.Contains(we.Message, tc.imported) {
				t.Fatalf("message = %q", we.Message)
			}
		})
	}
}

func TestExecuteAfterClose(t *testing.T) {
	rt := newTestRuntime(t, "completed.wasm")
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := rt.Execute(context.Background(), Request{Source: "pass", Filename: "main.py"})
	if err == nil {
		t.Fatal("expected error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindRuntimeClosed {
		t.Fatalf("error = %v", err)
	}
}

func TestNewRuntimeFromBytes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "completed.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	rt, err := NewRuntime(context.Background(), Options{
		ArtifactBytes:   data,
		MaxMemoryBytes:  16 << 20,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  1 << 20,
		DefaultTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRuntime from bytes: %v", err)
	}
	defer rt.Close(context.Background())

	res, err := rt.Execute(context.Background(), Request{Source: `print("a")`, Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Guest.ABIVersion != 4 || res.Guest.Engine != "micropython" || res.Guest.Version != "1.29.0" {
		t.Fatalf("guest identity = %+v", res.Guest)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Text != "hello from synthetic guest" {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
}

func TestNewRuntimeFromBytesDoesNotRetainSlice(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "completed.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	rt, err := NewRuntime(context.Background(), Options{
		ArtifactBytes:   data,
		MaxMemoryBytes:  16 << 20,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  1 << 20,
	})
	if err != nil {
		t.Fatalf("NewRuntime from bytes: %v", err)
	}
	defer rt.Close(context.Background())

	// Compilation is complete, so mutating the caller's slice must not affect
	// later executions.
	for i := range data {
		data[i] = 0
	}
	res, err := rt.Execute(context.Background(), Request{Source: "pass", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute after caller mutation: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestNewRuntimeArtifactSourceValidation(t *testing.T) {
	valid := []byte{0x00, 0x61, 0x73, 0x6d}
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "neither",
			opts: Options{MaxMemoryBytes: 1, MaxMessageBytes: 1, MaxOutputBytes: 1},
			want: "artifact path or bytes is required",
		},
		{
			name: "both",
			opts: Options{ArtifactPath: "x.wasm", ArtifactBytes: valid, MaxMemoryBytes: 1, MaxMessageBytes: 1, MaxOutputBytes: 1},
			want: "artifact must be either a path or bytes, not both",
		},
		{
			name: "empty bytes",
			opts: Options{ArtifactBytes: []byte{}, MaxMemoryBytes: 1, MaxMessageBytes: 1, MaxOutputBytes: 1},
			want: "artifact bytes are empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRuntime(context.Background(), tt.opts)
			if err == nil {
				t.Fatal("expected error")
			}
			we, ok := err.(*Error)
			if !ok || we.Kind != KindInvalidRequest {
				t.Fatalf("error = %v", err)
			}
			if !strings.Contains(we.Message, tt.want) {
				t.Fatalf("message = %q, want %q", we.Message, tt.want)
			}
		})
	}
}

func TestNewRuntimeFromBytesInvalidArtifact(t *testing.T) {
	_, err := NewRuntime(context.Background(), Options{
		ArtifactBytes:   []byte("not a wasm module"),
		MaxMemoryBytes:  16 << 20,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  1 << 20,
	})
	if err == nil {
		t.Fatal("expected compile error")
	}
	we, ok := err.(*Error)
	if !ok || we.Kind != KindProtocolError {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(we.Message, "compile artifact") {
		t.Fatalf("message = %q", we.Message)
	}
}

func TestNewRuntimePathWithEmptyBytesUsesPath(t *testing.T) {
	// An empty non-nil byte slice is not a second artifact form; the path is
	// authoritative. This matches the public layer's len-based validation.
	rt, err := NewRuntime(context.Background(), Options{
		ArtifactPath:    filepath.Join("testdata", "completed.wasm"),
		ArtifactBytes:   []byte{},
		MaxMemoryBytes:  16 << 20,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  1 << 20,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background())
}
