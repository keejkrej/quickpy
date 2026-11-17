// Spike-only smoke harness for the QuickPy M0 guest artifact.
//
// This is not the QuickPy Go SDK.  It exists to prove that the guest module
// instantiates in wazero with fresh state per instance and that the transient
// M0 ABI, including the async submit-and-drive-to-quiescence path, works end
// to end.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

type call struct {
	ID   int64          `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type response struct {
	V        int     `json:"v"`
	Type     string  `json:"type"`
	Reason   string  `json:"reason"`
	Stalled  bool    `json:"stalled"`
	Calls    []call  `json:"calls"`
	Awaiting []int64 `json:"awaiting"`
	Output   string  `json:"output"`
	Error    *struct {
		Type      string `json:"type"`
		Message   string `json:"message"`
		Traceback string `json:"traceback"`
	} `json:"error"`
}

type instance struct {
	mod    api.Module
	alloc  api.Function
	free   api.Function
	invoke api.Function
	mem    api.Memory
}

func newInstance(ctx context.Context, r wazero.Runtime, wasm []byte) (*instance, error) {
	mod, err := r.Instantiate(ctx, wasm)
	if err != nil {
		return nil, err
	}
	if init := mod.ExportedFunction("_initialize"); init != nil {
		if _, err := init.Call(ctx); err != nil {
			return nil, fmt.Errorf("_initialize: %w", err)
		}
	}
	return &instance{
		mod:    mod,
		alloc:  mod.ExportedFunction("qp_alloc"),
		free:   mod.ExportedFunction("qp_free"),
		invoke: mod.ExportedFunction("qp_invoke"),
		mem:    mod.Memory(),
	}, nil
}

func (i *instance) invokeJSON(ctx context.Context, request string) (response, error) {
	var out response
	ptrRes, err := i.alloc.Call(ctx, uint64(len(request)))
	if err != nil {
		return out, err
	}
	ptr := uint32(ptrRes[0])
	if ptr == 0 {
		return out, fmt.Errorf("qp_alloc returned 0")
	}
	defer i.free.Call(ctx, uint64(ptr), uint64(len(request)))
	if !i.mem.Write(ptr, []byte(request)) {
		return out, fmt.Errorf("write request")
	}
	res, err := i.invoke.Call(ctx, uint64(ptr), uint64(len(request)))
	if err != nil {
		return out, err
	}
	packed := res[0]
	if packed == ^uint64(0) {
		return out, fmt.Errorf("qp_invoke returned -1")
	}
	rptr := uint32(packed >> 32)
	rlen := uint32(packed)
	defer i.free.Call(ctx, uint64(rptr), uint64(rlen))
	raw, ok := i.mem.Read(rptr, rlen)
	if !ok {
		return out, fmt.Errorf("read response")
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode response %q: %w", raw, err)
	}
	return out, nil
}

func startRequest(source string) string {
	b, _ := json.Marshal(map[string]any{
		"v":        0,
		"type":     "start",
		"source":   source,
		"filename": "<smoke>",
	})
	return string(b)
}

func toolResponses(responses []map[string]any, gc bool) string {
	m := map[string]any{
		"v":         0,
		"type":      "tool_responses",
		"responses": responses,
	}
	if gc {
		m["gc"] = true
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func findCall(calls []call, name string) (call, bool) {
	for _, c := range calls {
		if c.Name == name {
			return c, true
		}
	}
	return call{}, false
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func main() {
	artifact := "../build/quickpy.wasm"
	if len(os.Args) > 1 {
		artifact = os.Args[1]
	}
	wasm, err := os.ReadFile(artifact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read artifact: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	failures := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("PASS %s\n", name)
		} else {
			failures++
			fmt.Printf("FAIL %s: %s\n", name, detail)
		}
	}

	// The artifact contains a wasm exception tag.  Default wazero features
	// must reject it; this is the host integration requirement, not a bug.
	defaultRuntime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig())
	_, defaultErr := defaultRuntime.CompileModule(ctx, wasm)
	defaultRuntime.Close(ctx)
	check("default features reject tag section",
		defaultErr != nil && strings.Contains(defaultErr.Error(), "exception-handling"),
		fmt.Sprintf("err=%v", defaultErr))

	cfg := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2.SetEnabled(experimental.CoreFeaturesExceptionHandling, true))
	r := wazero.NewRuntimeWithConfig(ctx, cfg)
	defer r.Close(ctx)

	a, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate: %v\n", err)
		os.Exit(1)
	}
	// Exports.
	for _, name := range []string{"memory", "_initialize", "qp_abi_version", "qp_alloc", "qp_free", "qp_invoke"} {
		_, ok := a.mod.ExportedFunctionDefinitions()[name]
		if name == "memory" {
			ok = a.mod.ExportedMemory("memory") != nil
		}
		check("export "+name, ok, "missing")
	}

	ver, err := a.mod.ExportedFunction("qp_abi_version").Call(ctx)
	check("abi version", err == nil && uint32(ver[0]) == 1, fmt.Sprintf("err=%v value=%v", err, ver))

	fresh := func() *instance {
		i, err := newInstance(ctx, r, wasm)
		if err != nil {
			fmt.Fprintf(os.Stderr, "instantiate: %v\n", err)
			os.Exit(1)
		}
		return i
	}

	// Boot/evaluate.
	helloInst := fresh()
	res, err := helloInst.invokeJSON(ctx, startRequest("print(\"hello\")"))
	check("hello", err == nil && res.Type == "completed" && res.Output == "hello\n",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Python exception.
	excInst := fresh()
	res, err = excInst.invokeJSON(ctx, startRequest("print(\"before\")\nraise ValueError(\"boom\")"))
	check("exception", err == nil && res.Type == "failed" && res.Error != nil &&
		res.Error.Type == "ValueError" && res.Error.Message == "boom" &&
		strings.Contains(res.Error.Traceback, "ValueError: boom") && res.Output == "before\n",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// try/except exercises the same non-local-return path as an uncaught
	// exception, but catches it inside the guest.
	tryInst := fresh()
	res, err = tryInst.invokeJSON(ctx, startRequest("try:\n    raise ValueError(\"x\")\nexcept ValueError as e:\n    print(\"caught\", e)"))
	check("try/except", err == nil && res.Type == "completed" && res.Output == "caught x\n",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// A single start can set and use state.
	stateInst := fresh()
	res, err = stateInst.invokeJSON(ctx, startRequest("x = 42\nprint(x)"))
	check("state set", err == nil && res.Type == "completed" && res.Output == "42\n",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Source is accepted only once per instance.
	secondStartInst := fresh()
	res, err = secondStartInst.invokeJSON(ctx, startRequest("print(1)"))
	check("first start accepted", err == nil && res.Type == "completed" && res.Output == "1\n",
		fmt.Sprintf("err=%v response=%+v", err, res))
	res, err = secondStartInst.invokeJSON(ctx, startRequest("print(2)"))
	check("second start rejected", err == nil && res.Type == "failed" && res.Error != nil &&
		res.Error.Type == "ProtocolError" && strings.Contains(res.Error.Message, "start already executed"),
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Fresh state in a second instance.
	freshInst := fresh()
	res, err = freshInst.invokeJSON(ctx, startRequest("print(x)"))
	check("fresh state per instance", err == nil && res.Type == "failed" && res.Error != nil &&
		res.Error.Type == "NameError",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Protocol error.
	protoInst := fresh()
	res, err = protoInst.invokeJSON(ctx, `{"v":0,"type":"start"`)
	check("protocol error", err == nil && res.Type == "failed" && res.Error != nil &&
		res.Error.Type == "ProtocolError",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// ---------------------------------------------------------------------
	// Async M0 tests.
	// ---------------------------------------------------------------------

	asyncGatherSource := `import asyncio, quickpy

async def child(name, tool, value):
    local = "local-" + name
    try:
        result = await tool(value=value)
        print("after", name, local, result)
        return result
    finally:
        print("finally", name, local)

async def main():
    a, b = await asyncio.gather(child("a", quickpy.tools.a, 1), child("b", quickpy.tools.b, 2))
    print("gather", a, b)

quickpy.run(main())`

	ai, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate async: %v\n", err)
		os.Exit(1)
	}
	res, err = ai.invokeJSON(ctx, startRequest(asyncGatherSource))
	check("async gather first invoke suspended",
		err == nil && res.Type == "suspended" && res.Reason == "awaiting_calls" &&
			!res.Stalled && len(res.Calls) == 2 && len(res.Awaiting) == 2 && res.Output == "",
		fmt.Sprintf("err=%v response=%+v", err, res))
	ca, okA := findCall(res.Calls, "a")
	cb, okB := findCall(res.Calls, "b")
	check("async gather two distinct calls",
		okA && okB && ca.ID != cb.ID && ca.Args["value"] == float64(1) && cb.Args["value"] == float64(2),
		fmt.Sprintf("a=%+v b=%+v", ca, cb))
	check("async gather awaiting both",
		containsID(res.Awaiting, ca.ID) && containsID(res.Awaiting, cb.ID),
		fmt.Sprintf("awaiting=%v", res.Awaiting))

	// Resume only b.
	res, err = ai.invokeJSON(ctx, toolResponses([]map[string]any{
		{"id": cb.ID, "ok": true, "value": "B"},
	}, false))
	check("async resume b awaiting a",
		err == nil && res.Type == "suspended" && res.Reason == "awaiting_calls" &&
			!res.Stalled && len(res.Calls) == 0 && len(res.Awaiting) == 1 && res.Awaiting[0] == ca.ID,
		fmt.Sprintf("err=%v response=%+v", err, res))
	check("async resume b output delta",
		res.Output == "after b local-b B\nfinally b local-b\n",
		fmt.Sprintf("output=%q", res.Output))
	check("async b not redispatched",
		!containsID(res.Awaiting, cb.ID) && len(res.Calls) == 0,
		fmt.Sprintf("calls=%v awaiting=%v", res.Calls, res.Awaiting))

	// Force a real GC plus allocations while a is still pending, then deliver a.
	big := make([]int, 1000)
	for i := range big {
		big[i] = i
	}
	res, err = ai.invokeJSON(ctx, toolResponses([]map[string]any{
		{"id": ca.ID, "ok": true, "value": big},
	}, true))
	check("async gc + deliver a completed",
		err == nil && res.Type == "completed" && res.Error == nil,
		fmt.Sprintf("err=%v response=%+v", err, res))
	check("async gc locals/finally preserved",
		strings.Contains(res.Output, "after a local-a [0, 1, 2") &&
			strings.Contains(res.Output, "finally a local-a"),
		fmt.Sprintf("output=%q", res.Output))
	check("async gather order preserved",
		strings.Contains(res.Output, "gather [0, 1, 2") && strings.HasSuffix(res.Output, " B\n"),
		fmt.Sprintf("output=%q", res.Output))

	// Event.wait with no setter must return stalled, not hang.
	stall, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate stall: %v\n", err)
		os.Exit(1)
	}
	res, err = stall.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def main():
    ev = asyncio.Event()
    await ev.wait()
quickpy.run(main())`))
	check("async event wait stalled",
		err == nil && res.Type == "suspended" && res.Reason == "stalled" && res.Stalled &&
			len(res.Calls) == 0 && len(res.Awaiting) == 0,
		fmt.Sprintf("err=%v response=%+v", err, res))
	res, err = stall.invokeJSON(ctx, `{"v":0,"type":"resume"}`)
	check("async stalled remains terminal",
		err == nil && res.Type == "suspended" && res.Reason == "stalled" && res.Stalled &&
			len(res.Calls) == 0 && len(res.Awaiting) == 0,
		fmt.Sprintf("err=%v response=%+v", err, res))

	// sleep(0) interleaves without host suspension.
	sleep0, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate sleep0: %v\n", err)
		os.Exit(1)
	}
	res, err = sleep0.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def worker(name):
    print("start", name)
    await asyncio.sleep(0)
    print("end", name)
