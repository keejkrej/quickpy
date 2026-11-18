package integration_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

// blockingTool is a host tool that signals when it starts and then blocks until
// released. The cooperative variant also observes context cancellation; the
// noncooperative variant deliberately ignores it.
type blockingTool struct {
	started     chan struct{}
	release     chan struct{}
	done        chan struct{}
	ctxErr      chan error
	startOnce   sync.Once
	releaseOnce sync.Once
	doneOnce    sync.Once
	called      atomic.Bool
}

func newBlockingTool() *blockingTool {
	return &blockingTool{
		started: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
		ctxErr:  make(chan error, 1),
	}
}

func (b *blockingTool) releaseTool() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingTool) cooperative(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
	b.called.Store(true)
	b.startOnce.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		b.ctxErr <- ctx.Err()
	case <-b.release:
		b.ctxErr <- ctx.Err()
	}
	b.doneOnce.Do(func() { close(b.done) })
	return "released", nil
}

func (b *blockingTool) noncooperative(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
	b.called.Store(true)
	b.startOnce.Do(func() { close(b.started) })
	<-b.release
	b.ctxErr <- ctx.Err()
	b.doneOnce.Do(func() { close(b.done) })
	return "released", nil
}

func (b *blockingTool) cleanup(t *testing.T) {
	t.Helper()
	b.releaseTool()
	if !b.called.Load() {
		return
	}
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
		t.Errorf("blocking tool did not exit")
	}
}

func TestCloseRunningExecution(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	e, resumed := startLoopingExecution(t, r, nil, "", nil)
	waitFor(t, resumed, "tool finished")
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res, err := wait(t, e)
	requireErrorKind(t, err, quickpy.KindCancelled)
	requireStatus(t, res, quickpy.StatusCancelled)
	requireNoState(t, res)

	_, err = r.Execute(context.Background(), quickpy.ExecuteRequest{Source: `print("x")`, Filename: "main.py"})
	requireErrorKind(t, err, quickpy.KindRuntimeClosed)
}

func TestCloseSuspendedExecution(t *testing.T) {
	tool := newBlockingTool()
	t.Cleanup(func() { tool.cleanup(t) })
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("block", tool.cooperative); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await quickpy.tools.block(value=1)
quickpy.run(main())`})
	waitFor(t, tool.started, "tool start")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res, err := wait(t, e)
	requireErrorKind(t, err, quickpy.KindCancelled)
	requireStatus(t, res, quickpy.StatusCancelled)
	requireNoState(t, res)

	waitForDone(t, tool.done, "cooperative tool exit")
	select {
	case ctxErr := <-tool.ctxErr:
		if !errors.Is(ctxErr, context.Canceled) {
			t.Fatalf("tool context error = %v, want context.Canceled", ctxErr)
		}
	default:
		t.Fatal("tool did not report its context error")
	}
}

func TestCloseNoncooperativeTool(t *testing.T) {
	tool := newBlockingTool()
	t.Cleanup(func() { tool.cleanup(t) })
	r := newRuntime(t, quickpy.RuntimeOptions{})
	if err := r.RegisterTool("block", tool.noncooperative); err != nil {
		t.Fatal(err)
	}
	e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
async def main():
    await quickpy.tools.block(value=1)
quickpy.run(main())`})
	waitFor(t, tool.started, "tool start")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res, err := wait(t, e)
	requireErrorKind(t, err, quickpy.KindCancelled)
	requireStatus(t, res, quickpy.StatusCancelled)
	requireNoState(t, res)

	// Close must not wait for a handler that ignores cancellation.
	select {
	case <-tool.done:
		t.Fatal("noncooperative tool exited before release; Close waited for it")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCloseConcurrentIdempotent(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	e, resumed := startLoopingExecution(t, r, nil, "", nil)
	waitFor(t, resumed, "tool finished")
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.Close(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Close[%d]: %v", i, err)
		}
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	res, err := wait(t, e)
	requireErrorKind(t, err, quickpy.KindCancelled)
	requireStatus(t, res, quickpy.StatusCancelled)
}

func TestCloseShortContext(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	e, resumed := startLoopingExecution(t, r, nil, "", nil)
	waitFor(t, resumed, "tool finished")
	time.Sleep(20 * time.Millisecond)

	short, cancelShort := context.WithCancel(context.Background())
	cancelShort()
	if err := r.Close(short); !errors.Is(err, context.Canceled) {
		t.Fatalf("short Close = %v, want context.Canceled", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	res, err := wait(t, e)
	requireErrorKind(t, err, quickpy.KindCancelled)
	requireStatus(t, res, quickpy.StatusCancelled)
}
