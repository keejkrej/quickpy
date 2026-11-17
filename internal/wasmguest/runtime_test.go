package wasmguest

import (
	"context"
	"errors"
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
	if a.Guest.ABIVersion != 1 {
		t.Fatalf("ABI version = %d", a.Guest.ABIVersion)
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
	if !strings.Contains(we.Message, "ABI version") {
		t.Fatalf("message = %q", we.Message)
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
