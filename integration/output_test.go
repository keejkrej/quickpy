package integration_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

// requireScriptTypeIn asserts that an uncaught guest exception is a
// ScriptError with one of the expected Python exception types. The guest may
// map an invalid image argument to TypeError or ValueError; both are clear
// script errors, not host faults.
func requireScriptTypeIn(t *testing.T, qe *quickpy.Error, want ...string) {
	t.Helper()
	if qe.Script == nil {
		t.Fatalf("Script = nil, want one of %v", want)
	}
	for _, w := range want {
		if qe.Script.Type == w {
			return
		}
	}
	t.Fatalf("Script.Type = %q, want one of %v (message: %q)", qe.Script.Type, want, qe.Script.Message)
}

// TestConsoleLevelsAndStringification covers all four console levels and the
// print-like stringification of multiple arguments. The final guest notes
// document the exact str()/single-space rule; console text has no trailing
// newline (unlike print). This test asserts that rule.
func TestConsoleLevelsAndStringification(t *testing.T) {
	requireABI4(t)

	var rec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{EventSink: rec.sink})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.console.debug("d", 1, True, None)
quickpy.console.info("i", 2.5)
quickpy.console.warn("w")
quickpy.console.error("e", "x")
print(quickpy.console.info("ret") is None)`}))
	requireStatus(t, res, quickpy.StatusCompleted)

	want := []quickpy.Event{
		quickpy.ConsoleEvent{Level: "debug", Text: "d 1 True None"},
		quickpy.ConsoleEvent{Level: "info", Text: "i 2.5"},
		quickpy.ConsoleEvent{Level: "warn", Text: "w"},
		quickpy.ConsoleEvent{Level: "error", Text: "e x"},
		quickpy.ConsoleEvent{Level: "info", Text: "ret"},
		quickpy.TextEvent{Text: "True\n"},
	}
	requireEvents(t, outputEvents(rec.snapshot()), want)
	requireEvents(t, res.Outputs, want)
	if got := textOf(res.Outputs); got != "True\n" {
		t.Fatalf("text output = %q, want only the print result", got)
	}
}

// TestConsoleStrictLevels covers the strict level surface: only debug, info,
// warn, and error are exposed. There is no generic log/trace entry point that
// could carry an arbitrary level token.
func TestConsoleStrictLevels(t *testing.T) {
	requireABI4(t)

	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
for name in ("debug", "info", "warn", "error"):
    print(name, callable(getattr(quickpy.console, name)))
for name in ("log", "trace"):
    try:
        getattr(quickpy.console, name)
        print(name, "present")
    except AttributeError:
        print(name, "AttributeError")`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	out := textOf(res.Outputs)
	for _, name := range []string{"debug", "info", "warn", "error"} {
		requireContains(t, out, name+" True")
	}
	requireContains(t, out, "log AttributeError")
	requireContains(t, out, "trace AttributeError")
}

// TestConsoleInterleavedWithPrint covers the precise ordered sink and
// Result.Outputs for print/console/print. The console text must not be
// duplicated into the legacy text stream.
func TestConsoleInterleavedWithPrint(t *testing.T) {
	requireABI4(t)

	var rec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{EventSink: rec.sink})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
print("before")
quickpy.console.info("mid")
print("after")`}))
	requireStatus(t, res, quickpy.StatusCompleted)

	want := []quickpy.Event{
		quickpy.TextEvent{Text: "before\n"},
		quickpy.ConsoleEvent{Level: "info", Text: "mid"},
		quickpy.TextEvent{Text: "after\n"},
	}
	requireEvents(t, outputEvents(rec.snapshot()), want)
	requireEvents(t, res.Outputs, want)
	if got := textOf(res.Outputs); got != "before\nafter\n" {
		t.Fatalf("text output = %q, want no duplicate console text", got)
	}
}

// TestImageRawRoundtrip covers exact raw byte round-trip, including invalid
// UTF-8 and NUL bytes. The image channel is binary, not a JSON string.
func TestImageRawRoundtrip(t *testing.T) {
	requireABI4(t)

	var rec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20, EventSink: rec.sink})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
data = bytes([0, 1, 2, 255, 81, 80])
print(quickpy.image(data, mime="image/png") is None)`}))
	requireStatus(t, res, quickpy.StatusCompleted)

	want := []quickpy.Event{
		quickpy.ImageEvent{MIME: "image/png", Data: []byte{0, 1, 2, 0xff, 'Q', 'P'}},
		quickpy.TextEvent{Text: "True\n"},
	}
	requireEvents(t, outputEvents(rec.snapshot()), want)
	requireEvents(t, res.Outputs, want)
}

