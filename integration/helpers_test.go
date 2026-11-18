package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

const artifactEnv = "QUICKPY_TEST_ARTIFACT"

// repoRoot returns the repository root from this test file's location. The
// integration package lives one directory below the root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(file))
}

// artifactPath resolves QUICKPY_TEST_ARTIFACT. It skips when the gate is
// absent and resolves relative paths from the repository root.
func artifactPath(t *testing.T) string {
	t.Helper()
	raw := os.Getenv(artifactEnv)
	if raw == "" {
		t.Skipf("%s is not set; set it to the real MicroPython artifact (for example guest/build/quickpy.wasm) to run these integration tests", artifactEnv)
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot(t), path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s=%q: stat %s: %v", artifactEnv, raw, path, err)
	}
	if info.IsDir() {
		t.Fatalf("%s=%q: %s is a directory, want a .wasm file", artifactEnv, raw, path)
	}
	if info.Size() == 0 {
		t.Fatalf("%s=%q: %s is empty", artifactEnv, raw, path)
	}
	return path
}

func artifactEvidence(t *testing.T) (size int64, sum string) {
	t.Helper()
	path := artifactPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact %s: %v", path, err)
	}
	h := sha256.Sum256(data)
	return int64(len(data)), hex.EncodeToString(h[:])
}

// newRuntime creates a public Runtime with test-friendly defaults and closes it
// during cleanup. Tests that close explicitly are still safe because Close is
// idempotent.
func newRuntime(t *testing.T, opts quickpy.RuntimeOptions) *quickpy.Runtime {
	t.Helper()
	if opts.Artifact.Path == "" && len(opts.Artifact.Bytes) == 0 {
		opts.Artifact = quickpy.ArtifactSource{Path: artifactPath(t)}
	}
	if opts.MaxMemoryBytes == 0 {
		opts.MaxMemoryBytes = 32 << 20
	}
	if opts.MaxExecutions == 0 {
		opts.MaxExecutions = 4
	}
	if opts.DefaultTimeout == 0 {
		opts.DefaultTimeout = 10 * time.Second
	}
	r, err := quickpy.New(opts)
	if err != nil {
		t.Fatalf("quickpy.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("Runtime.Close: %v", err)
		}
	})
	return r
}

func execute(t *testing.T, r *quickpy.Runtime, req quickpy.ExecuteRequest) *quickpy.Execution {
	t.Helper()
	if req.Filename == "" {
		req.Filename = "main.py"
	}
	e, err := r.Execute(t.Context(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return e
}

func wait(t *testing.T, e *quickpy.Execution) (quickpy.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	return e.Wait(ctx)
}

func mustWait(t *testing.T, e *quickpy.Execution) quickpy.Result {
	t.Helper()
	res, err := wait(t, e)
	if err != nil {
		t.Fatalf("Wait: %v (result: %+v)", err, res)
	}
	return res
}

func requireErrorKind(t *testing.T, err error, want quickpy.Kind) *quickpy.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error kind %s, got nil", want)
	}
	var qe *quickpy.Error
	if !errors.As(err, &qe) {
		t.Fatalf("expected *quickpy.Error, got %T: %v", err, err)
	}
	if qe.Kind != want {
		t.Fatalf("error kind = %s, want %s (error: %v)", qe.Kind, want, err)
	}
	return qe
}

func requireStatus(t *testing.T, res quickpy.Result, want quickpy.Status) {
	t.Helper()
	if res.Status != want {
		t.Fatalf("status = %q, want %q (result: %+v)", res.Status, want, res)
	}
}

func requireNoState(t *testing.T, res quickpy.Result) {
	t.Helper()
	if res.State != nil || res.StateWritten || res.HasState() {
		t.Fatalf("state must not be committed: State=%#v StateWritten=%v HasState=%v", res.State, res.StateWritten, res.HasState())
	}
}

func requireState(t *testing.T, res quickpy.Result, want quickpy.Value) {
	t.Helper()
	if !res.StateWritten || !res.HasState() {
		t.Fatalf("state not committed: StateWritten=%v HasState=%v", res.StateWritten, res.HasState())
	}
	if !reflect.DeepEqual(res.State, want) {
		t.Fatalf("state = %#v, want %#v", res.State, want)
	}
}

