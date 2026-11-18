package integration_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

// TestSoak is the opt-in resource/soak evidence test. It is skipped unless
// QUICKPY_SOAK is set, and it still skips when QUICKPY_TEST_ARTIFACT is unset.
// It runs a moderate, bounded workload (>=1000 guest executions) so it can be
// repeated in a few minutes without becoming an endless stress test.
func TestSoak(t *testing.T) {
	if os.Getenv("QUICKPY_SOAK") == "" {
		t.Skip("QUICKPY_SOAK is not set; set it to run the opt-in soak/resource test")
	}

	size, sum := artifactEvidence(t)
	t.Logf("artifact: %d bytes sha256=%s", size, sum)

	const (
		cycles             = 20
		sequentialPerCycle = 40
		parallelPerCycle   = 10
	)
	const totalPlanned = cycles * (sequentialPerCycle + parallelPerCycle + 2) // + recursion + cancellation

	rssBefore, rssOK := processRSSKiB(t)
	goroutinesBefore := runtime.NumGoroutine()
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	type batchMetrics struct {
		cycle      int
		executions int
		elapsed    time.Duration
		rssKiB     int64
		goroutines int
		heapAlloc  uint64
		totalAlloc uint64
		errors     int
	}
	batches := make([]batchMetrics, 0, cycles)
	totalExecutions := 0
	totalErrors := 0
	start := time.Now()

	for cycle := 0; cycle < cycles; cycle++ {
		cycleStart := time.Now()
		cycleErrors := 0
		r := newRuntime(t, quickpy.RuntimeOptions{MaxExecutions: 10})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			args, _ := call.Args.(map[string]any)
			return args["value"], nil
		}); err != nil {
			t.Fatal(err)
		}

		source := func(i int) string {
			return fmt.Sprintf(`import asyncio, quickpy
quickpy.set_state({"cycle": %d, "i": %d})
async def main():
    r = await quickpy.tools.echo(value=%d)
    print("ok", %d, %d, r)
quickpy.run(main())`, cycle, i, i, cycle, i)
		}

		// Sequential executions: one at a time, with prints, a tool, and state.
		for i := 0; i < sequentialPerCycle; i++ {
			res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: source(i)}))
			requireStatus(t, res, quickpy.StatusCompleted)
			requireContains(t, textOf(res.Outputs), fmt.Sprintf("ok %d %d %d", cycle, i, i))
			requireState(t, res, map[string]any{"cycle": int64(cycle), "i": int64(i)})
			totalExecutions++
		}

		// Parallel batch: distinct executions admitted together.
		var wg sync.WaitGroup
		errs := make([]error, parallelPerCycle)
		results := make([]quickpy.Result, parallelPerCycle)
		for i := 0; i < parallelPerCycle; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				e, err := r.Execute(context.Background(), quickpy.ExecuteRequest{
					Source:   source(sequentialPerCycle + i),
					Filename: "main.py",
				})
				if err != nil {
					errs[i] = err
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				results[i], errs[i] = e.Wait(ctx)
			}(i)
		}
		wg.Wait()
		for i := 0; i < parallelPerCycle; i++ {
			if errs[i] != nil {
				cycleErrors++
				totalErrors++
				t.Errorf("cycle %d parallel %d: %v", cycle, i, errs[i])
				continue
			}
			requireStatus(t, results[i], quickpy.StatusCompleted)
			requireContains(t, textOf(results[i].Outputs), fmt.Sprintf("ok %d %d %d", cycle, sequentialPerCycle+i, sequentialPerCycle+i))
			requireState(t, results[i], map[string]any{"cycle": int64(cycle), "i": int64(sequentialPerCycle + i)})
			totalExecutions++
		}

		// Recursion and allocation within guest bounds.
		rec := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
def rec(n):
    if n == 0:
        return 0
    return 1 + rec(n - 1)
