package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keejkrej/quickpy/internal/wasmguest"
)

type report struct {
	Artifact        string                  `json:"artifact"`
	Imports         []wasmguest.Import      `json:"imports"`
	Single          *executionReport        `json:"single,omitempty"`
	Sequential      []executionReport       `json:"sequential,omitempty"`
	Concurrent      []executionReport       `json:"concurrent,omitempty"`
	Cancellation    *cancelReport           `json:"cancellation,omitempty"`
	Async           *asyncReport            `json:"async,omitempty"`
	AsyncCancel     *asyncCancelReport      `json:"async_cancel,omitempty"`
	Stall           *stallReport            `json:"stall,omitempty"`
	Sleep0          *sleep0Report           `json:"sleep0,omitempty"`
	CancelIsolation []cancelIsolationReport `json:"cancel_isolation,omitempty"`
	Measure         *measureReport          `json:"measure,omitempty"`
	OK              bool                    `json:"ok"`
	Failures        []string                `json:"failures,omitempty"`
	Notes           []string                `json:"notes,omitempty"`
}

type executionReport struct {
	Source              string                      `json:"source"`
	ExecutionID         string                      `json:"execution_id,omitempty"`
	InstanceID          string                      `json:"instance_id,omitempty"`
	Status              string                      `json:"status,omitempty"`
	Outputs             []wasmguest.Output          `json:"outputs,omitempty"`
	Exception           *wasmguest.Exception        `json:"exception,omitempty"`
	CapturedStdout      string                      `json:"captured_stdout,omitempty"`
	CapturedStderr      string                      `json:"captured_stderr,omitempty"`
	StdoutTruncated     bool                        `json:"stdout_truncated,omitempty"`
	StderrTruncated     bool                        `json:"stderr_truncated,omitempty"`
	OutputTruncated     bool                        `json:"output_truncated,omitempty"`
	Stalled             bool                        `json:"stalled,omitempty"`
	UnknownFields       []string                    `json:"unknown_fields,omitempty"`
	DurationMS          float64                     `json:"duration_ms"`
	MemoryPages         uint32                      `json:"memory_pages,omitempty"`
	Guest               wasmguest.GuestInfo         `json:"guest"`
	ExportCalls         wasmguest.ExportCalls       `json:"export_calls"`
	InvokeReturned      bool                        `json:"invoke_returned"`
	SkippedFreeOnCancel bool                        `json:"skipped_free_on_cancel,omitempty"`
	StartUnixNano       int64                       `json:"start_unix_nano"`
	EndUnixNano         int64                       `json:"end_unix_nano"`
	Error               *errorReport                `json:"error,omitempty"`
	Requests            []wasmguest.CallRequest     `json:"requests,omitempty"`
	DispatchOrder       []int64                     `json:"dispatch_order,omitempty"`
	CompletionOrder     []int64                     `json:"completion_order,omitempty"`
	DeliveredOrder      []int64                     `json:"delivered_order,omitempty"`
	ResumeCount         int                         `json:"resume_count"`
	ToolCalls           int                         `json:"tool_calls"`
	CallLedger          []wasmguest.CallLedgerEntry `json:"call_ledger,omitempty"`
	Trace               []string                    `json:"trace,omitempty"`
}

type asyncReport struct {
	Execution executionReport `json:"execution"`
	Checks    []string        `json:"checks,omitempty"`
	OK        bool            `json:"ok"`
}

type asyncCancelReport struct {
	Execution        executionReport `json:"execution"`
	HandlerCancelled bool            `json:"handler_cancelled"`
	Proven           bool            `json:"proven"`
	Note             string          `json:"note,omitempty"`
}

type stallReport struct {
	Execution executionReport `json:"execution"`
	Proven    bool            `json:"proven"`
	Note      string          `json:"note,omitempty"`
}

type sleep0Report struct {
	Execution executionReport `json:"execution"`
	OK        bool            `json:"ok"`
	Note      string          `json:"note,omitempty"`
}

type cancelIsolationSample struct {
	Scenario         string          `json:"scenario"`
	Canceled         executionReport `json:"canceled"`
	Other            executionReport `json:"other"`
	CancelLatencyMS  float64         `json:"cancel_latency_ms"`
	HandlerCancelled bool            `json:"handler_cancelled,omitempty"`
	OK               bool            `json:"ok"`
}

type cancelIsolationReport struct {
	Scenario string                  `json:"scenario"`
	Repeats  int                     `json:"repeats"`
	Samples  []cancelIsolationSample `json:"samples"`
	MinMS    float64                 `json:"min_ms"`
	MeanMS   float64                 `json:"mean_ms"`
	MaxMS    float64                 `json:"max_ms"`
	OK       bool                    `json:"ok"`
}

type measureReport struct {
	ArtifactSize  int64                   `json:"artifact_size"`
	CompileMS     []float64               `json:"compile_ms"`
	CompileMinMS  float64                 `json:"compile_min_ms"`
	CompileMeanMS float64                 `json:"compile_mean_ms"`
	CompileMaxMS  float64                 `json:"compile_max_ms"`
	StartupMS     []float64               `json:"startup_ms"`
	StartupMinMS  float64                 `json:"startup_min_ms"`
	StartupMeanMS float64                 `json:"startup_mean_ms"`
	StartupMaxMS  float64                 `json:"startup_max_ms"`
	Suspended     []suspendedMemorySample `json:"suspended"`
	Notes         []string                `json:"notes,omitempty"`
}

type suspendedMemorySample struct {
	N                 int     `json:"n"`
	MinPages          uint32  `json:"min_pages"`
	MeanPages         float64 `json:"mean_pages"`
	MaxPages          uint32  `json:"max_pages"`
	MinBytes          int64   `json:"min_bytes"`
	MeanBytes         float64 `json:"mean_bytes"`
	MaxBytes          int64   `json:"max_bytes"`
	RSSBaselineKiB    int64   `json:"rss_baseline_kib,omitempty"`
	RSSSuspendedKiB   int64   `json:"rss_suspended_kib,omitempty"`
	RSSDeltaKiB       int64   `json:"rss_delta_kib,omitempty"`
	RSSPerInstanceKiB float64 `json:"rss_per_instance_kib,omitempty"`
}