// TestImageDefaultMIME covers the documented default MIME.
func TestImageDefaultMIME(t *testing.T) {
	requireABI4(t)

	r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.image(bytes([7, 8]))`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	want := []quickpy.Event{quickpy.ImageEvent{MIME: "image/png", Data: []byte{7, 8}}}
	requireEvents(t, res.Outputs, want)
}

// TestImageBytearrayCaptureCopy covers bytearray input and the capture-copy
// rule: mutating the caller's buffer after the call must not change the
// already-emitted image.
func TestImageBytearrayCaptureCopy(t *testing.T) {
	requireABI4(t)

	r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
buf = bytearray([1, 2, 3])
quickpy.image(buf, mime="image/png")
buf[0] = 99
buf.append(4)
quickpy.image(buf, mime="image/png")`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	want := []quickpy.Event{
		quickpy.ImageEvent{MIME: "image/png", Data: []byte{1, 2, 3}},
		quickpy.ImageEvent{MIME: "image/png", Data: []byte{99, 2, 3, 4}},
	}
	requireEvents(t, res.Outputs, want)
}

// TestImageInvalidDataAndMIME covers invalid data and MIME forms. Each must be
// a clear catchable script error (TypeError or ValueError), never a host fault
// or protocol error, and must not commit state.
func TestImageInvalidDataAndMIME(t *testing.T) {
	requireABI4(t)

	cases := []struct {
		name   string
		source string
	}{
		{"string data", `import quickpy
quickpy.image("text")`},
		{"none data", `import quickpy
quickpy.image(None)`},
		{"int data", `import quickpy
quickpy.image(123)`},
		{"list data", `import quickpy
quickpy.image([1, 2])`},
		{"mime no slash", `import quickpy
quickpy.image(bytes([1]), mime="png")`},
		{"mime empty subtype", `import quickpy
quickpy.image(bytes([1]), mime="image/")`},
		{"mime empty type", `import quickpy
quickpy.image(bytes([1]), mime="/png")`},
		{"mime extra segment", `import quickpy
quickpy.image(bytes([1]), mime="image/png/extra")`},
		{"mime parameters", `import quickpy
quickpy.image(bytes([1]), mime="image/png; charset=utf-8")`},
		{"mime empty", `import quickpy
quickpy.image(bytes([1]), mime="")`},
		{"mime non-string", `import quickpy
quickpy.image(bytes([1]), mime=123)`},
	}
	r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: tc.source}))
			qe := requireErrorKind(t, err, quickpy.KindScriptError)
			requireStatus(t, res, quickpy.StatusFailed)
			requireNoState(t, res)
			requireScriptTypeIn(t, qe, "TypeError", "ValueError")
			if qe.Kind == quickpy.KindHostFault || qe.Kind == quickpy.KindProtocolError {
				t.Fatalf("invalid image input mapped to %s, want a clear script error", qe.Kind)
			}
		})
	}
}

// TestImageInvalidCatchable proves the invalid image errors are ordinary
// Python exceptions: the script can catch them and continue.
func TestImageInvalidCatchable(t *testing.T) {
	requireABI4(t)

	r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
try:
    quickpy.image("text")
except (TypeError, ValueError) as e:
    print("caught", str(type(e)))
try:
    quickpy.image(bytes([1]), mime="png")
except (TypeError, ValueError) as e:
    print("caught", str(type(e)))
print("after")`}))
	requireStatus(t, res, quickpy.StatusCompleted)
	lines := strings.Split(strings.TrimSpace(textOf(res.Outputs)), "\n")
	if len(lines) != 3 {
		t.Fatalf("output lines = %#v, want 3", lines)
	}
	if !strings.Contains(lines[0], "TypeError") {
		t.Fatalf("data error line = %q, want TypeError", lines[0])
	}
	if !strings.Contains(lines[1], "ValueError") {
		t.Fatalf("MIME error line = %q, want ValueError", lines[1])
	}
	if lines[2] != "after" {
		t.Fatalf("last line = %q, want after", lines[2])
	}
}

// TestOrderedOutputAcrossAsyncSuspensions covers print/console/image/tool
// start/response cycles across async suspensions. The output chunks are
// stable because console/image separate the text; the test asserts semantic
// ordered chunks rather than a fixed number of arbitrary text events.
func TestOrderedOutputAcrossAsyncSuspensions(t *testing.T) {
	requireABI4(t)

	var rec eventRecorder
	r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20, EventSink: rec.sink})
	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
print("p0")
quickpy.console.info("c0")
quickpy.image(bytes([0, 1]), mime="image/png")
async def main():
    print("p1")
    quickpy.console.warn("c1")
    quickpy.image(bytes([2, 3]), mime="image/png")
    r = await quickpy.tools.echo(value=1)
    print("p2", r)
    quickpy.console.error("c2")
    quickpy.image(bytes([4, 5]), mime="image/png")
quickpy.run(main())`}))
	requireStatus(t, res, quickpy.StatusCompleted)

	wantOutputs := []quickpy.Event{
		quickpy.TextEvent{Text: "p0\n"},
		quickpy.ConsoleEvent{Level: "info", Text: "c0"},
		quickpy.ImageEvent{MIME: "image/png", Data: []byte{0, 1}},
		quickpy.TextEvent{Text: "p1\n"},
		quickpy.ConsoleEvent{Level: "warn", Text: "c1"},
		quickpy.ImageEvent{MIME: "image/png", Data: []byte{2, 3}},
		quickpy.TextEvent{Text: "p2 ok\n"},
		quickpy.ConsoleEvent{Level: "error", Text: "c2"},
		quickpy.ImageEvent{MIME: "image/png", Data: []byte{4, 5}},
	}
	requireEvents(t, outputEvents(rec.snapshot()), wantOutputs)
	requireEvents(t, res.Outputs, wantOutputs)
	if got := textOf(res.Outputs); got != "p0\np1\np2 ok\n" {
		t.Fatalf("text output = %q", got)
	}

	var toolPhases []string
	for _, ev := range rec.snapshot() {
		if te, ok := ev.(quickpy.ToolEvent); ok && te.Name == "echo" {
			toolPhases = append(toolPhases, te.Phase)
		}
	}
	if !reflect.DeepEqual(toolPhases, []string{"started", "finished"}) {
		t.Fatalf("tool phases = %v, want [started finished]", toolPhases)
	}
}