type eventRecorder struct {
	mu     sync.Mutex
	events []quickpy.Event
}

func (r *eventRecorder) sink(ev quickpy.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *eventRecorder) snapshot() []quickpy.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]quickpy.Event(nil), r.events...)
}

func outputEvents(events []quickpy.Event) []quickpy.Event {
	var out []quickpy.Event
	for _, ev := range events {
		switch ev.(type) {
		case quickpy.TextEvent, quickpy.ConsoleEvent, quickpy.ImageEvent:
			out = append(out, ev)
		}
	}
	return out
}

func outputBytes(events []quickpy.Event) int {
	n := 0
	for _, ev := range events {
		switch e := ev.(type) {
		case quickpy.TextEvent:
			n += len(e.Text)
		case quickpy.ConsoleEvent:
			n += len(e.Text)
		case quickpy.ImageEvent:
			n += len(e.Data)
		}
	}
	return n
}

func textOf(events []quickpy.Event) string {
	var b strings.Builder
	for _, ev := range events {
		if te, ok := ev.(quickpy.TextEvent); ok {
			b.WriteString(te.Text)
		}
	}
	return b.String()
}

func requireEvents(t *testing.T, got, want []quickpy.Event) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func textEvents(texts ...string) []quickpy.Event {
	out := make([]quickpy.Event, len(texts))
	for i, s := range texts {
		out[i] = quickpy.TextEvent{Text: s}
	}
	return out
}

func argsMap(t *testing.T, v quickpy.Value) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("args type = %T, want map[string]any", v)
	}
	return m
}

func requireInt64(t *testing.T, v quickpy.Value, want int64) {
	t.Helper()
	got, ok := v.(int64)
	if !ok {
		t.Fatalf("value = %#v (%T), want int64(%d)", v, v, want)
	}
	if got != want {
		t.Fatalf("value = %d, want %d", got, want)
	}
}

func requireFloat64(t *testing.T, v quickpy.Value, want float64) {
	t.Helper()
	got, ok := v.(float64)
	if !ok {
		t.Fatalf("value = %#v (%T), want float64(%v)", v, v, want)
	}
	if got != want {
		t.Fatalf("value = %v, want %v", got, want)
	}
}

func requireString(t *testing.T, v quickpy.Value, want string) {
	t.Helper()
	got, ok := v.(string)
	if !ok {
		t.Fatalf("value = %#v (%T), want string(%q)", v, v, want)
	}
	if got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
}

func requireContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Fatalf("%q does not contain %q", s, substr)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitForError(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func waitForDone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// requireNoGoroutineLeak is a deliberately rough stability check. It allows a
// generous slack for wazero and test-framework goroutines instead of asserting
// an exact count.
func requireNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		after := runtime.NumGoroutine()
		if after <= before+32 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines grew from %d to %d", before, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startLoopingExecution starts an execution that calls a host tool once and
// then enters an infinite loop. The returned channel closes when the tool
// finished event is observed, which means the guest is about to resume into the
// loop. Tests should wait for it and then briefly settle before cancelling or
// closing. The prefix is prepended to the source, for example to set state
// before the loop.
func startLoopingExecution(t *testing.T, r *quickpy.Runtime, parent context.Context, prefix string, extraSink func(quickpy.Event)) (*quickpy.Execution, <-chan struct{}) {
	t.Helper()
	if parent == nil {
		parent = t.Context()
	}
	resumed := make(chan struct{})
	var once sync.Once
	sink := func(ev quickpy.Event) {
		if extraSink != nil {
			extraSink(ev)
		}
		if te, ok := ev.(quickpy.ToolEvent); ok && te.Name == "signal" && te.Phase == "finished" {
			once.Do(func() { close(resumed) })
		}
	}
	if err := r.RegisterTool("signal", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	source := prefix + `
import asyncio, quickpy
async def main():
    await quickpy.tools.signal(value=1)
    while True:
        pass
quickpy.run(main())`
	e, err := r.Execute(parent, quickpy.ExecuteRequest{Source: source, Filename: "main.py", EventSink: sink})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return e, resumed
}