const defaultAsyncSource = `import asyncio
import quickpy

async def main():
    results = await asyncio.gather(
        quickpy.tools.a(value="A", delay_ms=200),
        quickpy.tools.b(value="B", delay_ms=50),
    )
    print(results)

quickpy.run(main())
`

const defaultAsyncCancelSource = `import asyncio
import quickpy

async def main():
    await asyncio.gather(
        quickpy.tools.a(value="A", delay_ms=10),
        quickpy.tools.block(value="X", delay_ms=10000),
    )

quickpy.run(main())
`

const defaultStallSource = `import asyncio
import quickpy

async def main():
    ev = asyncio.Event()
    await ev.wait()

quickpy.run(main())
`

const defaultSleep0Source = `import asyncio
import quickpy

async def main():
    await asyncio.sleep(0)
    print("after sleep0")

quickpy.run(main())
`

const defaultOtherSource = `import quickpy

async def main():
    v = await quickpy.tools.echo(value="unaffected")
    print(v)

quickpy.run(main())
`

const defaultCancelIsolationSuspendedSource = `import quickpy

async def main():
    await quickpy.tools.block(value="X", delay_ms=10000)

quickpy.run(main())
`

type errorReport struct {
	Kind          string               `json:"kind"`
	Status        string               `json:"status,omitempty"`
	Message       string               `json:"message"`
	Exception     *wasmguest.Exception `json:"exception,omitempty"`
	UnknownFields []string             `json:"unknown_fields,omitempty"`
}

type cancelReport struct {
	Execution executionReport `json:"execution"`
	Proven    bool            `json:"proven"`
	Note      string          `json:"note,omitempty"`
}

func main() {
	os.Exit(run())
}

func run() int {
	var (
		artifact                       = flag.String("artifact", "", "path to the QuickPy WASI artifact (.wasm)")
		source                         = flag.String("source", `print("hello from quickpy")`, "source for the primary execution")
		source2                        = flag.String("source2", `print("sequential run 2")`, "source for the second sequential/concurrent execution")
		filename                       = flag.String("filename", "main.py", "filename used in guest tracebacks")
		timeout                        = flag.Duration("timeout", 5*time.Second, "per-execution timeout (0 disables)")
		maxMemory                      = flag.Int64("max-memory", 64<<20, "per-instance linear memory cap in bytes")
		maxMessage                     = flag.Int("max-message", 1<<20, "max protocol message bytes")
		maxOutput                      = flag.Int("max-output", 4<<20, "max captured output bytes")
		mode                           = flag.String("mode", "all", "comma-separated modes: single,sequential,concurrent,cancel,async,async-cancel,stall,sleep0,cancel-isolation,measure,all")
		cancelTimeout                  = flag.Duration("cancel-timeout", 250*time.Millisecond, "deadline for the cancellation smoke")
		cancelSource                   = flag.String("cancel-source", "while True:\n    pass\n", "source for the cancellation smoke")
		asyncSource                    = flag.String("async-source", defaultAsyncSource, "source for the async gather proof")
		asyncCancelSource              = flag.String("async-cancel-source", defaultAsyncCancelSource, "source for the async cancellation proof")
		stallSource                    = flag.String("stall-source", defaultStallSource, "source for the terminal-stall proof")
		sleep0Source                   = flag.String("sleep0-source", defaultSleep0Source, "source for the sleep(0) proof")
		otherSource                    = flag.String("other-source", defaultOtherSource, "source for the unaffected overlapping execution")
		cancelIsolationSuspendedSource = flag.String("cancel-isolation-suspended-source", defaultCancelIsolationSuspendedSource, "source for the suspended-tool explicit-cancel isolation proof")
		cancelIsolationRepeats         = flag.Int("cancel-isolation-repeats", 3, "repeats for each explicit-cancel isolation scenario")
		measureSamples                 = flag.Int("measure-samples", 5, "samples for compile/startup measurements")
		measureSuspended               = flag.Int("measure-suspended", 4, "concurrently live suspended executions for memory measurement")
		jsonOut                        = flag.Bool("json", false, "emit JSON evidence")
		verbose                        = flag.Bool("verbose", false, "verbose human output")
	)
	flag.Parse()

	if *artifact == "" {
		fmt.Fprintln(os.Stderr, "quickpy-spike: -artifact is required")
		flag.Usage()
		return 2
	}

	ctx := context.Background()
	rt, err := wasmguest.NewRuntime(ctx, wasmguest.Options{
		ArtifactPath:    *artifact,
		MaxMemoryBytes:  *maxMemory,
		MaxMessageBytes: *maxMessage,
		MaxOutputBytes:  *maxOutput,
		DefaultTimeout:  *timeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "quickpy-spike: %v\n", err)
		return 1
	}
	defer func() { _ = rt.Close(context.Background()) }()

	rep := &report{
		Artifact: *artifact,
		Imports:  rt.Imports(),
		OK:       true,
	}

	modes := parseModes(*mode)
	if modes["all"] {
		modes["sequential"] = true
		modes["concurrent"] = true
		modes["cancel"] = true
	}

	if modes["single"] {
		er := runOne(ctx, rt, *source, *filename, *timeout)
		rep.Single = &er
		if er.Status != string(wasmguest.StatusCompleted) {
			rep.Failures = append(rep.Failures, fmt.Sprintf("single execution status %q", er.Status))
		}
	}

	if modes["sequential"] {
		reports, failures := runSequential(ctx, rt, *source, *source2, *filename, *timeout)
		rep.Sequential = reports
		rep.Failures = append(rep.Failures, failures...)
	}

	if modes["concurrent"] {
		reports, failures := runConcurrent(ctx, rt, *source, *source2, *filename, *timeout)
		rep.Concurrent = reports
		rep.Failures = append(rep.Failures, failures...)
	}

	if modes["cancel"] {
		cr, failures, proven := runCancellation(ctx, rt, *cancelSource, *filename, *cancelTimeout)
		rep.Cancellation = &cr
		rep.Failures = append(rep.Failures, failures...)
		if !proven {
			rep.Notes = append(rep.Notes, "cancellation smoke is UNPROVEN: the guest did not remain in a synchronous loop long enough to be force-closed")
		}
	}

	if modes["async"] {
		ar, failures := runAsync(ctx, rt, *asyncSource, *filename, *timeout)
		rep.Async = &ar
		rep.Failures = append(rep.Failures, failures...)
	}

	if modes["async-cancel"] {
		acr, failures, proven := runAsyncCancel(ctx, rt, *asyncCancelSource, *filename, *cancelTimeout)
		rep.AsyncCancel = &acr
		rep.Failures = append(rep.Failures, failures...)
		if !proven {
			rep.Notes = append(rep.Notes, "async cancellation proof is UNPROVEN: the guest did not suspend with an outstanding tool call")
		}
	}

	if modes["stall"] {
		sr, failures, proven := runStall(ctx, rt, *stallSource, *filename, *timeout)
		rep.Stall = &sr
		rep.Failures = append(rep.Failures, failures...)
		if !proven {
			rep.Notes = append(rep.Notes, "terminal-stall proof is UNPROVEN: the guest did not return a terminal stalled suspension")
		}
	}

	if modes["sleep0"] {
		sr, failures := runSleep0(ctx, rt, *sleep0Source, *filename, *timeout)
		rep.Sleep0 = &sr
		rep.Failures = append(rep.Failures, failures...)
	}

	if modes["cancel-isolation"] {
		reports, failures := runCancelIsolation(ctx, rt, *cancelSource, *cancelIsolationSuspendedSource, *otherSource, *filename, *cancelIsolationRepeats)
		rep.CancelIsolation = reports
		rep.Failures = append(rep.Failures, failures...)
	}

	if modes["measure"] {
		mr, failures := runMeasure(ctx, *artifact, *maxMemory, *maxMessage, *maxOutput, *timeout, *measureSamples, *measureSuspended)
		rep.Measure = &mr
		rep.Failures = append(rep.Failures, failures...)
	}

	if len(rep.Failures) > 0 {
		rep.OK = false
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "quickpy-spike: encode report: %v\n", err)
			return 1
		}
	} else {
		printHuman(rep, *verbose)
	}

	if !rep.OK {
		return 1
	}
	return 0
}