// TestOutputCapCountsDecodedImageBytes covers the cumulative output cap. The
// cap counts text bytes, console text bytes, and decoded image bytes, never
// the raw JSON/base64 characters. The retained image is a raw byte prefix; a
// truncated prefix is not a valid image and the test does not assert that it
// is a formed compressor output.
func TestOutputCapCountsDecodedImageBytes(t *testing.T) {
	requireABI4(t)

	t.Run("text console and image", func(t *testing.T) {
		const maxOutput = 10
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{
			MaxMessageBytes: 1 << 20,
			MaxOutputBytes:  1 << 20,
			EventSink:       rec.sink,
		})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: `import quickpy
print("AAAA")
quickpy.console.info("BB")
quickpy.image(bytes([1, 2, 3, 4, 5]), mime="image/png")`,
			MaxOutputBytes: maxOutput,
		}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)

		want := []quickpy.Event{
			quickpy.TextEvent{Text: "AAAA\n"},
			quickpy.ConsoleEvent{Level: "info", Text: "BB"},
			quickpy.ImageEvent{MIME: "image/png", Data: []byte{1, 2, 3}},
		}
		requireEvents(t, res.Outputs, want)
		requireEvents(t, outputEvents(rec.snapshot()), want)
		if got := outputBytes(res.Outputs); got != maxOutput {
			t.Fatalf("output bytes = %d, want %d", got, maxOutput)
		}
	})

	t.Run("single image exact cap", func(t *testing.T) {
		const maxOutput = 6
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{
			MaxMessageBytes: 1 << 20,
			MaxOutputBytes:  1 << 20,
			EventSink:       rec.sink,
		})
		res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: `import quickpy
quickpy.image(bytes([0, 1, 2, 255, 81, 80]), mime="image/png")`,
			MaxOutputBytes: maxOutput,
		}))
		requireStatus(t, res, quickpy.StatusCompleted)
		want := []quickpy.Event{
			quickpy.ImageEvent{MIME: "image/png", Data: []byte{0, 1, 2, 0xff, 'Q', 'P'}},
		}
		requireEvents(t, res.Outputs, want)
		requireEvents(t, outputEvents(rec.snapshot()), want)
	})

	t.Run("single image over cap partial prefix", func(t *testing.T) {
		const maxOutput = 3
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{
			MaxMessageBytes: 1 << 20,
			MaxOutputBytes:  1 << 20,
			EventSink:       rec.sink,
		})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: `import quickpy
quickpy.image(bytes([0, 1, 2, 255, 81, 80]), mime="image/png")`,
			MaxOutputBytes: maxOutput,
		}))
		// A valid single image larger than the total cap is decoded under
		// MaxMessageBytes and then clipped to a raw byte prefix, exactly like
		// cumulative overflow. The prefix is not a valid image; the test does
		// not assert that it is a formed compressor output.
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
		want := []quickpy.Event{
			quickpy.ImageEvent{MIME: "image/png", Data: []byte{0, 1, 2}},
		}
		requireEvents(t, res.Outputs, want)
		requireEvents(t, outputEvents(rec.snapshot()), want)
		if got := outputBytes(res.Outputs); got != maxOutput {
			t.Fatalf("output bytes = %d, want %d", got, maxOutput)
		}
	})

	t.Run("single image tiny cap", func(t *testing.T) {
		const maxOutput = 1
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{
			MaxMessageBytes: 1 << 20,
			MaxOutputBytes:  1 << 20,
			EventSink:       rec.sink,
		})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: `import quickpy
quickpy.image(bytes([0, 1, 2, 255, 81, 80]), mime="image/png")`,
			MaxOutputBytes: maxOutput,
		}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
		want := []quickpy.Event{
			quickpy.ImageEvent{MIME: "image/png", Data: []byte{0}},
		}
		requireEvents(t, res.Outputs, want)
		requireEvents(t, outputEvents(rec.snapshot()), want)
	})

	t.Run("text then image", func(t *testing.T) {
		const maxOutput = 7
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{
			MaxMessageBytes: 1 << 20,
			MaxOutputBytes:  1 << 20,
			EventSink:       rec.sink,
		})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: `import quickpy
print("AAAA")
quickpy.image(bytes([0, 1, 2, 255, 81, 80]), mime="image/png")`,
			MaxOutputBytes: maxOutput,
		}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)

		want := []quickpy.Event{
			quickpy.TextEvent{Text: "AAAA\n"},
			quickpy.ImageEvent{MIME: "image/png", Data: []byte{0, 1}},
		}
		requireEvents(t, res.Outputs, want)
		requireEvents(t, outputEvents(rec.snapshot()), want)
	})
}