async def main():
    await asyncio.gather(worker("a"), worker("b"))
quickpy.run(main())`))
	check("async sleep(0) interleaves",
		err == nil && res.Type == "completed" && res.Output == "start a\nstart b\nend a\nend b\n",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Positive asyncio sleep raises catchable UnsupportedError.
	unsupported, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate unsupported: %v\n", err)
		os.Exit(1)
	}
	res, err = unsupported.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def main():
    try:
        await asyncio.sleep(1)
    except asyncio.UnsupportedError as e:
        print("caught", e)
quickpy.run(main())`))
	check("async positive sleep unsupported",
		err == nil && res.Type == "completed" && res.Output == "caught positive timers are not supported\n",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Positive time.sleep raises the same catchable UnsupportedError.
	timeSleep, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate time sleep: %v\n", err)
		os.Exit(1)
	}
	res, err = timeSleep.invokeJSON(ctx, startRequest(`import asyncio, time
try:
    time.sleep(1)
except asyncio.UnsupportedError as e:
    print("caught", e)`))
	check("async time.sleep unsupported",
		err == nil && res.Type == "completed" && res.Output == "caught time.sleep is not supported\n",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Second quickpy.run is an error.
	secondRun, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate second run: %v\n", err)
		os.Exit(1)
	}
	res, err = secondRun.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def main():
    pass