func parseModes(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

func runOne(ctx context.Context, rt *wasmguest.Runtime, source, filename string, timeout time.Duration) executionReport {
	return runOneRequest(ctx, rt, wasmguest.Request{Source: source, Filename: filename, Timeout: timeout})
}

func runOneRequest(ctx context.Context, rt *wasmguest.Runtime, req wasmguest.Request) executionReport {
	start := time.Now()
	var res *wasmguest.Result
	var err error
	if req.Tools == nil {
		res, err = rt.Execute(ctx, req)
	} else {
		res, err = rt.ExecuteWithTools(ctx, req)
	}
	end := time.Now()

	er := executionReport{
		Source:        req.Source,
		Guest:         wasmguest.GuestInfo{Imports: rt.Imports()},
		StartUnixNano: start.UnixNano(),
		EndUnixNano:   end.UnixNano(),
	}
	if res != nil {
		er.ExecutionID = res.ExecutionID
		er.InstanceID = res.InstanceID
		er.Status = string(res.Status)
		er.Outputs = res.Outputs
		er.Exception = res.Exception
		er.CapturedStdout = res.CapturedStdout
		er.CapturedStderr = res.CapturedStderr
		er.StdoutTruncated = res.StdoutTruncated
		er.StderrTruncated = res.StderrTruncated
		er.OutputTruncated = res.OutputTruncated
		er.Stalled = res.Stalled
		er.UnknownFields = res.UnknownFields
		er.DurationMS = res.DurationMS
		er.MemoryPages = res.MemoryPages
		er.Guest = res.Guest
		er.ExportCalls = res.ExportCalls
		er.InvokeReturned = res.InvokeReturned
		er.SkippedFreeOnCancel = res.SkippedFreeOnCancel
		er.Requests = res.Requests
		er.DispatchOrder = res.DispatchOrder
		er.CompletionOrder = res.CompletionOrder
		er.DeliveredOrder = res.DeliveredOrder
		er.ResumeCount = res.ResumeCount
		er.ToolCalls = res.ToolCalls
		er.CallLedger = res.CallLedger
		er.Trace = res.Trace
	}
	if err != nil {
		er.Error = toErrorReport(err)
		if er.Status == "" {
			if we, ok := err.(*wasmguest.Error); ok {
				er.Status = string(we.Status)
				if er.Status == "" {
					er.Status = string(we.Kind)
				}
			}
		}
	}
	return er
}

func toErrorReport(err error) *errorReport {
	if we, ok := err.(*wasmguest.Error); ok {
		return &errorReport{
			Kind:          string(we.Kind),
			Status:        string(we.Status),
			Message:       we.Message,
			Exception:     we.Exception,
			UnknownFields: we.UnknownFields,
		}
	}
	return &errorReport{Kind: "error", Message: err.Error()}
}

func runSequential(ctx context.Context, rt *wasmguest.Runtime, source, source2, filename string, timeout time.Duration) ([]executionReport, []string) {
	var failures []string
	a := runOne(ctx, rt, source, filename, timeout)
	b := runOne(ctx, rt, source2, filename, timeout)
	reports := []executionReport{a, b}

	if a.Status != string(wasmguest.StatusCompleted) {
		failures = append(failures, fmt.Sprintf("sequential run 1 status %q", a.Status))
	}
	if b.Status != string(wasmguest.StatusCompleted) {
		failures = append(failures, fmt.Sprintf("sequential run 2 status %q", b.Status))
	}
	if a.ExecutionID == b.ExecutionID {
		failures = append(failures, "sequential executions reused an execution ID")
	}
	if a.InstanceID == b.InstanceID {
		failures = append(failures, "sequential executions reused an instance ID")
	}
	return reports, failures
}

func runConcurrent(ctx context.Context, rt *wasmguest.Runtime, source, source2, filename string, timeout time.Duration) ([]executionReport, []string) {
	var wg sync.WaitGroup
	reports := make([]executionReport, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		reports[0] = runOne(ctx, rt, source, filename, timeout)
	}()
	go func() {
		defer wg.Done()
		reports[1] = runOne(ctx, rt, source2, filename, timeout)
	}()
	wg.Wait()

	var failures []string
	for i, r := range reports {
		if r.Status != string(wasmguest.StatusCompleted) {
			failures = append(failures, fmt.Sprintf("concurrent run %d status %q", i+1, r.Status))
		}
	}
	if reports[0].ExecutionID == reports[1].ExecutionID {
		failures = append(failures, "concurrent executions reused an execution ID")
	}
	if reports[0].InstanceID == reports[1].InstanceID {
		failures = append(failures, "concurrent executions reused an instance ID")
	}
	overlap := reports[0].StartUnixNano < reports[1].EndUnixNano && reports[1].StartUnixNano < reports[0].EndUnixNano
	if !overlap {
		failures = append(failures, "concurrent executions did not overlap in time")
	}
	return reports, failures
}

func runCancellation(ctx context.Context, rt *wasmguest.Runtime, source, filename string, timeout time.Duration) (cancelReport, []string, bool) {
	er := runOne(ctx, rt, source, filename, timeout)
	cr := cancelReport{Execution: er}
	proven := er.Status == string(wasmguest.StatusTimedOut) || er.Status == string(wasmguest.StatusCancelled)
	cr.Proven = proven
	if !proven {
		var failures []string
		if er.Status != string(wasmguest.StatusCompleted) {
			failures = append(failures, fmt.Sprintf("cancellation smoke status %q", er.Status))
		}
		cr.Note = "guest did not remain in a synchronous loop; cancellation path not exercised"
		return cr, failures, false
	}

	var failures []string
	if !er.InvokeReturned {
		failures = append(failures, "cancellation did not observe the active qp_invoke entry return")
	}
	if !er.SkippedFreeOnCancel {
		failures = append(failures, "cancellation did not record that guest exports were skipped after force-close")
	}
	if er.ExportCalls.Free != 0 {
		failures = append(failures, fmt.Sprintf("cancellation called qp_free %d times after force-close", er.ExportCalls.Free))
	}
	if er.ExportCalls.Invoke != 1 {
		failures = append(failures, fmt.Sprintf("cancellation entered qp_invoke %d times (want 1)", er.ExportCalls.Invoke))
	}
	bound := float64(timeout.Milliseconds()) + 2000
	if er.DurationMS > bound {
		failures = append(failures, fmt.Sprintf("cancellation latency %.1fms exceeds %.0fms bound", er.DurationMS, bound))
	}
	return cr, failures, true
}

func runAsync(ctx context.Context, rt *wasmguest.Runtime, source, filename string, timeout time.Duration) (asyncReport, []string) {
	tools := map[string]wasmguest.ToolHandler{
		"a":    delayValueTool(200*time.Millisecond, "A"),
		"b":    delayValueTool(50*time.Millisecond, "B"),
		"echo": echoTool,
	}
	er := runOneRequest(ctx, rt, wasmguest.Request{
		Source:                 source,
		Filename:               filename,
		Timeout:                timeout,
		Tools:                  tools,
		MaxConcurrentToolCalls: 2,
	})
	ar := asyncReport{Execution: er}
	var failures []string

	if er.Status != string(wasmguest.StatusCompleted) {
		failures = append(failures, fmt.Sprintf("async execution status %q", er.Status))
	}
	if len(er.Requests) != 2 {
		failures = append(failures, fmt.Sprintf("async requests = %d (want 2)", len(er.Requests)))
	}
	names := map[int64]string{}
	for _, req := range er.Requests {
		names[req.ID] = req.Name
	}
	if len(er.DispatchOrder) != 2 {
		failures = append(failures, fmt.Sprintf("dispatch order = %v (want 2 unique IDs)", er.DispatchOrder))
	}
	if len(er.CompletionOrder) != 2 {
		failures = append(failures, fmt.Sprintf("completion order = %v (want 2 unique IDs)", er.CompletionOrder))
	}
	if len(er.DeliveredOrder) != 2 {
		failures = append(failures, fmt.Sprintf("delivered order = %v (want 2 unique IDs)", er.DeliveredOrder))
	}
	if len(er.CompletionOrder) == 2 && names[er.CompletionOrder[0]] != "b" {
		failures = append(failures, fmt.Sprintf("completion order %v did not put b first", er.CompletionOrder))
	}
	if len(er.DeliveredOrder) == 2 && names[er.DeliveredOrder[0]] != "b" {
		failures = append(failures, fmt.Sprintf("delivered order %v did not put b first", er.DeliveredOrder))
	}
	if er.ResumeCount < 2 {
		failures = append(failures, fmt.Sprintf("resume count = %d (want >= 2)", er.ResumeCount))
	}
	if er.ToolCalls != 2 {
		failures = append(failures, fmt.Sprintf("tool calls = %d (want 2)", er.ToolCalls))
	}

	dispatched := map[int64]int{}
	for _, id := range er.DispatchOrder {
		dispatched[id]++
	}
	delivered := map[int64]int{}
	for _, id := range er.DeliveredOrder {
		delivered[id]++
	}
	for _, req := range er.Requests {
		if dispatched[req.ID] != 1 {
			failures = append(failures, fmt.Sprintf("call %d dispatched %d times", req.ID, dispatched[req.ID]))
		}
		if delivered[req.ID] != 1 {
			failures = append(failures, fmt.Sprintf("call %d delivered %d times", req.ID, delivered[req.ID]))
		}
	}
	for _, e := range er.CallLedger {
		if !e.Dispatched || !e.Delivered || !e.Completed {
			failures = append(failures, fmt.Sprintf("ledger call %d state %s dispatched=%v delivered=%v completed=%v", e.ID, e.State, e.Dispatched, e.Delivered, e.Completed))
		}
	}

	text := outputText(er.Outputs)
	ai := strings.Index(text, "A")
	bi := strings.Index(text, "B")
	if ai < 0 || bi < 0 || ai > bi {
		failures = append(failures, fmt.Sprintf("output does not show gather order A before B: %q", text))
	}

	ar.Checks = []string{
		"status completed",
		"two requests a and b",
		"b completed before a",
		"b delivered before a",
		"each call dispatched exactly once",
		"each call delivered exactly once",
		"resume count >= 2",
		"output preserves gather order A before B",
	}
	ar.OK = len(failures) == 0
	return ar, failures
}

func runAsyncCancel(ctx context.Context, rt *wasmguest.Runtime, source, filename string, timeout time.Duration) (asyncCancelReport, []string, bool) {
	cancelled := make(chan struct{})
	var once sync.Once
	tools := map[string]wasmguest.ToolHandler{
		"a": delayValueTool(10*time.Millisecond, "A"),
		"b": delayValueTool(10*time.Millisecond, "B"),
		"block": func(ctx context.Context, call wasmguest.ToolCall) (any, error) {
			<-ctx.Done()
			once.Do(func() { close(cancelled) })
			return nil, ctx.Err()
		},
	}
	er := runOneRequest(ctx, rt, wasmguest.Request{
		Source:                 source,
		Filename:               filename,
		Timeout:                timeout,
		Tools:                  tools,
		MaxConcurrentToolCalls: 1,
	})
	acr := asyncCancelReport{Execution: er}
	proven := er.Status == string(wasmguest.StatusTimedOut) || er.Status == string(wasmguest.StatusCancelled)
	acr.Proven = proven
	if !proven {
		acr.Note = "guest did not suspend with an outstanding tool call; async cancellation path not exercised"
		return acr, nil, false
	}

	var failures []string
	select {
	case <-cancelled:
		acr.HandlerCancelled = true
	case <-time.After(2 * time.Second):
		failures = append(failures, "tool handler context was not cancelled")
	}
	if er.ExportCalls.Invoke < 1 {
		failures = append(failures, fmt.Sprintf("async cancellation entered qp_invoke %d times (want >= 1)", er.ExportCalls.Invoke))
	}
	if er.ExportCalls.Free != 2*er.ExportCalls.Invoke {
		failures = append(failures, fmt.Sprintf("async cancellation free count %d != 2*invoke count %d (guest export after cancellation?)", er.ExportCalls.Free, er.ExportCalls.Invoke))
	}
	if er.SkippedFreeOnCancel {
		failures = append(failures, "async cancellation force-closed an active invoke; expected cancellation while suspended")
	}
	if !er.InvokeReturned {
		failures = append(failures, "async cancellation did not observe the active entry return")
	}
	var outstanding bool
	for _, e := range er.CallLedger {
		if e.Name == "block" && e.State == wasmguest.CallDispatched && !e.Completed && !e.Delivered {
			outstanding = true
		}
	}
	if !outstanding {
		failures = append(failures, "no outstanding block call remained at cancellation")
	}
	return acr, failures, true
}

func runStall(ctx context.Context, rt *wasmguest.Runtime, source, filename string, timeout time.Duration) (stallReport, []string, bool) {
	er := runOneRequest(ctx, rt, wasmguest.Request{
		Source:   source,
		Filename: filename,
		Timeout:  timeout,
	})
	sr := stallReport{Execution: er}
	proven := er.Status == string(wasmguest.StatusStalled)
	sr.Proven = proven
	if !proven {
		sr.Note = "guest did not return a terminal stalled suspension"
		return sr, nil, false
	}

	var failures []string
	if er.Error == nil || er.Error.Kind != string(wasmguest.KindStalled) {
		failures = append(failures, fmt.Sprintf("stall error kind = %v (want stalled)", er.Error))
	}
	if !er.Stalled {
		failures = append(failures, "stalled flag not set")
	}
	if er.ToolCalls != 0 {
		failures = append(failures, fmt.Sprintf("tool calls = %d (want 0)", er.ToolCalls))
	}
	if er.ExportCalls.Invoke != 1 {
		failures = append(failures, fmt.Sprintf("qp_invoke count = %d (want 1; no resume after stall)", er.ExportCalls.Invoke))
	}
	if er.ResumeCount != 0 {
		failures = append(failures, fmt.Sprintf("resume count = %d (want 0; no resume after stall)", er.ResumeCount))
	}
	if !er.InvokeReturned {
		failures = append(failures, "active entry did not return")
	}
	return sr, failures, true
}

func runSleep0(ctx context.Context, rt *wasmguest.Runtime, source, filename string, timeout time.Duration) (sleep0Report, []string) {
	er := runOneRequest(ctx, rt, wasmguest.Request{
		Source:   source,
		Filename: filename,
		Timeout:  timeout,
	})
	sr := sleep0Report{Execution: er}
	var failures []string
	if er.Status != string(wasmguest.StatusCompleted) {
		failures = append(failures, fmt.Sprintf("sleep0 status %q", er.Status))
	}
	text := outputText(er.Outputs)
	if !strings.Contains(text, "after sleep0") {
		failures = append(failures, fmt.Sprintf("sleep0 output missing marker: %q", text))
	}
	if er.ToolCalls != 0 {
		failures = append(failures, fmt.Sprintf("sleep0 tool calls = %d (want 0)", er.ToolCalls))
	}
	if er.ResumeCount > 0 {
		sr.Note = fmt.Sprintf("observed %d runnable resume(s); corrected guest was expected not to emit runnable", er.ResumeCount)
	}
	sr.OK = len(failures) == 0
	return sr, failures
}

func runCancelIsolation(ctx context.Context, rt *wasmguest.Runtime, runningSource, suspendedSource, otherSource, filename string, repeats int) ([]cancelIsolationReport, []string) {
	if repeats <= 0 {
		repeats = 1
	}
	var reports []cancelIsolationReport
	var failures []string
	for _, scenario := range []string{"running-loop", "suspended-tool"} {
		rep := cancelIsolationReport{Scenario: scenario, Repeats: repeats}
		for i := 0; i < repeats; i++ {
			sample, sampleFailures := runCancelIsolationSample(ctx, rt, scenario, runningSource, suspendedSource, otherSource, filename)
			rep.Samples = append(rep.Samples, sample)
			failures = append(failures, sampleFailures...)
		}
		rep.OK = true
		var sum float64
		for i, s := range rep.Samples {
			if !s.OK {
				rep.OK = false
			}
			if i == 0 || s.CancelLatencyMS < rep.MinMS {
				rep.MinMS = s.CancelLatencyMS
			}
			if s.CancelLatencyMS > rep.MaxMS {
				rep.MaxMS = s.CancelLatencyMS
			}
			sum += s.CancelLatencyMS
		}
		if len(rep.Samples) > 0 {
			rep.MeanMS = sum / float64(len(rep.Samples))
		}
		reports = append(reports, rep)
	}
	return reports, failures
}

func runCancelIsolationSample(ctx context.Context, rt *wasmguest.Runtime, scenario, runningSource, suspendedSource, otherSource, filename string) (cancelIsolationSample, []string) {
	sample := cancelIsolationSample{Scenario: scenario}
	var failures []string

	firstCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(5*time.Second, cancel)
	defer watchdog.Stop()

	handlerStarted := make(chan struct{})
	handlerCancelled := make(chan struct{})
	firstReq := wasmguest.Request{Source: runningSource, Filename: filename}
	if scenario == "suspended-tool" {
		firstReq = wasmguest.Request{
			Source:   suspendedSource,
			Filename: filename,
			Tools: map[string]wasmguest.ToolHandler{
				"block": func(ctx context.Context, call wasmguest.ToolCall) (any, error) {
					close(handlerStarted)
					<-ctx.Done()
					close(handlerCancelled)
					return nil, ctx.Err()
				},
			},
		}
	}

	firstCh := make(chan executionReport, 1)
	go func() {
		firstCh <- runOneRequest(firstCtx, rt, firstReq)
	}()

	if scenario == "running-loop" {
		time.Sleep(50 * time.Millisecond)
	} else {
		select {
		case <-handlerStarted:
		case <-time.After(2 * time.Second):
			failures = append(failures, fmt.Sprintf("%s block handler did not start", scenario))
			sample.OK = false
			return sample, failures
		}
	}

	otherCtx, otherCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otherCancel()
	other := runOneRequest(otherCtx, rt, wasmguest.Request{
		Source:   otherSource,
		Filename: filename,
		Tools: map[string]wasmguest.ToolHandler{
			"echo": echoTool,
		},
	})
	sample.Other = other
	if other.Status != string(wasmguest.StatusCompleted) {
		failures = append(failures, fmt.Sprintf("%s other status %q", scenario, other.Status))
	}
	if outputText(other.Outputs) != "unaffected\n" {
		failures = append(failures, fmt.Sprintf("%s other output %q", scenario, outputText(other.Outputs)))
	}

	cancelRequested := time.Now()
	cancel()
	var first executionReport
	select {
	case first = <-firstCh:
	case <-time.After(2 * time.Second):
		failures = append(failures, fmt.Sprintf("%s first execution did not return after cancel", scenario))
		sample.OK = false
		return sample, failures
	}
	sample.Canceled = first
	sample.CancelLatencyMS = float64(time.Since(cancelRequested).Microseconds()) / 1000.0

	if first.Status != string(wasmguest.StatusCancelled) {
		failures = append(failures, fmt.Sprintf("%s first status %q (want cancelled)", scenario, first.Status))
	}
	if first.Error == nil || first.Error.Kind != string(wasmguest.KindCancelled) {
		failures = append(failures, fmt.Sprintf("%s first error kind %v (want cancelled)", scenario, first.Error))
	}
	if !first.InvokeReturned {
		failures = append(failures, fmt.Sprintf("%s first InvokeReturned false", scenario))
	}
	if first.ExecutionID == other.ExecutionID || first.InstanceID == other.InstanceID {
		failures = append(failures, fmt.Sprintf("%s executions not distinct", scenario))
	}

	if scenario == "running-loop" {
		if first.ExportCalls.Invoke != 1 {
			failures = append(failures, fmt.Sprintf("%s first invoke count %d (want 1)", scenario, first.ExportCalls.Invoke))
		}
		if first.ExportCalls.Free != 0 {
			failures = append(failures, fmt.Sprintf("%s first free count %d (want 0; no post-cancel free)", scenario, first.ExportCalls.Free))
		}
		if !first.SkippedFreeOnCancel {
			failures = append(failures, fmt.Sprintf("%s first SkippedFreeOnCancel false", scenario))
		}
	} else {
		select {
		case <-handlerCancelled:
			sample.HandlerCancelled = true
		case <-time.After(2 * time.Second):
			failures = append(failures, fmt.Sprintf("%s block handler context not cancelled", scenario))
		}
		if first.ExportCalls.Invoke != 1 {
			failures = append(failures, fmt.Sprintf("%s first invoke count %d (want 1)", scenario, first.ExportCalls.Invoke))
		}
		if first.ExportCalls.Free != 2 {
			failures = append(failures, fmt.Sprintf("%s first free count %d (want 2 for the start invoke)", scenario, first.ExportCalls.Free))
		}
		if first.SkippedFreeOnCancel {
			failures = append(failures, fmt.Sprintf("%s first SkippedFreeOnCancel true (want false)", scenario))
		}
		if first.ToolCalls != 1 {
			failures = append(failures, fmt.Sprintf("%s first tool calls %d (want 1)", scenario, first.ToolCalls))
		}
		if len(first.CallLedger) != 1 || first.CallLedger[0].State != wasmguest.CallDispatched || first.CallLedger[0].Completed || first.CallLedger[0].Delivered {
			failures = append(failures, fmt.Sprintf("%s first ledger %+v", scenario, first.CallLedger))
		}
	}

	if sample.CancelLatencyMS >= 250 {
		failures = append(failures, fmt.Sprintf("%s cancel latency %.2fms >= 250ms", scenario, sample.CancelLatencyMS))
	}
	sample.OK = len(failures) == 0
	return sample, failures
}

func runMeasure(ctx context.Context, artifact string, maxMemory int64, maxMessage, maxOutput int, timeout time.Duration, samples, suspendedN int) (measureReport, []string) {
	rep := measureReport{}
	var failures []string
	if info, err := os.Stat(artifact); err == nil {
		rep.ArtifactSize = info.Size()
	}
	if samples <= 0 {
		samples = 1
	}
	opts := wasmguest.Options{
		ArtifactPath:    artifact,
		MaxMemoryBytes:  maxMemory,
		MaxMessageBytes: maxMessage,
		MaxOutputBytes:  maxOutput,
		DefaultTimeout:  timeout,
	}

	for i := 0; i < samples; i++ {
		start := time.Now()
		rt, err := wasmguest.NewRuntime(ctx, opts)
		if err != nil {
			failures = append(failures, fmt.Sprintf("compile sample %d: %v", i+1, err))
			break
		}
		rep.CompileMS = append(rep.CompileMS, float64(time.Since(start).Microseconds())/1000.0)
		_ = rt.Close(context.Background())
	}

	rt, err := wasmguest.NewRuntime(ctx, opts)
	if err != nil {
		failures = append(failures, fmt.Sprintf("startup runtime: %v", err))
		return rep, failures
	}
	defer func() { _ = rt.Close(context.Background()) }()

	startup, err := rt.MeasureStartup(ctx, samples)
	if err != nil {
		failures = append(failures, fmt.Sprintf("startup: %v", err))
	} else {
		for _, d := range startup {
			rep.StartupMS = append(rep.StartupMS, float64(d.Microseconds())/1000.0)
		}
	}

	if suspendedN <= 0 {
		suspendedN = 1
	}
	sample, sampleFailures := measureSuspended(ctx, rt, suspendedN)
	rep.Suspended = append(rep.Suspended, sample)
	failures = append(failures, sampleFailures...)

	rep.CompileMinMS, rep.CompileMeanMS, rep.CompileMaxMS = stats(rep.CompileMS)
	rep.StartupMinMS, rep.StartupMeanMS, rep.StartupMaxMS = stats(rep.StartupMS)
	rep.Notes = append(rep.Notes,
		"compile_ms measures NewRuntime (read + compile + WASI prelude + import check), not just wasm compilation",
		"startup_ms measures warm instantiation + _initialize only; no start message, source, or tool work",
		"guest memory is exact linear-memory pages/bytes at suspension; RSS is process-level and approximate",
	)
	return rep, failures
}

func measureSuspended(ctx context.Context, rt *wasmguest.Runtime, n int) (suspendedMemorySample, []string) {
	sample := suspendedMemorySample{N: n}
	var failures []string
	baseline, baselineOK := processRSSKiB()
	if baselineOK {
		sample.RSSBaselineKiB = baseline
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := make(chan struct{}, n)
	reports := make([]executionReport, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reports[i] = runOneRequest(runCtx, rt, wasmguest.Request{
				Source:   defaultCancelIsolationSuspendedSource,
				Filename: "main.py",
				Tools: map[string]wasmguest.ToolHandler{
					"block": func(ctx context.Context, call wasmguest.ToolCall) (any, error) {
						started <- struct{}{}
						<-ctx.Done()
						return nil, ctx.Err()
					},
				},
			})
		}(i)
	}

	for i := 0; i < n; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			failures = append(failures, fmt.Sprintf("suspended n=%d: handler start timeout", n))
			cancel()
			wg.Wait()
			return sample, failures
		}
	}

	time.Sleep(100 * time.Millisecond)
	if rss, ok := processRSSKiB(); ok {
		sample.RSSSuspendedKiB = rss
		sample.RSSDeltaKiB = rss - baseline
		if n > 0 {
			sample.RSSPerInstanceKiB = float64(sample.RSSDeltaKiB) / float64(n)
		}
	} else {
		failures = append(failures, fmt.Sprintf("suspended n=%d: process RSS unavailable", n))
	}

	cancel()
	wg.Wait()

	var pages []uint32
	for _, r := range reports {
		if r.Status != string(wasmguest.StatusCancelled) {
			failures = append(failures, fmt.Sprintf("suspended n=%d: execution status %q", n, r.Status))
		}
		pages = append(pages, r.MemoryPages)
	}
	if len(pages) > 0 {
		sample.MinPages = pages[0]
		sample.MaxPages = pages[0]
		var sum uint64
		for _, p := range pages {
			if p < sample.MinPages {
				sample.MinPages = p
			}
			if p > sample.MaxPages {
				sample.MaxPages = p
			}
			sum += uint64(p)
		}
		sample.MeanPages = float64(sum) / float64(len(pages))
		sample.MinBytes = int64(sample.MinPages) * 65536
		sample.MaxBytes = int64(sample.MaxPages) * 65536
		sample.MeanBytes = sample.MeanPages * 65536
	}
	return sample, failures
}

