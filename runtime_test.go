package quickpy

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeOptionsValidation(t *testing.T) {
	validArtifact := ArtifactSource{Bytes: []byte("fake")}
	tests := []struct {
		name string
		opts RuntimeOptions
	}{
		{"missing artifact", RuntimeOptions{MaxMemoryBytes: 1, MaxExecutions: 1}},
		{"both artifact", RuntimeOptions{Artifact: ArtifactSource{Path: "x", Bytes: []byte("y")}, MaxMemoryBytes: 1, MaxExecutions: 1}},
		{"zero memory", RuntimeOptions{Artifact: validArtifact, MaxExecutions: 1}},
		{"negative memory", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: -1, MaxExecutions: 1}},
		{"zero executions", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1}},
		{"negative executions", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: -1}},
		{"negative timeout", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, DefaultTimeout: -1}},
		{"negative source", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxSourceBytes: -1}},
		{"negative message", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxMessageBytes: -1}},
		{"negative output", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxOutputBytes: -1}},
		{"negative tool result", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxToolResultBytes: -1}},
		{"negative state", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxStateBytes: -1}},
		{"negative tool calls", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxToolCalls: -1}},
		{"negative concurrent", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxConcurrentToolCalls: -1}},
		{"negative pending", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxPendingCalls: -1}},
		{"negative resumes", RuntimeOptions{Artifact: validArtifact, MaxMemoryBytes: 1, MaxExecutions: 1, MaxResumes: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newRuntime(context.Background(), tt.opts, func(ctx context.Context, eo engineOptions) (engine, error) {
				t.Fatal("factory must not be called for invalid options")
				return nil, nil
			})
			var qe *Error
			if !errors.As(err, &qe) || qe.Kind != KindInvalidRequest {
				t.Fatalf("got %v, want InvalidRequest", err)
			}
		})
	}
}