quickpy.run(main())
quickpy.run(main())`))
	check("async second run fails",
		err == nil && res.Type == "failed" && res.Error != nil &&
			res.Error.Type == "RuntimeError" && strings.Contains(res.Error.Message, "may only be called once"),
		fmt.Sprintf("err=%v response=%+v", err, res))

	// ToolError is catchable.
	toolErr, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate tool error: %v\n", err)
		os.Exit(1)
	}
	res, err = toolErr.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def main():
    try:
        await quickpy.tools.a(value=1)
    except quickpy.ToolError as e:
        print("toolerror", e.code, e.message)
quickpy.run(main())`))
	check("async tool error first invoke",
		err == nil && res.Type == "suspended" && len(res.Calls) == 1,
		fmt.Sprintf("err=%v response=%+v", err, res))
	if len(res.Calls) == 1 {
		res, err = toolErr.invokeJSON(ctx, toolResponses([]map[string]any{
			{"id": res.Calls[0].ID, "ok": false, "error": map[string]any{"code": "boom", "message": "tool failed"}},
		}, false))
		check("async tool error catchable",
			err == nil && res.Type == "completed" && res.Output == "toolerror boom tool failed\n",
			fmt.Sprintf("err=%v response=%+v", err, res))
	}

	// Unknown response id fails cleanly.
	unknown, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate unknown: %v\n", err)
		os.Exit(1)
	}
	res, err = unknown.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def main():
    await quickpy.tools.a()
