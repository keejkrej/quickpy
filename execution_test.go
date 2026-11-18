package quickpy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWaitRepeated(t *testing.T) {
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			return engineResult{Status: StatusCompleted, ToolCalls: 2, Duration: time.Millisecond}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res1, err1 := e.Wait(context.Background())
	res2, err2 := e.Wait(context.Background())
	if err1 != nil || err2 != nil {
		t.Fatalf("errs: %v %v", err1, err2)
	}
	if !reflect.DeepEqual(res1, res2) {
		t.Fatalf("results differ: %+v %+v", res1, res2)
	}
	if res1.Status != StatusCompleted || res1.ToolCalls != 2 {
		t.Fatalf("result = %+v", res1)
	}
}

func TestWaitConcurrent(t *testing.T) {
	release := make(chan struct{})
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			<-release
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	results := make([]Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = e.Wait(context.Background())
		}(i)
	}
	close(release)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("err[%d] = %v", i, errs[i])
		}
		if !reflect.DeepEqual(results[i], results[0]) {
			t.Fatalf("result[%d] differs", i)
		}
	}
}

func TestWaitLocalTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			close(started)
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := e.Wait(ctx)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("got %v, want ErrWaitTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	if !reflect.DeepEqual(res, Result{}) {
		t.Fatalf("expected zero result, got %+v", res)
	}
	close(release)
	res, err = e.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
}

func cancelableEngine(started chan<- struct{}) *fakeEngine {
	return &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			close(started)
			<-ctx.Done()
			cause := context.Cause(ctx)
			if errors.Is(cause, context.DeadlineExceeded) {
				return engineResult{Status: StatusTimedOut}, &Error{Kind: KindTimedOut, Status: StatusTimedOut, Message: "timed out"}
			}
			return engineResult{Status: StatusCancelled}, &Error{Kind: KindCancelled, Status: StatusCancelled, Message: "cancelled"}
		},
	}
}

func TestCancel(t *testing.T) {
	started := make(chan struct{})
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, cancelableEngine(started))
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	e.Cancel()
	e.Cancel()
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindCancelled {
		t.Fatalf("got %v, want Cancelled", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestParentCancel(t *testing.T) {
	started := make(chan struct{})
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, cancelableEngine(started))
	parent, cancel := context.WithCancel(context.Background())
	e, err := r.Execute(parent, ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindCancelled {
		t.Fatalf("got %v, want Cancelled", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestParentDeadline(t *testing.T) {
	started := make(chan struct{})
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, cancelableEngine(started))
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	e, err := r.Execute(parent, ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindTimedOut {
		t.Fatalf("got %v, want TimedOut", err)
	}
	if res.Status != StatusTimedOut {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestCancelFinishRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		eng := &fakeEngine{
			executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
				return engineResult{Status: StatusCompleted}, nil
			},
		}
		r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
		e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
		if err != nil {
			t.Fatal(err)
		}
		go e.Cancel()
		res, err := e.Wait(context.Background())
		if err != nil {
			var qe *Error
			if !errors.As(err, &qe) || qe.Kind != KindCancelled {
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if res.Status != StatusCompleted && res.Status != StatusCancelled {
			t.Fatalf("unexpected status %q", res.Status)
		}
	}
}

func TestErrorDoesNotCommitState(t *testing.T) {
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			return engineResult{
				Status:       StatusFailed,
				State:        map[string]any{"x": int64(1)},
				StateWritten: true,
			}, &Error{Kind: KindScriptError, Status: StatusFailed, Message: "boom"}
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if res.State != nil || res.StateWritten || res.HasState() {
		t.Fatalf("state committed: %+v", res)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestToolErrorKindsDoNotCommitState(t *testing.T) {
	kinds := []Kind{
		KindScriptError,
		KindGuestFault,
		KindHostFault,
		KindProtocolError,
		KindLimitExceeded,
		KindCancelled,
		KindTimedOut,
		KindStalled,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			eng := &fakeEngine{
				executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
					return engineResult{
						Status:       statusForError(&Error{Kind: kind}),
						State:        map[string]any{"x": int64(1)},
						StateWritten: true,
					}, &Error{Kind: kind, Message: "tool failure"}
				},
			}
			r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
			e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
			if err != nil {
				t.Fatal(err)
			}
			res, err := e.Wait(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if res.State != nil || res.StateWritten || res.HasState() {
				t.Fatalf("state committed: %+v", res)
			}
		})
	}
}

func TestStatusFromErrorKind(t *testing.T) {
	tests := []struct {
		kind Kind
		want Status
	}{
		{KindScriptError, StatusFailed},
		{KindGuestFault, StatusFailed},
		{KindHostFault, StatusFailed},
		{KindProtocolError, StatusFailed},
		{KindTimedOut, StatusTimedOut},
		{KindCancelled, StatusCancelled},
		{KindLimitExceeded, StatusLimitExceeded},
		{KindStalled, StatusStalled},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			eng := &fakeEngine{
				executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
					return engineResult{}, &Error{Kind: tt.kind, Message: "x"}
				},
			}
			r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
			e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
			if err != nil {
				t.Fatal(err)
			}
			res, err := e.Wait(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if res.Status != tt.want {
				t.Fatalf("status = %q, want %q", res.Status, tt.want)
			}
		})
	}
}

func TestStateCommit(t *testing.T) {
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			return engineResult{
				Status:       StatusCompleted,
				State:        map[string]any{"x": int64(1)},
				StateWritten: true,
			}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.StateWritten || !res.HasState() {
		t.Fatalf("state not committed: %+v", res)
	}
	if !reflect.DeepEqual(res.State, map[string]any{"x": int64(1)}) {
		t.Fatalf("state = %#v", res.State)
	}
}

func TestStateLimit(t *testing.T) {
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			return engineResult{
				Status:       StatusCompleted,
				State:        strings.Repeat("x", 100),
				StateWritten: true,
			}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1, MaxStateBytes: 10}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindLimitExceeded {
		t.Fatalf("got %v, want LimitExceeded", err)
	}
	if res.State != nil || res.StateWritten {
		t.Fatalf("state committed: %+v", res)
	}
	if res.Status != StatusLimitExceeded {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestExecutionIDStable(t *testing.T) {
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 2}, &fakeEngine{})
	e1, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	if e1.ID() == "" || e2.ID() == "" || e1.ID() == e2.ID() {
		t.Fatalf("ids: %q %q", e1.ID(), e2.ID())
	}
	if _, err := e1.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
