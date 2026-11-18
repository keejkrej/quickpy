package quickpy

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeEngine is a narrow test double for the low-level engine boundary.
type fakeEngine struct {
	mu           sync.Mutex
	closeCalls   int
	closeErr     error
	closeBlock   chan struct{}
	closeStarted chan struct{}
	executeFn    func(ctx context.Context, req engineRequest) (engineResult, error)
}

func (f *fakeEngine) Execute(ctx context.Context, req engineRequest) (engineResult, error) {
	if f.executeFn != nil {
		return f.executeFn(ctx, req)
	}
	return engineResult{Status: StatusCompleted}, nil
}

func (f *fakeEngine) Close(ctx context.Context) error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	if f.closeStarted != nil {
		select {
		case f.closeStarted <- struct{}{}:
		default:
		}
	}
	if f.closeBlock != nil {
		<-f.closeBlock
	}
	return f.closeErr
}

func (f *fakeEngine) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

func testRuntime(t *testing.T, opts RuntimeOptions, eng *fakeEngine) *Runtime {
	t.Helper()
	if opts.Artifact.Path == "" && len(opts.Artifact.Bytes) == 0 {
		opts.Artifact = ArtifactSource{Bytes: []byte("fake")}
	}
	r, err := newRuntime(context.Background(), opts, func(ctx context.Context, eo engineOptions) (engine, error) {
		return eng, nil
	})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	return r
}

func waitActive(t *testing.T, r *Runtime, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.activeMu.Lock()
		n := len(r.active)
		r.activeMu.Unlock()
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active executions = %d, want %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}