quickpy.run(main())`))
	check("async unknown setup", err == nil && res.Type == "suspended" && len(res.Calls) == 1,
		fmt.Sprintf("err=%v response=%+v", err, res))
	res, err = unknown.invokeJSON(ctx, toolResponses([]map[string]any{
		{"id": 999999, "ok": true, "value": "X"},
	}, false))
	check("async unknown response fails cleanly",
		err == nil && res.Type == "failed" && res.Error != nil && res.Error.Type == "ProtocolError",
		fmt.Sprintf("err=%v response=%+v", err, res))

	// Duplicate response id fails cleanly.
	dup, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate duplicate: %v\n", err)
		os.Exit(1)
	}
	res, err = dup.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def main():
    await quickpy.tools.a()
quickpy.run(main())`))
	check("async duplicate setup", err == nil && res.Type == "suspended" && len(res.Calls) == 1,
		fmt.Sprintf("err=%v response=%+v", err, res))
	if len(res.Calls) == 1 {
		id := res.Calls[0].ID
		res, err = dup.invokeJSON(ctx, toolResponses([]map[string]any{
			{"id": id, "ok": true, "value": "A"},
		}, false))
		check("async duplicate first delivery", err == nil && res.Type == "completed",
			fmt.Sprintf("err=%v response=%+v", err, res))
		res, err = dup.invokeJSON(ctx, toolResponses([]map[string]any{
			{"id": id, "ok": true, "value": "A"},
		}, false))
		check("async duplicate response fails cleanly",
			err == nil && res.Type == "failed" && res.Error != nil && res.Error.Type == "ProtocolError",
			fmt.Sprintf("err=%v response=%+v", err, res))
	}

	// resume with no responses keeps the same awaiting set and emits no output.
	resumeInst, err := newInstance(ctx, r, wasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "instantiate resume: %v\n", err)
		os.Exit(1)
	}
	res, err = resumeInst.invokeJSON(ctx, startRequest(`import asyncio, quickpy
async def main():
    await quickpy.tools.a()
quickpy.run(main())`))
	check("async resume setup", err == nil && res.Type == "suspended" && len(res.Awaiting) == 1,
		fmt.Sprintf("err=%v response=%+v", err, res))
	res, err = resumeInst.invokeJSON(ctx, `{"v":0,"type":"resume"}`)
	check("async resume with pending call not stalled",
		err == nil && res.Type == "suspended" && res.Reason == "awaiting_calls" &&
			!res.Stalled && len(res.Calls) == 0 && len(res.Awaiting) == 1 && res.Output == "",
		fmt.Sprintf("err=%v response=%+v", err, res))
	if len(res.Awaiting) == 1 {
		res, err = resumeInst.invokeJSON(ctx, toolResponses([]map[string]any{
			{"id": res.Awaiting[0], "ok": true, "value": "A"},
		}, false))
		check("async resume pending call later completes",
			err == nil && res.Type == "completed",
			fmt.Sprintf("err=%v response=%+v", err, res))
	}

	if failures > 0 {
		fmt.Printf("%d failure(s)\n", failures)
		os.Exit(1)
	}
	fmt.Println("all smoke checks passed")
}
