package integration_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/keejkrej/quickpy"
)

func TestOutputTruncationDeterministic(t *testing.T) {
	const maxOutput = 15
	source := `import asyncio, quickpy
quickpy.set_state({"x": 1})
print("A" * 10)
async def main():
    await quickpy.tools.echo(value=1)
    print("B" * 10)
    await quickpy.tools.echo(value=2)
    print("C" * 10)
quickpy.run(main())`

	var first []quickpy.Event
	for i := 0; i < 3; i++ {
		var rec eventRecorder
		r := newRuntime(t, quickpy.RuntimeOptions{
			MaxOutputBytes: 1 << 20,
			EventSink:      rec.sink,
		})
		if err := r.RegisterTool("echo", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source:         source,
			MaxOutputBytes: maxOutput,
		}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)

		want := textEvents("AAAAAAAAAA\n", "BBBB")
		requireEvents(t, res.Outputs, want)
		if got := outputBytes(res.Outputs); got > maxOutput {
			t.Fatalf("run %d output bytes = %d, want <= %d", i, got, maxOutput)
		}
		sinkOutputs := outputEvents(rec.snapshot())
		requireEvents(t, sinkOutputs, res.Outputs)
		if i == 0 {
			first = res.Outputs
		} else if !reflect.DeepEqual(first, res.Outputs) {
			t.Fatalf("run %d outputs = %#v, want deterministic prefix %#v", i, res.Outputs, first)
		}
	}
}

func TestOutputTruncationSingleDelta(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{MaxOutputBytes: 1 << 20})
	res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
		Source:         `print("X" * 100)`,
		MaxOutputBytes: 10,
	}))
	requireErrorKind(t, err, quickpy.KindLimitExceeded)
	requireStatus(t, res, quickpy.StatusLimitExceeded)
	requireNoState(t, res)
	requireEvents(t, res.Outputs, textEvents("XXXXXXXXXX"))
	if got := outputBytes(res.Outputs); got > 10 {
		t.Fatalf("output bytes = %d, want <= 10", got)
	}
}