x = rec(50)
data = [i for i in range(1000)]
quickpy.set_state({"x": x, "n": len(data)})
print("rec", x, len(data))`}))
		requireStatus(t, rec, quickpy.StatusCompleted)
		requireContains(t, textOf(rec.Outputs), "rec 50 1000")
		requireState(t, rec, map[string]any{"x": int64(50), "n": int64(1000)})
		totalExecutions++

		// Cancellation while a noncooperative handler is outstanding. The
		// handler ignores its context and returns only after the test releases
		// it; the late result must not commit state or change the terminal
		// result.
		tool := newSoakBlockingTool()
		if err := r.RegisterTool("block", tool.handler); err != nil {
			t.Fatal(err)
		}
		e := execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await quickpy.tools.block(value=1)
quickpy.run(main())`})
		waitFor(t, tool.started, "soak blocking tool start")
		if got := tool.called.Load(); got != 1 {
			t.Fatalf("cycle %d: blocking handler called %d times, want 1", cycle, got)
		}
		e.Cancel()
		res, err := wait(t, e)
		requireErrorKind(t, err, quickpy.KindCancelled)
		requireStatus(t, res, quickpy.StatusCancelled)
		requireNoState(t, res)
		tool.releaseTool()
		waitForDone(t, tool.done, "soak blocking tool exit")
		// A later Wait must still see the same terminal result; the late
		// handler return must not commit state.
		res, err = wait(t, e)
		requireErrorKind(t, err, quickpy.KindCancelled)
		requireNoState(t, res)
		totalExecutions++

		// Capacity must be released after the cancelled execution.
		healthy := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("healthy")`}))
		requireStatus(t, healthy, quickpy.StatusCompleted)
		requireContains(t, textOf(healthy.Outputs), "healthy")
		totalExecutions++

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := r.Close(ctx); err != nil {
			cycleErrors++
			totalErrors++
			t.Errorf("cycle %d Close: %v", cycle, err)
		}
		cancel()

		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		rss, _ := processRSSKiB(t)
		batches = append(batches, batchMetrics{
			cycle:      cycle,
			executions: sequentialPerCycle + parallelPerCycle + 3,
			elapsed:    time.Since(cycleStart),
			rssKiB:     rss,
			goroutines: runtime.NumGoroutine(),
			heapAlloc:  mem.HeapAlloc,
			totalAlloc: mem.TotalAlloc,
			errors:     cycleErrors,
		})
	}

	elapsed := time.Since(start)
	rssAfter, _ := processRSSKiB(t)
	goroutinesAfter := runtime.NumGoroutine()
	// Let goroutines settle before the quiescent sample. This is a bounded
	// retry, not an exact-count assertion.
	waitForGoroutines(t, goroutinesBefore+32, 5*time.Second)
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	rssQuiescent, _ := processRSSKiB(t)
	goroutinesQuiescent := runtime.NumGoroutine()

	t.Logf("soak: %d executions in %v (planned %d), errors=%d", totalExecutions, elapsed, totalPlanned, totalErrors)
	if rssOK {
		t.Logf("rss: before=%d KiB after=%d KiB quiescent=%d KiB", rssBefore, rssAfter, rssQuiescent)
	} else {
		t.Logf("rss: unavailable on this platform")
	}
	t.Logf("goroutines: before=%d after=%d quiescent=%d", goroutinesBefore, goroutinesAfter, goroutinesQuiescent)
	t.Logf("heap: before=%d after=%d total_alloc_delta=%d", memBefore.HeapAlloc, memAfter.HeapAlloc, memAfter.TotalAlloc-memBefore.TotalAlloc)
	for _, b := range batches {
		t.Logf("batch %02d: exec=%d elapsed=%v rss=%d KiB goroutines=%d heap=%d total_alloc=%d errors=%d",
			b.cycle, b.executions, b.elapsed, b.rssKiB, b.goroutines, b.heapAlloc, b.totalAlloc, b.errors)
	}

	if totalExecutions < 1000 {
		t.Fatalf("total executions = %d, want >= 1000", totalExecutions)
	}
	if totalErrors != 0 {
		t.Fatalf("soak errors = %d, want 0", totalErrors)
	}

	// Coarse resource-growth checks. These are deliberately generous and only
	// fail on a clearly monotonic increase across comparable batches. A single
	// RSS sample is not a memory-bound proof.
	goroutines := make([]int64, len(batches))
	rss := make([]int64, len(batches))
	heap := make([]int64, len(batches))
	for i, b := range batches {
		goroutines[i] = int64(b.goroutines)
		rss[i] = b.rssKiB
		heap[i] = int64(b.heapAlloc)
	}
	checkMonotonicGrowth(t, "goroutines", goroutines, 64)
	checkMonotonicGrowth(t, "rss_kib", rss, 256*1024)
	checkMonotonicGrowth(t, "heap_alloc", heap, 64<<20)
}

// soakBlockingTool is a noncooperative handler: it ignores context
// cancellation and returns only when the test releases it.
type soakBlockingTool struct {
	started     chan struct{}
	release     chan struct{}
	done        chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
	doneOnce    sync.Once
	called      atomic.Int32
}

func newSoakBlockingTool() *soakBlockingTool {
	return &soakBlockingTool{
		started: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (b *soakBlockingTool) releaseTool() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *soakBlockingTool) handler(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
	b.called.Add(1)
	b.startOnce.Do(func() { close(b.started) })
	<-b.release
	b.doneOnce.Do(func() { close(b.done) })
	return "late", nil
}

// waitForGoroutines waits up to d for the goroutine count to fall to at most
// want. It never fails; the caller records the final count as evidence.
func waitForGoroutines(t *testing.T, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= want || time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkMonotonicGrowth fails only when every sample increases and the total
// increase exceeds threshold. Equal or noisy samples are informational.
func checkMonotonicGrowth(t *testing.T, name string, values []int64, threshold int64) {
	t.Helper()
	if len(values) < 3 {
		return
	}
	monotonic := true
	for i := 1; i < len(values); i++ {
		if values[i] <= values[i-1] {
			monotonic = false
			break
		}
	}
	if !monotonic {
		return
	}
	delta := values[len(values)-1] - values[0]
	if delta > threshold {
		t.Errorf("%s grew monotonically by %d over %d batches (threshold %d); inspect the soak log", name, delta, len(values), threshold)
	}
}