// TestPartialOutputsOnFailureStallCancel covers the terminal failure paths:
// the ordered outputs produced before the failure remain available, the sink
// and Result.Outputs agree, and no state is committed.
func TestPartialOutputsOnFailureStallCancel(t *testing.T) {
	requireABI4(t)

	prefix := `import quickpy
print("p")
quickpy.console.info("c")
quickpy.image(bytes([0, 1]), mime="image/png")
`
	wantPrefix := []quickpy.Event{
		quickpy.TextEvent{Text: "p\n"},
		quickpy.ConsoleEvent{Level: "info", Text: "c"},
		quickpy.ImageEvent{MIME: "image/png", Data: []byte{0, 1}},
	}

	t.Run("failed", func(t *testing.T) {
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20, EventSink: rec.sink})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: prefix + `raise ValueError("boom")`}))
		requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
		requireEvents(t, outputEvents(res.Outputs), wantPrefix)
		requireEvents(t, outputEvents(rec.snapshot()), wantPrefix)
	})

	t.Run("stall", func(t *testing.T) {
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20, EventSink: rec.sink})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: prefix + `import asyncio
async def main():
    await asyncio.Event().wait()
quickpy.run(main())`}))
		requireErrorKind(t, err, quickpy.KindStalled)
		requireStatus(t, res, quickpy.StatusStalled)
		requireNoState(t, res)
		requireEvents(t, outputEvents(res.Outputs), wantPrefix)
		requireEvents(t, outputEvents(rec.snapshot()), wantPrefix)
	})

	t.Run("cancel", func(t *testing.T) {
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{MaxMessageBytes: 1 << 20})
		e, resumed := startLoopingExecution(t, r, nil, prefix, rec.sink)
		waitFor(t, resumed, "tool finished")
		time.Sleep(20 * time.Millisecond)
		e.Cancel()
		res, err := wait(t, e)
		requireErrorKind(t, err, quickpy.KindCancelled)
		requireStatus(t, res, quickpy.StatusCancelled)
		requireNoState(t, res)
		got := outputEvents(res.Outputs)
		requireEvents(t, got, wantPrefix)
		requireEvents(t, outputEvents(rec.snapshot()), got)
	})
}

// TestSinkPanicRecovered proves a panicking event sink is recovered and
// classified as HostFault; it must not escape Wait. The runtime remains usable
// with a non-panicking per-execution sink, including tool calls.
func TestSinkPanicRecovered(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{
		EventSink: func(ev quickpy.Event) { panic("sink boom") },
	})
	res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("x")`}))
	requireErrorKind(t, err, quickpy.KindHostFault)
	requireStatus(t, res, quickpy.StatusFailed)
	requireNoState(t, res)

	if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	res = mustWait(t, execute(t, r, quickpy.ExecuteRequest{
		Source: `import asyncio, quickpy
async def main():
    r = await quickpy.tools.echo(value=1)
    print("healthy", r)
quickpy.run(main())`,
		EventSink: func(ev quickpy.Event) {},
	}))
	requireStatus(t, res, quickpy.StatusCompleted)
	requireContains(t, textOf(res.Outputs), "healthy ok")
}