func TestRuntimeDefaults(t *testing.T) {
	opts, err := validateRuntimeOptions(RuntimeOptions{
		Artifact:       ArtifactSource{Bytes: []byte("fake")},
		MaxMemoryBytes: 1,
		MaxExecutions:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.MaxSourceBytes != 256<<10 {
		t.Fatalf("MaxSourceBytes = %d", opts.MaxSourceBytes)
	}
	if opts.MaxMessageBytes != 1<<20 {
		t.Fatalf("MaxMessageBytes = %d", opts.MaxMessageBytes)
	}
	if opts.MaxOutputBytes != 4<<20 {
		t.Fatalf("MaxOutputBytes = %d", opts.MaxOutputBytes)
	}
	if opts.MaxToolResultBytes != 1<<20 {
		t.Fatalf("MaxToolResultBytes = %d", opts.MaxToolResultBytes)
	}
	if opts.MaxStateBytes != 1<<20 {
		t.Fatalf("MaxStateBytes = %d", opts.MaxStateBytes)
	}
	if opts.MaxConcurrentToolCalls != 8 {
		t.Fatalf("MaxConcurrentToolCalls = %d", opts.MaxConcurrentToolCalls)
	}
	if opts.MaxPendingCalls != 64 {
		t.Fatalf("MaxPendingCalls = %d", opts.MaxPendingCalls)
	}
	if opts.MaxResumes != 10000 {
		t.Fatalf("MaxResumes = %d", opts.MaxResumes)
	}
	if opts.MaxToolCalls != 0 {
		t.Fatalf("MaxToolCalls = %d, want 0 (no cap)", opts.MaxToolCalls)
	}
}

func TestRegistry(t *testing.T) {
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, &fakeEngine{})
	h1 := func(ctx context.Context, call ToolCall) (Value, error) { return "one", nil }
	h2 := func(ctx context.Context, call ToolCall) (Value, error) { return "two", nil }
	if err := r.RegisterTool("b", h1); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterTool("a", h2); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterTool("a", h1); err == nil {
		t.Fatal("duplicate registration must fail")
	}
	if got := r.ToolNames(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("names = %v", got)
	}
	if err := r.UnregisterTool("a"); err != nil {
		t.Fatal(err)
	}
	if err := r.UnregisterTool("a"); err == nil {
		t.Fatal("unregister missing must fail")
	}
	if got := r.ToolNames(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestRegistryNameValidation(t *testing.T) {
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, &fakeEngine{})
	h := func(ctx context.Context, call ToolCall) (Value, error) { return nil, nil }
	for _, name := range []string{"", "1abc", "a-b", "a b", strings.Repeat("a", 65)} {
		if err := r.RegisterTool(name, h); err == nil {
			t.Fatalf("name %q should be rejected", name)
		}
	}
	if err := r.RegisterTool("_ok", h); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterTool(strings.Repeat("a", 64), h); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterTool("nil", nil); err == nil {
		t.Fatal("nil handler must be rejected")
	}
}

func TestRegistrySnapshot(t *testing.T) {
	var mu sync.Mutex
	var snapshots [][]string
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			names := make([]string, 0, len(req.Tools))
			for name := range req.Tools {
				names = append(names, name)
			}
			sort.Strings(names)
			mu.Lock()
			snapshots = append(snapshots, names)
			mu.Unlock()
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	h := func(ctx context.Context, call ToolCall) (Value, error) { return nil, nil }
	if err := r.RegisterTool("a", h); err != nil {
		t.Fatal(err)
	}
	e1, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterTool("b", h); err != nil {
		t.Fatal(err)
	}
	e2, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(snapshots) != 2 {
		t.Fatalf("snapshots = %v", snapshots)
	}
	if !reflect.DeepEqual(snapshots[0], []string{"a"}) {
		t.Fatalf("first snapshot = %v", snapshots[0])
	}
	if !reflect.DeepEqual(snapshots[1], []string{"a", "b"}) {
		t.Fatalf("second snapshot = %v", snapshots[1])
	}
}

func TestCapacityExceeded(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			once.Do(func() { close(started) })
			<-release
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_, err = r.Execute(context.Background(), ExecuteRequest{Source: "y", Filename: "y.py"})
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindCapacityExceeded {
		t.Fatalf("got %v, want CapacityExceeded", err)
	}
	close(release)
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitActive(t, r, 0)
	e2, err := r.Execute(context.Background(), ExecuteRequest{Source: "z", Filename: "z.py"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// waitDone is a bounded guard for lifecycle tests. It is not a retry loop: the
// test either observes the terminal signal or fails.
func waitDone(t *testing.T, e *Execution) {
	t.Helper()
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("execution did not reach terminal state")
	}
}

// TestTerminalDoneAfterActiveRemoval pins the capacity invariant: done must
// not be observable while the execution is still counted against
// MaxExecutions. The test holds the admission mutex, which the old deferred
// removal needed, so the old ordering would leave the execution in the active
// set when done closed.
func TestTerminalDoneAfterActiveRemoval(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	r.mu.Lock()
	close(release)
	waitDone(t, e)
	r.activeMu.Lock()
	_, stillActive := r.active[e]
	r.activeMu.Unlock()
	r.mu.Unlock()
	if stillActive {
		t.Fatal("execution still in-flight after done closed")
	}

	// With MaxExecutions=1, an immediate Execute after Wait must be admitted.
	e2, err := r.Execute(context.Background(), ExecuteRequest{Source: "y", Filename: "y.py"})
	if err != nil {
		t.Fatalf("immediate execute after Wait: %v", err)
	}
	if _, err := e2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalDoneAfterActiveRemovalConcurrentWait(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	const n = 8
	var wg sync.WaitGroup
	results := make([]Result, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = e.Wait(context.Background())
		}(i)
	}

	r.mu.Lock()
	close(release)
	waitDone(t, e)
	r.activeMu.Lock()
	_, stillActive := r.active[e]
	r.activeMu.Unlock()
	r.mu.Unlock()
	wg.Wait()
	if stillActive {
		t.Fatal("execution still in-flight after done closed")
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("wait[%d]: %v", i, errs[i])
		}
		if results[i].Status != StatusCompleted {
			t.Fatalf("status[%d] = %q", i, results[i].Status)
		}
	}

	e2, err := r.Execute(context.Background(), ExecuteRequest{Source: "y", Filename: "y.py"})
	if err != nil {
		t.Fatalf("immediate execute after concurrent Wait: %v", err)
	}
	if _, err := e2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalDoneAfterActiveRemovalCancel(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return engineResult{Status: StatusCancelled}, &Error{Kind: KindCancelled, Status: StatusCancelled, Message: "cancelled"}
			}
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	r.mu.Lock()
	e.Cancel()
	waitDone(t, e)
	r.activeMu.Lock()
	_, stillActive := r.active[e]
	r.activeMu.Unlock()
	r.mu.Unlock()
	if stillActive {
		t.Fatal("execution still in-flight after done closed")
	}

	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindCancelled {
		t.Fatalf("got %v, want Cancelled", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status = %q", res.Status)
	}

	e2, err := r.Execute(context.Background(), ExecuteRequest{Source: "y", Filename: "y.py"})
	if err != nil {
		t.Fatalf("immediate execute after cancel: %v", err)
	}
	if _, err := e2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalDoneAfterActiveRemovalClose(t *testing.T) {
	started := make(chan struct{})
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			close(started)
			<-ctx.Done()
			return engineResult{Status: StatusCancelled}, &Error{Kind: KindCancelled, Status: StatusCancelled, Message: "cancelled"}
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(context.Background()) }()
	waitDone(t, e)
	r.activeMu.Lock()
	_, stillActive := r.active[e]
	r.activeMu.Unlock()
	if stillActive {
		t.Fatal("execution still in-flight after done closed")
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}

	_, err = r.Execute(context.Background(), ExecuteRequest{Source: "y", Filename: "y.py"})
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindRuntimeClosed {
		t.Fatalf("got %v, want RuntimeClosed", err)
	}
}

// TestSinkPanicDoesNotFinishEarly proves that a recovered sink panic does not
// publish a terminal result while the engine execution is still running. The
// execution must remain in-flight until the engine returns.
func TestSinkPanicDoesNotFinishEarly(t *testing.T) {
	started := make(chan struct{})
	observed := make(chan struct{})
	release := make(chan struct{})
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			close(started)
			req.Observer(observation{Kind: observationText, Text: "x"})
			close(observed)
			<-release
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
		EventSink:      func(ev Event) { panic("boom") },
	}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	<-observed

	select {
	case <-e.done:
		t.Fatal("done closed before engine execution ended")
	default:
	}
	r.activeMu.Lock()
	_, active := r.active[e]
	r.activeMu.Unlock()
	if !active {
		t.Fatal("execution removed from active before engine execution ended")
	}

	close(release)
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindHostFault {
		t.Fatalf("got %v, want HostFault", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status = %q", res.Status)
	}
	r.activeMu.Lock()
	_, active = r.active[e]
	r.activeMu.Unlock()
	if active {
		t.Fatal("execution still active after done")
	}
}

func TestExecuteValidation(t *testing.T) {
	r := testRuntime(t, RuntimeOptions{
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
		DefaultTimeout: time.Second,
		MaxSourceBytes: 4,
		MaxOutputBytes: 8,
		MaxToolCalls:   2,
	}, &fakeEngine{})
	tests := []struct {
		name string
		req  ExecuteRequest
	}{
		{"missing filename", ExecuteRequest{Source: "x"}},
		{"source too large", ExecuteRequest{Source: "12345", Filename: "x.py"}},
		{"negative timeout", ExecuteRequest{Source: "x", Filename: "x.py", Timeout: -1}},
		{"loose timeout", ExecuteRequest{Source: "x", Filename: "x.py", Timeout: 2 * time.Second}},
		{"negative memory", ExecuteRequest{Source: "x", Filename: "x.py", MaxMemoryBytes: -1}},
		{"lower memory", ExecuteRequest{Source: "x", Filename: "x.py", MaxMemoryBytes: 512 << 10}},
		{"loose memory", ExecuteRequest{Source: "x", Filename: "x.py", MaxMemoryBytes: 2 << 20}},
		{"negative output", ExecuteRequest{Source: "x", Filename: "x.py", MaxOutputBytes: -1}},
		{"loose output", ExecuteRequest{Source: "x", Filename: "x.py", MaxOutputBytes: 9}},
		{"negative tool calls", ExecuteRequest{Source: "x", Filename: "x.py", MaxToolCalls: -1}},
		{"loose tool calls", ExecuteRequest{Source: "x", Filename: "x.py", MaxToolCalls: 3}},
		{"invalid initial state", ExecuteRequest{Source: "x", Filename: "x.py", InitialState: int(1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.Execute(context.Background(), tt.req)
			var qe *Error
			if !errors.As(err, &qe) || qe.Kind != KindInvalidRequest {
				t.Fatalf("got %v, want InvalidRequest", err)
			}
		})
	}
}

func TestExecuteTightensLimits(t *testing.T) {
	var got engineRequest
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			got = req
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
		DefaultTimeout: time.Second,
		MaxOutputBytes: 100,
		MaxToolCalls:   10,
	}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{
		Source:         "x",
		Filename:       "x.py",
		Timeout:        500 * time.Millisecond,
		MaxOutputBytes: 50,
		MaxToolCalls:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Timeout != 500*time.Millisecond {
		t.Fatalf("timeout = %v", got.Timeout)
	}
	if got.MaxMemoryBytes != 1<<20 {
		t.Fatalf("memory = %d", got.MaxMemoryBytes)
	}
	if got.MaxOutputBytes != 50 {
		t.Fatalf("output = %d", got.MaxOutputBytes)
	}
	if got.MaxToolCalls != 5 {
		t.Fatalf("tool calls = %d", got.MaxToolCalls)
	}
}

func TestExecuteMemoryOverride(t *testing.T) {
	const runtimeMemory = int64(1 << 20)
	var got engineRequest
	var calls int
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			calls++
			got = req
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: runtimeMemory, MaxExecutions: 1}, eng)

	// Zero inherits the runtime cap.
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.MaxMemoryBytes != runtimeMemory {
		t.Fatalf("inherited memory = %d, want %d", got.MaxMemoryBytes, runtimeMemory)
	}

	// An explicit value equal to the runtime cap is accepted.
	e, err = r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py", MaxMemoryBytes: runtimeMemory})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.MaxMemoryBytes != runtimeMemory {
		t.Fatalf("equal memory = %d, want %d", got.MaxMemoryBytes, runtimeMemory)
	}

	// A lower value is rejected before admission and starts no execution.
	_, err = r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py", MaxMemoryBytes: runtimeMemory / 2})
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindInvalidRequest {
		t.Fatalf("got %v, want InvalidRequest", err)
	}
	if !strings.Contains(qe.Message, "not supported") {
		t.Fatalf("message = %q", qe.Message)
	}
	if calls != 2 {
		t.Fatalf("engine calls = %d, want 2", calls)
	}
	waitActive(t, r, 0)
}

func TestExecuteNoToolCallCap(t *testing.T) {
	var got engineRequest
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			got = req
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.MaxToolCalls != math.MaxInt {
		t.Fatalf("MaxToolCalls = %d, want math.MaxInt", got.MaxToolCalls)
	}
}

func TestCloseIdempotent(t *testing.T) {
	eng := &fakeEngine{}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := eng.closeCount(); got != 1 {
		t.Fatalf("close calls = %d", got)
	}
}

func TestCloseAdmission(t *testing.T) {
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, &fakeEngine{})
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindRuntimeClosed {
		t.Fatalf("got %v, want RuntimeClosed", err)
	}
}

func TestCloseConcurrent(t *testing.T) {
	eng := &fakeEngine{}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.Close(context.Background())
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	if got := eng.closeCount(); got != 1 {
		t.Fatalf("close calls = %d", got)
	}
}

func TestCloseShortContext(t *testing.T) {
	release := make(chan struct{})
	eng := &fakeEngine{closeBlock: release}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	close(release)
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if got := eng.closeCount(); got != 1 {
		t.Fatalf("close calls = %d", got)
	}
}

func TestCloseWaitsForActiveEntry(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	closeStarted := make(chan struct{}, 1)
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			close(started)
			<-release
			return engineResult{Status: StatusCompleted}, nil
		},
		closeStarted: closeStarted,
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(context.Background()) }()
	select {
	case <-closeStarted:
		t.Fatal("engine.Close called before active entry stopped")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("engine.Close not called")
	}
}

func TestCloseReturnsEngineError(t *testing.T) {
	want := errors.New("close failed")
	eng := &fakeEngine{closeErr: want}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	if err := r.Close(context.Background()); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if err := r.Close(context.Background()); !errors.Is(err, want) {
		t.Fatalf("second close got %v, want %v", err, want)
	}
	if got := eng.closeCount(); got != 1 {
		t.Fatalf("close calls = %d", got)
	}
}

func TestCloseCancelsExecutions(t *testing.T) {
	started := make(chan struct{})
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			close(started)
			<-ctx.Done()
			return engineResult{Status: StatusCancelled}, &Error{Kind: KindCancelled, Status: StatusCancelled, Message: "cancelled"}
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindCancelled {
		t.Fatalf("got %v, want Cancelled", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status = %q", res.Status)
	}
}