func processRSSKiB() (int64, bool) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(out))
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func stats(v []float64) (min, mean, max float64) {
	if len(v) == 0 {
		return 0, 0, 0
	}
	min, max = v[0], v[0]
	var sum float64
	for _, x := range v {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
		sum += x
	}
	return min, sum / float64(len(v)), max
}

func delayValueTool(defaultDelay time.Duration, defaultValue string) wasmguest.ToolHandler {
	return func(ctx context.Context, call wasmguest.ToolCall) (any, error) {
		value := defaultValue
		if v, ok := call.Args["value"].(string); ok {
			value = v
		}
		delay := defaultDelay
		if ms, ok := intArg(call.Args["delay_ms"]); ok {
			delay = time.Duration(ms) * time.Millisecond
		}
		select {
		case <-time.After(delay):
			return value, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func echoTool(ctx context.Context, call wasmguest.ToolCall) (any, error) {
	return call.Args["value"], nil
}

func intArg(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	default:
		return 0, false
	}
}

func outputText(outputs []wasmguest.Output) string {
	var b strings.Builder
	for _, o := range outputs {
		b.WriteString(o.Text)
	}
	return b.String()
}

func printHuman(rep *report, verbose bool) {
	fmt.Printf("artifact: %s\n", rep.Artifact)
	fmt.Printf("imports (%d):\n", len(rep.Imports))
	for _, imp := range rep.Imports {
		fmt.Printf("  %s %s.%s\n", imp.Kind, imp.Module, imp.Name)
	}
	if len(rep.Imports) == 0 {
		fmt.Println("  (none)")
	}

	if rep.Single != nil {
		fmt.Println("\n[single]")
		printExecution(*rep.Single, verbose)
	}
	if len(rep.Sequential) > 0 {
		fmt.Println("\n[sequential]")
		for _, er := range rep.Sequential {
			printExecution(er, verbose)
		}
	}
	if len(rep.Concurrent) > 0 {
		fmt.Println("\n[concurrent]")
		for _, er := range rep.Concurrent {
			printExecution(er, verbose)
		}
	}
	if rep.Cancellation != nil {
		fmt.Println("\n[cancellation]")
		printExecution(rep.Cancellation.Execution, verbose)
		if rep.Cancellation.Proven {
			fmt.Println("  proven: true (active entry force-closed; no guest export after cancellation)")
		} else {
			fmt.Println("  proven: false (UNPROVEN)")
			if rep.Cancellation.Note != "" {
				fmt.Printf("  note: %s\n", rep.Cancellation.Note)
			}
		}
	}
	if rep.Async != nil {
		fmt.Println("\n[async]")
		printExecution(rep.Async.Execution, verbose)
		printAsyncEvidence(rep.Async.Execution)
		fmt.Printf("  async proof: %v\n", rep.Async.OK)
		for _, check := range rep.Async.Checks {
			fmt.Printf("    check: %s\n", check)
		}
	}
	if rep.AsyncCancel != nil {
		fmt.Println("\n[async-cancel]")
		printExecution(rep.AsyncCancel.Execution, verbose)
		fmt.Printf("  proven: %v  handler_cancelled: %v\n", rep.AsyncCancel.Proven, rep.AsyncCancel.HandlerCancelled)
		if rep.AsyncCancel.Note != "" {
			fmt.Printf("  note: %s\n", rep.AsyncCancel.Note)
		}
	}
	if rep.Stall != nil {
		fmt.Println("\n[stall]")
		printExecution(rep.Stall.Execution, verbose)
		fmt.Printf("  proven: %v\n", rep.Stall.Proven)
		if rep.Stall.Note != "" {
			fmt.Printf("  note: %s\n", rep.Stall.Note)
		}
	}
	if rep.Sleep0 != nil {
		fmt.Println("\n[sleep0]")
		printExecution(rep.Sleep0.Execution, verbose)
		fmt.Printf("  sleep0 proof: %v\n", rep.Sleep0.OK)
		if rep.Sleep0.Note != "" {
			fmt.Printf("  note: %s\n", rep.Sleep0.Note)
		}
	}
	for _, ci := range rep.CancelIsolation {
		fmt.Printf("\n[cancel-isolation:%s]\n", ci.Scenario)
		fmt.Printf("  repeats: %d  min: %.2fms  mean: %.2fms  max: %.2fms  ok: %v\n",
			ci.Repeats, ci.MinMS, ci.MeanMS, ci.MaxMS, ci.OK)
		for i, s := range ci.Samples {
			fmt.Printf("  sample %d: latency %.2fms  canceled=%s/%s  other=%s/%s  handler_cancelled=%v  ok=%v\n",
				i+1, s.CancelLatencyMS,
				s.Canceled.ExecutionID, s.Canceled.InstanceID,
				s.Other.ExecutionID, s.Other.InstanceID,
				s.HandlerCancelled, s.OK)
			if verbose {
				printExecution(s.Canceled, true)
				printExecution(s.Other, true)
			}
		}
	}
	if rep.Measure != nil {
		m := rep.Measure
		fmt.Println("\n[measure]")
		fmt.Printf("  artifact_size: %d bytes\n", m.ArtifactSize)
		fmt.Printf("  compile_ms: n=%d min=%.2f mean=%.2f max=%.2f\n", len(m.CompileMS), m.CompileMinMS, m.CompileMeanMS, m.CompileMaxMS)
		fmt.Printf("  startup_ms: n=%d min=%.2f mean=%.2f max=%.2f\n", len(m.StartupMS), m.StartupMinMS, m.StartupMeanMS, m.StartupMaxMS)
		for _, s := range m.Suspended {
			fmt.Printf("  suspended n=%d: pages min=%.0f mean=%.2f max=%.0f bytes min=%d mean=%.0f max=%d rss_delta_kib=%d rss_per_instance_kib=%.1f\n",
				s.N, float64(s.MinPages), s.MeanPages, float64(s.MaxPages), s.MinBytes, s.MeanBytes, s.MaxBytes, s.RSSDeltaKiB, s.RSSPerInstanceKiB)
		}
		for _, note := range m.Notes {
			fmt.Printf("  note: %s\n", note)
		}
	}

	for _, note := range rep.Notes {
		fmt.Printf("\nnote: %s\n", note)
	}
	if rep.OK {
		fmt.Println("\nOK")
	} else {
		fmt.Println("\nFAILED")
		for _, f := range rep.Failures {
			fmt.Printf("  - %s\n", f)
		}
	}
}

func printAsyncEvidence(er executionReport) {
	fmt.Printf("  requests: %d  tool_calls: %d  resume_count: %d\n", len(er.Requests), er.ToolCalls, er.ResumeCount)
	fmt.Printf("  dispatch_order: %v\n", er.DispatchOrder)
	fmt.Printf("  completion_order: %v\n", er.CompletionOrder)
	fmt.Printf("  delivered_order: %v\n", er.DeliveredOrder)
	for _, e := range er.CallLedger {
		fmt.Printf("  ledger: id=%d name=%s state=%s dispatched=%v delivered=%v completed=%v error=%q\n",
			e.ID, e.Name, e.State, e.Dispatched, e.Delivered, e.Completed, e.Error)
	}
}

func printExecution(er executionReport, verbose bool) {
	fmt.Printf("  execution: %s  instance: %s  status: %s  duration: %.2fms  memory_pages: %d\n",
		er.ExecutionID, er.InstanceID, er.Status, er.DurationMS, er.MemoryPages)
	if er.Error != nil {
		fmt.Printf("    error: %s: %s\n", er.Error.Kind, er.Error.Message)
		if er.Error.Exception != nil {
			fmt.Printf("    exception: %s: %s\n", er.Error.Exception.Type, er.Error.Exception.Message)
		}
	}
	if er.Exception != nil {
		fmt.Printf("    exception: %s: %s\n", er.Exception.Type, er.Exception.Message)
	}
	for _, out := range er.Outputs {
		fmt.Printf("    output[%s]: %s\n", out.Type, out.Text)
	}
	if er.CapturedStdout != "" {
		fmt.Printf("    stdout: %q\n", er.CapturedStdout)
	}
	if er.CapturedStderr != "" {
		fmt.Printf("    stderr: %q\n", er.CapturedStderr)
	}
	if er.StdoutTruncated || er.StderrTruncated {
		fmt.Printf("    truncated: stdout=%v stderr=%v\n", er.StdoutTruncated, er.StderrTruncated)
	}
	if er.OutputTruncated {
		fmt.Println("    output_truncated: true")
	}
	if er.Stalled {
		fmt.Println("    stalled: true")
	}
	if len(er.UnknownFields) > 0 {
		fmt.Printf("    unknown_fields: %v\n", er.UnknownFields)
	}
	if verbose {
		fmt.Printf("    invoke_returned: %v  skipped_free_on_cancel: %v\n", er.InvokeReturned, er.SkippedFreeOnCancel)
		fmt.Printf("    export_calls: init=%d abi=%d alloc=%d invoke=%d free=%d\n",
			er.ExportCalls.Initialize, er.ExportCalls.ABI, er.ExportCalls.Alloc, er.ExportCalls.Invoke, er.ExportCalls.Free)
		for _, line := range er.Trace {
			fmt.Printf("    trace: %s\n", line)
		}
	}
}
