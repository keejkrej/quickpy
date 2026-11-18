package integration_test

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
)

func TestArtifactIdentity(t *testing.T) {
	size, sum := artifactEvidence(t)
	t.Logf("artifact: %d bytes sha256=%s", size, sum)
	if size < 100_000 {
		t.Fatalf("artifact size = %d, want a real MicroPython artifact (>= 100000 bytes)", size)
	}
	if len(sum) != 64 {
		t.Fatalf("sha256 = %q, want 64 hex characters", sum)
	}
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("ok")`}))
	if res.Guest.ABIVersion != 4 {
		t.Fatalf("ABI version = %d, want 4", res.Guest.ABIVersion)
	}
	if res.Guest.Engine != "micropython" {
		t.Fatalf("engine = %q, want micropython", res.Guest.Engine)
	}
	if res.Guest.Version != "1.29.0" {
		t.Fatalf("guest version = %q, want 1.29.0", res.Guest.Version)
	}
	if res.Guest.ProtocolVersion != "0" {
		t.Fatalf("protocol version = %q, want 0", res.Guest.ProtocolVersion)
	}
	if len(res.Guest.Imports) != 0 {
		t.Fatalf("imports = %+v, want none", res.Guest.Imports)
	}
}

func TestStateInitialIntFloat(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	initial := map[string]any{
		"i":     int64(1),
		"f":     float64(1.0),
		"n":     1.0000000000000002,
		"small": 0.1,
		"large": 123456789.123456789,
	}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{
		Source: `import quickpy
s = quickpy.get_state()
print(isinstance(s["i"], int), isinstance(s["f"], float))
print(s["i"], s["f"])
quickpy.set_state({"i": s["i"], "f": s["f"], "n": s["n"], "small": s["small"], "large": s["large"]})`,
		InitialState: initial,
	}))
	requireContains(t, textOf(res.Outputs), "True True")
	requireState(t, res, map[string]any{
		"i":     int64(1),
		"f":     float64(1.0),
		"n":     1.0000000000000002,
		"small": 0.1,
		"large": 123456789.123456789,
	})
	state := res.State.(map[string]any)
	if state["small"].(float64) == float64(float32(0.1)) {
		t.Fatalf("small float collapsed to float32: %v", state["small"])
	}
	if state["large"].(float64) == float64(float32(123456789.123456789)) {
		t.Fatalf("large float collapsed to float32: %v", state["large"])
	}
	if !reflect.DeepEqual(initial, map[string]any{
		"i":     int64(1),
		"f":     float64(1.0),
		"n":     1.0000000000000002,
		"small": 0.1,
		"large": 123456789.123456789,
	}) {
		t.Fatalf("caller initial state mutated: %#v", initial)
	}
}

func TestStateSetNoneVsAbsent(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	absent := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy`}))
	if absent.StateWritten || absent.HasState() || absent.State != nil {
		t.Fatalf("absent state: State=%#v StateWritten=%v HasState=%v", absent.State, absent.StateWritten, absent.HasState())
	}
	none := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.set_state(None)`}))
	if !none.StateWritten || !none.HasState() {
		t.Fatalf("set_state(None) must be a write: StateWritten=%v HasState=%v", none.StateWritten, none.HasState())
	}
	if none.State != nil {
		t.Fatalf("set_state(None) state = %#v, want nil", none.State)
	}
}

func TestStateLastWriteWins(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.set_state(1)
quickpy.set_state({"n": 2, "b": True, "z": None})`}))
	requireState(t, res, map[string]any{"n": int64(2), "b": true, "z": nil})
}

func TestStateDeepCopy(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
v = [1]
quickpy.set_state(v)
v.append(2)
print("snapshot", quickpy.get_state())
s = quickpy.get_state()
s.append(3)
print("copy", quickpy.get_state())
d = {"a": [1]}
quickpy.set_state(d)
d["a"].append(2)
print("deep", quickpy.get_state())`}))
	requireContains(t, textOf(res.Outputs), "snapshot [1]")
	requireContains(t, textOf(res.Outputs), "copy [1]")
	requireContains(t, textOf(res.Outputs), "deep {'a': [1]}")
	requireState(t, res, map[string]any{"a": []any{int64(1)}})
}

func TestStateNestedJSON(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.set_state({
    "list": [1, 2.5, True, None, "héllo 😀"],
    "nested": {"a": [1, {"b": (2, 3)}]},
    "tuple": (1, 2),
    "int_min": -9223372036854775807 - 1,
    "int_max": 9223372036854775807,
    "float": 1.0,
    "escaped": "quote\" backslash\\ newline\n tab\t",
})`}))
	want := map[string]any{
		"list":    []any{int64(1), float64(2.5), true, nil, "héllo 😀"},
		"nested":  map[string]any{"a": []any{int64(1), map[string]any{"b": []any{int64(2), int64(3)}}}},
		"tuple":   []any{int64(1), int64(2)},
		"int_min": int64(math.MinInt64),
		"int_max": int64(math.MaxInt64),
		"float":   float64(1.0),
		"escaped": "quote\" backslash\\ newline\n tab\t",
	}
	requireState(t, res, want)
}

func TestStateNotCommittedOnError(t *testing.T) {
	t.Run("script error", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.set_state({"x": 1})
raise ValueError("boom")`}))
		requireErrorKind(t, err, quickpy.KindScriptError)
		requireStatus(t, res, quickpy.StatusFailed)
		requireNoState(t, res)
	})

	t.Run("stall", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import asyncio, quickpy
quickpy.set_state({"x": 1})
async def main():
    await asyncio.Event().wait()
quickpy.run(main())`}))
		requireErrorKind(t, err, quickpy.KindStalled)
		requireStatus(t, res, quickpy.StatusStalled)
		requireNoState(t, res)
	})

	t.Run("cancel", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		e, resumed := startLoopingExecution(t, r, nil, `import quickpy
quickpy.set_state({"x": 1})
`, nil)
		waitFor(t, resumed, "tool finished")
		time.Sleep(20 * time.Millisecond)
		e.Cancel()
		res, err := wait(t, e)
		requireErrorKind(t, err, quickpy.KindCancelled)
		requireStatus(t, res, quickpy.StatusCancelled)
		requireNoState(t, res)
	})

	t.Run("timeout", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
			Source: `import quickpy
quickpy.set_state({"x": 1})
print("started")
while True:
    pass`,
			Timeout: 100 * time.Millisecond,
		}))
		requireErrorKind(t, err, quickpy.KindTimedOut)
		requireStatus(t, res, quickpy.StatusTimedOut)
		requireNoState(t, res)
	})

	t.Run("final oversize", func(t *testing.T) {
		r := newRuntime(t, quickpy.RuntimeOptions{MaxStateBytes: 64})
		res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: `import quickpy
quickpy.set_state("x" * 100)`}))
		requireErrorKind(t, err, quickpy.KindLimitExceeded)
		requireStatus(t, res, quickpy.StatusLimitExceeded)
		requireNoState(t, res)
	})
}

func TestStateFailurePreservesCallerMap(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	initial := map[string]any{"a": []any{int64(1)}}
	res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{
		Source: `import quickpy
s = quickpy.get_state()
s["a"].append(2)
raise ValueError("boom")`,
		InitialState: initial,
	}))
	requireErrorKind(t, err, quickpy.KindScriptError)
	requireNoState(t, res)
	if !reflect.DeepEqual(initial, map[string]any{"a": []any{int64(1)}}) {
		t.Fatalf("caller initial state mutated: %#v", initial)
	}
}

func TestStateNoMutableAlias(t *testing.T) {
	r := newRuntime(t, quickpy.RuntimeOptions{})
	initial := map[string]any{"a": []any{int64(1)}}
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{
		Source: `import quickpy
s = quickpy.get_state()
s["a"].append(2)
quickpy.set_state(s)`,
		InitialState: initial,
	}))
	requireState(t, res, map[string]any{"a": []any{int64(1), int64(2)}})
	if !reflect.DeepEqual(initial, map[string]any{"a": []any{int64(1)}}) {
		t.Fatalf("caller initial state mutated: %#v", initial)
	}
	// Mutating the returned state must not affect the caller's map.
	res.State.(map[string]any)["a"].([]any)[0] = int64(99)
	if !reflect.DeepEqual(initial, map[string]any{"a": []any{int64(1)}}) {
		t.Fatalf("result state aliases caller map: %#v", initial)
	}
}
