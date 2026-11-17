# QuickPy M0 guest feasibility spike

**Status:** spike succeeded for the narrow M0 question: MicroPython C plus a
small C adapter builds as a reactor-style WASM/WASI module, instantiates in
wazero v1.12.0 with fresh state per instance, and evaluates ordinary
module-like Python source. The follow-up M0 async experiment also succeeded
for submit-and-drive-to-quiescence: `quickpy.run(main())` can park on host
tool calls, return to the adapter, and resume later without stack switching.
Cancellation is **not implemented guest-side** (no cooperative interrupt hook); the
host performs forced cancellation by closing the active wazero invocation, which
is verified in host integration. Positive timers are deliberately rejected, and
stream I/O remains out of scope.

This is an experimental spike, not a production guest. The ABI and response
shapes below are transient and may change.

## Verification status

**Final independent verdict:** all 11 §16 M0 criteria PASS; **GO** for merge as
an M0 feasibility baseline, not a production SDK.

- **Guest smoke:** 41 checks reproduced against the real artifact, including
  `gather`/out-of-order/partial resume, GC-pressure preservation of
  locals/`finally`, true terminal `stalled`, nonterminal pending
  `awaiting_calls`, `sleep(0)`, positive timer rejection, catchable unknown
  tool, memory bomb/output cap, and no stdout leak.
- **Host:** `gofmt`, `go test`, `go vet`, and `go build` pass. Real
  `context.WithCancel` running-loop and suspended-tool cancellation each passed
  3/3 as `Cancelled` (not timeout), with the other execution in the same runtime
  completing `unaffected\n`; running invoke 1 / free 0 / invoke-returned true;
  suspended invoke 1 / free 2 / handler context canceled; request-to-return
  latency 0.008–0.060 ms in an independent run.
- **WASI:** host `allowedWASIFuncs` is empty; `path_open`, `environ_get`,
  `args_get`, and `poll_oneoff` are rejected before instantiation; guest imports
  are zero.
- **Artifact:** reproducible twice on this machine — 195,053 bytes, sha256
  `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`, with
  Zig 0.17.1 / wasm-opt 133 / MicroPython v1.29.0 pins.
- **Measurements (informational, run-specific):** compile mean ~39.8 ms
  (read + compile + prelude + import check); warm instantiate + `_initialize`
  mean ~0.193 ms, range 0.060–0.426 ms over the final N=8 run; suspended linear
  memory exactly 100 pages / 6,553,600 bytes per instance; process RSS
  approximately 11.5–11.8 MiB per instance in the independent N=4/8 run. RSS is
  approximate host-process memory, not an exact per-instance figure; suspended
  quiescence was not a long soak.

M0 is a feasibility baseline, not completion of M1. Remaining hard limits and
production TODOs are M1+: public SDK, persistent state/events, graceful `Close`
under load, per-execution memory configuration given the runtime-level
limitation, a narrow frozen-module set, positive timers unsupported, no
top-level `await`, `quickpy.run` final, no OS sandbox or pi-go integration, and
the experimental wazero EH requirement.

## Result summary

| Question | Result |
|---|---|
| MicroPython C builds for `wasm32-wasi` | Yes, with the workarounds below |
| Reactor module, no required `_start` | Yes; `_initialize` is exported by wasi-libc |
| wazero instantiates the module | Yes, wazero v1.12.0 **with `experimental.CoreFeaturesExceptionHandling` enabled**; default features reject the tag section |
| Fresh state per instance | Yes; verified with two instances |
| `print("hello")` | Yes; output `hello\n` |
| Python exception details | Yes; type, message, and traceback text |
| `try/except` | Yes; caught exception prints `caught x` |
| Deep recursion | Raises `RuntimeError: maximum recursion depth exceeded` (stack check enabled), not a wasm trap |
| Host FS/network/env inherited | No; the produced module has **zero imports** |
| Async submit-and-drive-to-quiescence | Yes; `quickpy.run(main())` parks on host calls and resumes later |
| Stock `_asyncio` Task/TaskQueue + stock `gather` | Yes; custom frozen `asyncio` loop drives them |
| GC while a tool call is pending | Yes; forced `gc.collect()` + allocations, then resume |
| Positive timers | Rejected with catchable `UnsupportedError` |
| Dead `Event.wait()` | Returns terminal `suspended`/`stalled`, no hang |
| Cancellation / interruption | Guest-side cooperative cancellation not implemented; host-led forced cancellation verified in host integration |

## Adapter language

The M0 adapter is written in **C** and compiled/linked with **Zig** (`zig cc`).
The spike did not need a Zig-language adapter: the host-facing surface is five
exports plus a small JSON codec, and the hard part was the MicroPython/WASI
build. A Zig-language adapter remains a follow-up if the ABI grows. This note
is explicit so the result is not read as "Zig source adapter proven".

## Pins and toolchain

- MicroPython: **v1.29.0**, commit
  `0fd6c573ea815774668bbb16b8e197c8822368b2`
  (cloned by `guest/build.sh` into `guest/.cache/micropython`; not vendored)
- Zig: **0.17.1** (bundled clang 22.1.8, bundled wasi-libc)
- Binaryen / `wasm-opt`: **133**
- Go (smoke harness only): 1.24+ with `github.com/tetratelabs/wazero v1.12.0`
- Host: macOS arm64 (the build is host-independent apart from the tool paths)

`guest/build.sh` checks for `git`, `make`, `python3`, `zig`, and `wasm-opt`.
It warns if Zig is not 0.17.1 and fails if the MicroPython checkout is not the
exact commit above.

## Build

```sh
./guest/build.sh
```

Artifact: `guest/build/quickpy.wasm`

Measured artifact (this run):

- size: **195,053 bytes**
- sha256: `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`

The build is two-stage:

1. `make -C guest/port` compiles MicroPython + `adapter.c` + `setjmp_rt.c`
   + `extmod/modasyncio.c`, `extmod/modjson.c`, `extmod/modtime.c` with
   `zig cc --target=wasm32-wasi`.  The frozen `asyncio` package and
   `quickpy.py` are generated from `guest/port/manifest.py`; the build script
   builds host `mpy-cross` and initialises `lib/micropython-lib` because the
   MicroPython manifest machinery requires them even for `freeze_as_str`.
2. `wasm-opt --strip-dwarf` then
   `wasm-opt --enable-reference-types --enable-exception-handling
   --translate-to-exnref` produces the final module.

The intermediate `guest/build/obj/quickpy.wasm` is not the artifact to ship:
it still contains legacy WebAssembly exception opcodes that wazero rejects.

## Actual module shape

From `wasm-dis guest/build/quickpy.wasm`:

```
(memory $0 64 1024)
(tag $tag$0 (type $5) (param i32))
(export "memory" (memory $0))
(export "_initialize" (func $1))
(export "qp_abi_version" (func $691))
(export "qp_alloc" (func $692))
(export "qp_free" (func $693))
(export "qp_invoke" (func $694))
```

- **Imports: none.** `grep -c '(import'` on the disassembly returns `0`.
  There are no `wasi_snapshot_preview1` imports, no filesystem, network,
  environment, clock, or random imports in this artifact. The host does not
  need to instantiate `wasi_snapshot_preview1` for this build.
- **Tag section is required.** The module contains one wasm exception tag
  (`__c_longjmp`) used by MicroPython's non-local-return implementation. A
  host that does not enable wasm exception handling will reject the module at
  compile time. See "Exceptions-disabled build attempt" below.
- Memory: initial 64 pages (4 MiB), maximum 1024 pages (64 MiB).
- Linker stack: 1 MiB (`-Wl,-z,stack-size=1048576`).
- MicroPython GC heap: 1 MiB, allocated from wasi-libc `malloc` on first
  `qp_invoke`.
- `_initialize` is the wasi-libc reactor initializer. The adapter also
  lazily initializes MicroPython on the first `qp_invoke`, so a host that
  skips `_initialize` still works in this spike. Hosts should call
  `_initialize` once per instance if it is exported.
- There is no `_start` export and no automatic start function.

## Transient M0 ABI

All integers are wasm32 `i32` unless noted.

| Export | Signature | Meaning |
|---|---|---|
| `qp_abi_version` | `() -> i32` | Packed major/minor. `1` means **0.1**. |
| `qp_alloc` | `(len: i32) -> i32` | Allocate `len` bytes. Returns pointer, or `0` on failure or `len <= 0`. |
| `qp_free` | `(ptr: i32, len: i32) -> ()` | Free a buffer from `qp_alloc` or a response from `qp_invoke`. `len` is ignored. |
| `qp_invoke` | `(ptr: i32, len: i32) -> i64` | Evaluate one length-delimited UTF-8 JSON request. Returns packed `(ptr << 32) | len`, or `-1` on unrecoverable adapter failure. |

### Ownership

- The host allocates the request buffer with `qp_alloc`, writes the request
  bytes, and passes `(ptr, len)` to `qp_invoke`.
- `qp_invoke` **does not free the request buffer**. The host frees it with
  `qp_free(ptr, len)` after `qp_invoke` returns.
- `qp_invoke` allocates the response buffer with `malloc`. The response bytes
  remain valid until the host calls `qp_free(response_ptr, response_len)`.
- The host must copy the response bytes before freeing them.
- A `qp_invoke` return of `-1` means no response buffer was produced; the host
  owns nothing from that call.
- The host must serialize calls into one instance. The adapter is not
  reentrant and has no internal locking.

### Request

```json
{"v":0,"type":"start","source":"print(\"hello\")","filename":"<test>"}
```

- `v` must be `0`.
- `type` must be `"start"`.
- `source` is the Python source string.
- `filename` is used in tracebacks. It may be empty.
- Unknown top-level fields are skipped.
- `start` executes the source once per instance.  The async experiment
  supports `import asyncio, quickpy`, `quickpy.tools.<name>(**kwargs)`, and
  `quickpy.run(main())` as the last top-level statement.  There is no
  top-level `await` transform, no state store, and no host imports.
- The build uses MicroPython's minimum ROM level plus the M0 async additions.
  Core builtins, `sys`, `json`, `time`, and `io` (StringIO only; `open` is
  unsupported) are available.  `math` and `os` remain disabled.

### Response

Completed:

```json
{"v":0,"type":"completed","output":"hello\n"}
```

Failed Python execution:

```json
{"v":0,"type":"failed","output":"before\n","error":{"type":"ValueError","message":"boom","traceback":"Traceback (most recent call last):\n  File \"<test>\", line 2, in <module>\nValueError: boom\n"}}
```

Protocol failure:

```json
{"v":0,"type":"failed","output":"","error":{"type":"ProtocolError","message":"invalid request JSON"}}
```

- `output` is the ordered concatenation of captured text. The spike captures
  raw text and does **not** translate LF to CRLF. There is no separate stderr
  channel and no WASI `fd_write`: `print()` and MicroPython's error printer are
  captured explicitly by the adapter.
- `output_truncated: true` is added if the guest's 1 MiB output cap was hit or
  the output buffer allocation failed. The host must still enforce its own
  output limit.
- `error.type` is the MicroPython exception type name (for example
  `ValueError`, `NameError`, `SyntaxError`, `RuntimeError`) or
  `ProtocolError`.
- `error.message` is the first exception argument printed as text.
- `error.traceback` is MicroPython's formatted traceback text, capped at
  16 KiB. `traceback_truncated: true` is added when the cap is hit.
- The response is a single JSON object. It is not newline-terminated.

## Evidence commands

Build:

```sh
./guest/build.sh
```

Smoke test (spike-only Go harness, not the SDK):

```sh
cd guest/smoke
go run . ../build/quickpy.wasm
```

Observed output:

```
PASS default features reject tag section
PASS export memory
PASS export _initialize
PASS export qp_abi_version
PASS export qp_alloc
PASS export qp_free
PASS export qp_invoke
PASS abi version
PASS hello
PASS exception
PASS try/except
PASS state set
PASS first start accepted
PASS second start rejected
PASS fresh state per instance
PASS protocol error
PASS async gather first invoke suspended
PASS async gather two distinct calls
PASS async gather awaiting both
PASS async resume b awaiting a
PASS async resume b output delta
PASS async b not redispatched
PASS async gc + deliver a completed
PASS async gc locals/finally preserved
PASS async gather order preserved
PASS async event wait stalled
PASS async stalled remains terminal
PASS async sleep(0) interleaves
PASS async positive sleep unsupported
PASS async time.sleep unsupported
PASS async second run fails
PASS async tool error first invoke
PASS async tool error catchable
PASS async unknown setup
PASS async unknown response fails cleanly
PASS async duplicate setup
PASS async duplicate first delivery
PASS async duplicate response fails cleanly
PASS async resume setup
PASS async resume with pending call not stalled
PASS async resume pending call later completes
all smoke checks passed
```

Manual wazero check used during the spike (temporary, outside the repo):
instantiate with
`api.CoreFeaturesV2.SetEnabled(experimental.CoreFeaturesExceptionHandling, true)`,
call `_initialize`, then `qp_abi_version`, `qp_alloc`, `qp_invoke`, `qp_free`.
The module instantiated and ran without any WASI imports.

## Build workarounds and why they exist

These are the real blockers found, not hidden:

1. **Zig's wasi-libc has no setjmp/longjmp runtime.** MicroPython needs
   non-local returns for exceptions. Zig 0.17.1 ships an older
   `libc-top-half/musl/src/setjmp/wasm32/rt.c` that lacks the `__c_longjmp`
   tag required by LLVM 22+. The spike carries a current copy as
   `guest/port/setjmp_rt.c` (MIT, adapted from wasi-libc).
2. **Zig disables the wasm exception-handling target feature.** The build
   re-enables it with
   `-Xclang=-target-feature -Xclang=+exception-handling` and enables the
   setjmp lowering with `-mllvm=-wasm-enable-sjlj`.
3. **LLVM still emits legacy EH opcodes for setjmp/longjmp.** wazero v1.12.0
   rejects them with `legacy exception handling instruction ... recompile with
   wasm-opt --translate-to-exnref`. The build runs
   `wasm-opt --translate-to-exnref` (with reference types and EH enabled).
   The translated module still has a tag section, so the host must enable
   `experimental.CoreFeaturesExceptionHandling`.
4. **`zig cc -E` loses its wasi-libc include paths with multiple input
   files.** MicroPython's qstr collector batches sources. `guest/port/cpp.sh`
   preprocesses one file at a time and concatenates the output.
5. **Zig's wasi-libc has no `<alloca.h>`.** `mpconfigport.h` maps `alloca` to
   `__builtin_alloca`.
6. **Reactor model.** `-mexec-model=reactor` gives `_initialize` and avoids a
   required `_start`.
7. **Frozen-module import needs the external-import path.** The frozen
   `.frozen` search lives behind `MICROPY_ENABLE_EXTERNAL_IMPORT` and
   `sys.path`.  The build enables both, but `mp_import_stat()` returns
   `NO_EXIST`, so there is still no filesystem import capability.
8. **Manifest machinery needs mpy-cross and micropython-lib.** Even with
   `freeze_as_str`, `makemanifest.py` requires a host `mpy-cross` binary and
   `lib/micropython-lib/README.md`.  `guest/build.sh` builds the former and
   initialises the latter from the pinned checkout.

## Exceptions-disabled build attempt (host integration finding)

The Go host worker reported that wazero v1.12.0 rejects the artifact with:

```
tag section not supported as feature "exception-handling" is disabled
```

This is reproducible with wazero's default core features. The same module
compiles successfully when the host enables the experimental feature:

```go
cfg := wazero.NewRuntimeConfig().WithCoreFeatures(
    api.CoreFeaturesV2.SetEnabled(experimental.CoreFeaturesExceptionHandling, true),
)
```

Observed with a small wazero v1.12.0 compile check:

```
eh=false err=tag section not supported as feature "exception-handling" is disabled
eh=true  err=<nil>
```

Both the wazero compiler and interpreter engines compile the module when EH is
enabled.

A focused attempt was made to produce an exceptions-disabled build. It is not
viable with MicroPython's current non-local-return implementation. The exact
reproduction commands were:

```sh
# 1. No EH flags: fails in the wasi-libc setjmp header.
zig cc --target=wasm32-wasi \
  -I guest/port -I guest/.cache/micropython -I guest/build/obj \
  -c guest/.cache/micropython/py/nlrsetjmp.c -o /tmp/nlr_noeh.o

# 2. Define the header guard but do not enable EH: backend catchret failure.
zig cc --target=wasm32-wasi -D__wasm_exception_handling__ \
  -I guest/port -I guest/.cache/micropython -I guest/build/obj \
  -c guest/.cache/micropython/py/runtime.c -o /tmp/runtime_noeh.o
```

The observed failures were:

1. **No EH flags at all.** `py/nlrsetjmp.c` includes `<setjmp.h>`, and Zig's
   wasi-libc header fails with:
   `Setjmp/longjmp support requires Exception handling support`.
2. **Define `__wasm_exception_handling__` but do not enable EH.** The
   preprocessor accepts the header, but the full build fails in
   `py/runtime.c` with:
   `fatal error: error in backend: Cannot select: ... ch = catchret ...`
   in `mp_iternext`.
3. **`-mllvm=-wasm-enable-sjlj` without `-fwasm-exceptions`.** Same backend
   `catchret` failure. The sjlj lowering still needs the wasm EH target
   feature.
4. **`-fno-exceptions`.** C is already compiled without C++ exceptions; this
   does not change the setjmp/longjmp lowering and does not remove the tag.
5. **`setjmp_rt.c` itself defines the tag.** It uses
   `__builtin_wasm_throw(1, arg)` and inline assembly to define
   `__c_longjmp`. The final WAT contains `(tag $tag$0 ...)`, `(throw $tag$0)`,
   and `(catch $tag$0)`. Removing the tag would break exception propagation.
6. **Stub-nlr no-EH variant.** Setting `MICROPY_NLR_SETJMP=0` and providing a
   trapping `nlr_push`/`nlr_jump` still fails: `py/nlr.h` auto-selects the
   setjmp fallback for unknown architectures (wasm is unknown), so it still
   includes `<setjmp.h>` and hits the same header error. Patching `py/nlr.h`
   to add a wasm branch would be a port change, and the resulting runtime
   would trap on every Python exception. It would not be a viable guest.

MicroPython uses non-local returns for ordinary Python exceptions, including
`try/except`, `raise`, and VM-internal control flow. Replacing that with a
return-code/CPS mechanism is an interpreter rewrite, not a build flag. The
spike therefore keeps the EH-based artifact and records the host requirement
explicitly. **No no-tag artifact was produced.** The tag section is present in
`guest/build/quickpy.wasm` and is required by the generated `throw`/`catch`
instructions.

**Host integration requirement:** wazero v1.12.0 must be configured with
`experimental.CoreFeaturesExceptionHandling` enabled. The artifact must be the
post-`wasm-opt --translate-to-exnref` file (`guest/build/quickpy.wasm`), not
the intermediate `guest/build/obj/quickpy.wasm` (legacy EH opcodes). No WASI
imports are needed.

If the host cannot enable wasm EH, M0 is blocked for any Python code that can
raise an exception. A no-EH build would only be possible after replacing
MicroPython's nlr mechanism.

## M0 async experiment (submit-and-drive-to-quiescence)

**Verdict: proven for the narrow M0 shape.**  No stack switching is needed
when `quickpy.run` is submit-and-drive-to-quiescence rather than a literally
blocked C/Python call.  The main task, sub-tasks, coroutine frames, and
`_HostFuture` objects survive as GC-rooted Python objects.  Every
`qp_invoke` returns only after `quickpy.run`/`_resume`, the scheduler's
`t.coro.send`, and all active C frames have returned to the adapter baseline.
No NLR/setjmp frame survives across exports.

### Python surface

```python
import asyncio, quickpy

async def main():
    a, b = await asyncio.gather(
        quickpy.tools.a(value=1),
        quickpy.tools.b(value=2),
    )
    print(a, b)

quickpy.run(main())
```

- `quickpy.tools.<name>(**kwargs)` returns a coroutine that awaits a
  `_HostFuture`.  The kwargs object is JSON-encoded and sent as the tool
  request `args`.
- `quickpy.run(main())` promotes exactly one main task, drives the loop to
  quiescence, and returns normally if the main task is parked on host work.
- **Run-last requirement:** statements after `quickpy.run(main())` execute
  before outstanding tools finish.  For M0, `quickpy.run(main())` must be the
  last top-level statement.
- A second `quickpy.run` raises `RuntimeError("quickpy.run may only be called
  once")`.
- `asyncio.gather` is stock MicroPython v1.29.0 `extmod/asyncio/funcs.py`.
  Result order, exception propagation, coroutine locals, and `finally`
  behavior are preserved.
- `_asyncio.Task` and `_asyncio.TaskQueue` are the stock native C
  implementations.  The custom frozen `asyncio/core.py` is adapted from
  upstream `extmod/asyncio/core.py` and `ports/webassembly/asyncio/core.py`.
- `Event` and `Lock` are stock.  `ThreadSafeFlag`, stream I/O,
  `open_connection`, and `start_server` fail loudly with
  `UnsupportedError`.
- `asyncio.sleep(0)` yields once and reschedules without host suspension.
  `asyncio.sleep(t>0)` and `time.sleep(t>0)` raise `UnsupportedError`.
  For M0, `UnsupportedError` is an alias of `NotImplementedError` so the C
  `time.sleep` path and the Python `asyncio.sleep` path are catchable under
  the same name.
- `time` is enabled only so `time.sleep` can fail loudly; `open()` is
  unsupported.  `json` is enabled for tool args/results.  `math`, `os`, and
  stream I/O remain disabled.

### Protocol extension

The synchronous `completed`/`failed` shapes are unchanged.  The async
experiment adds:

```json
{"v":0,"type":"suspended","reason":"awaiting_calls","calls":[{"id":1,"name":"a","args":{"value":1}}],"awaiting":[1,2],"output":""}
```

```json
{"v":0,"type":"suspended","reason":"stalled","stalled":true,"calls":[],"awaiting":[],"output":""}
```

- `calls` is the **NEW REQUEST DELTA** for this invoke only.  A call is
  reported exactly once, when it is first registered.
- `awaiting` is **ALL UNDELIVERED IDS** for the instance, including calls
  reported in earlier invokes.
- `reason:"awaiting_calls"` means at least one host call is undelivered.
- `reason:"stalled"` with `stalled:true` is **terminal**: the main task is
  alive, there is no runnable work, no undelivered call, and no timer (for
  example `Event.wait()` with no setter).  The host must not resume it.
- `reason:"runnable"` is reserved for a future non-terminal early return and
  is **not emitted by this build**.  Do not treat `reason:"runnable"` as a
  terminal stall.
- **Host adaptation:** the previous artifact used `reason:"runnable"` for
  terminal stalls.  Hosts must now treat `reason:"stalled"` + `stalled:true`
  as terminal and must not resume it.  `reason:"awaiting_calls"` remains
  non-terminal and is resumed only after responses arrive.
- `output` is a per-invocation delta.  The host accumulates deltas; it must
  not re-append the same text.
- Tool request: `{"id":<int>,"name":<string>,"args":<JSON object>}`.  IDs are
  unique and monotonic per execution, starting at 1.
- Host re-entry requests:

```json
{"v":0,"type":"tool_responses","responses":[{"id":1,"ok":true,"value":...}]}
```

```json
{"v":0,"type":"tool_responses","responses":[{"id":1,"ok":false,"error":{"code":"boom","message":"tool failed"}}]}
```

```json
{"v":0,"type":"resume"}
```

- `tool_responses` delivers one or more responses, then drives the loop.
  `resume` drives runnable work without delivering responses.
- Unknown or duplicate response IDs return a structured
  `ProtocolError` and do not hang.
- A failed tool response becomes a catchable `quickpy.ToolError` with
  `.code` and `.message`.
- Test hook: `{"v":0,"type":"tool_responses","gc":true,...}` forces a real
  `gc.collect()` at the adapter boundary before delivering responses.  This
  is used by the smoke test to prove parked tasks/futures survive GC.  It is
  not required by the host protocol.

### Ownership and GC roots

- The imported `quickpy` module is stored in a registered root pointer
  (`MP_REGISTER_ROOT_POINTER(mp_obj_t quickpy_module)`), not a bare static
  `mp_obj_t`.
- `quickpy._main_task`, `quickpy._pending`, and the `asyncio.core` module
  globals (`_task_queue`, `cur_task`) are reachable from that module and the
  loaded-modules dictionary.
- `_HostFuture` objects are referenced by `_pending` and by the parked task's
  `data`; the task is referenced by the main task/gather frame.
- The C call ledger (`g_calls`, `g_new_calls`) holds only C strings/IDs, not
  Python objects.
- `qp_invoke` resets the output delta and the NEW-call delta, but keeps
  `g_calls` (all undelivered calls) across invokes.
- No NLR frame is live when `qp_invoke` returns: `run_source`, `_resume`, and
  `_deliver` each push/pop their own nlr around the Python call, and the
  coroutine frames are heap objects, not C stack frames.

### Observed results

The real wazero smoke harness (`guest/smoke`) passes:

```
PASS async gather first invoke suspended
PASS async gather two distinct calls
PASS async gather awaiting both
PASS async resume b awaiting a
PASS async resume b output delta
PASS async b not redispatched
PASS async gc + deliver a completed
PASS async gc locals/finally preserved
PASS async gather order preserved
PASS async event wait stalled
PASS async stalled remains terminal
PASS async sleep(0) interleaves
PASS async positive sleep unsupported
PASS async time.sleep unsupported
PASS async second run fails
PASS async tool error first invoke
PASS async tool error catchable
PASS async unknown setup
PASS async unknown response fails cleanly
PASS async duplicate setup
PASS async duplicate first delivery
PASS async duplicate response fails cleanly
PASS async resume setup
PASS async resume with pending call not stalled
PASS async resume pending call later completes
```

Key observations:

- First gather invoke: `suspended`, `reason:"awaiting_calls"`, two distinct
  calls (`a` with `{"value":1}`, `b` with `{"value":2}`), `awaiting:[1,2]`,
  empty output.
- Resume only `b`: `suspended`, `calls:[]`, `awaiting:[1]`, output delta
  `after b local-b B\nfinally b local-b\n`.  `b` is never redispatched.
- Forced `gc.collect()` plus a 1000-element list allocation while `a` is
  pending, then deliver `a`: `completed`, output delta contains
  `after a local-a ...`, `finally a local-a`, and
  `gather [0, 1, 2, ...] B`.  Gather order is `[a, b]`.
- `Event.wait()` with no setter: `suspended`, `reason:"stalled"`,
  `stalled:true`, no calls, no awaiting, no hang.  This is terminal.
- `resume` with a pending call before its response arrives:
  `suspended`, `reason:"awaiting_calls"`, `stalled:false`, awaiting `[id]`.
  This is not a stall and must be resumed only after the response arrives.
- Resuming a terminal stalled execution returns the same terminal stalled
  response; it does not make progress.
- `sleep(0)` interleaves: `start a\nstart b\nend a\nend b\n`.
- `asyncio.sleep(1)` and `time.sleep(1)` both raise catchable
  `UnsupportedError`.
- A second `start` request on the same instance is rejected with
  `ProtocolError("start already executed")`, even if the first source did not
  call `quickpy.run`.
- Second `quickpy.run` fails with `RuntimeError`.
- Failed tool response is catchable as `quickpy.ToolError`.
- Unknown and duplicate response IDs return `ProtocolError` without hanging.
- `resume` with no responses keeps the same awaiting set and emits no output.

### Comparison against the research prediction

| Prediction | Result |
|---|---|
| Feasible without stack switching if `quickpy.run` is submit-and-drive-to-quiescence | Confirmed |
| Stock `_asyncio` Task/TaskQueue and stock `gather` usable | Confirmed |
| Main task/coroutine/futures survive as GC-rooted Python objects | Confirmed, including forced GC |
| Every return happens after scheduler/C frames return to baseline | Confirmed by repeated re-entry and GC test |
| NLR setjmp frames must not survive across exports | Confirmed; no trap/corruption |
| Later entries resume tasks, not the returned `quickpy.run` frame | Confirmed |
| Positive timers can be rejected before truncation | Confirmed |
| Dead wait returns stalled, no hang | Confirmed |
| Unsupported primitives fail loudly | Confirmed for stream I/O, ThreadSafeFlag, positive sleeps |

### Remaining async limits

- No guest-side cooperative cancellation/interruption, no guest deadlines, no fuel/step limit. Host-led forced cancellation is verified in host integration.
- No positive timers, no host I/O polling, no stream I/O.
- No top-level `await` transform; `quickpy.run(main())` must be the last
  top-level statement.
- `_HostFuture` is single-waiter.  Multiple tasks awaiting the same tool call
  are not supported.
- No persistence/state API, no arbitrary module loader, no full public SDK.
- `asyncio.UnsupportedError` is an alias of `NotImplementedError` in M0.
- `time.sleep(0)` also raises; only `asyncio.sleep(0)` is the supported
  no-host-wait yield.
- The host must still enforce source/message/output/memory/time limits and
  discard the instance after a wasm trap.

## Host integration checklist

1. Use `guest/build/quickpy.wasm` (post-`wasm-opt --translate-to-exnref`), not
   the intermediate object.
2. Compile with wazero v1.12.0 and
   `experimental.CoreFeaturesExceptionHandling` enabled.
3. Do not instantiate `wasi_snapshot_preview1`; the module has no imports.
4. Instantiate a fresh module per execution. Call `_initialize` once if
   exported (recommended; the adapter also lazy-inits).
5. Check `qp_abi_version() == 1`.
6. Allocate the request with `qp_alloc`, write UTF-8 JSON, call `qp_invoke`.
7. If the result is `-1`, treat it as an unrecoverable adapter failure.
8. Otherwise unpack `(ptr << 32) | len`, copy the response bytes, then
   `qp_free(response_ptr, response_len)` and `qp_free(request_ptr, request_len)`.
9. For `type:"suspended"`, dispatch `calls` (new requests only), keep
   `awaiting` as the full undelivered set, and accumulate `output` deltas.
   Re-enter only after the prior `qp_invoke` returned.
10. Send `tool_responses` for completed calls and/or `resume` for runnable
    work.  Do not resend a call ID that is no longer in `awaiting`.
11. Treat `reason:"stalled"` + `stalled:true` as terminal; do not resume it.
    `reason:"awaiting_calls"` is non-terminal and must wait for responses.
12. Serialize all calls into one instance. Discard the instance after a wasm
    trap.
13. Enforce host-side source, message, output, memory, and time limits; the
    guest caps are only a backstop.

## Remaining limits / M1 work

- **Async tools:** proven for the narrow M0 submit-and-drive-to-quiescence
  shape.  See the async section above.  No guest-side cooperative cancellation,
  no positive timers, no stream I/O, no top-level `await` transform.
- **Cancellation/interruption:** guest-side cooperative cancellation is not
  implemented. A long-running script blocks the host's `qp_invoke` call until
  it returns; there is no guest interrupt hook, deadline, or fuel/step limit.
  The host instead forces cancellation by closing the active wazero invocation,
  which is verified in host integration.
- **Resource limits:** the guest enforces a 1 MiB output cap and a 16 KiB
  traceback cap, and the MicroPython heap is a fixed 1 MiB. It does not enforce
  execution time or a configurable heap limit. The host must cap memory,
  output, and wall time. The fixed heap size is a spike choice and may need
  tuning.
- **WASI imports:** the current artifact has none, which is good for
  capability isolation. If future MicroPython features pull in libc
  functionality, re-check imports; do not assume they stay empty.
- **Protocol:** version `1` means 0.1. The request/response shapes are
  experimental. There is no streaming, no events, no tool calls, no state
  store, and no structured traceback frames beyond MicroPython's text.
- **Error fidelity:** `error.message` is the first exception argument printed
  as text. It is not a structured value. Traceback is text only.
- **Reentrancy:** `qp_invoke` is not reentrant. The host must serialize all
  calls into one instance.
- **Wasm traps:** an unrecoverable trap (for example a wasm stack overflow or
  an out-of-bounds access) surfaces as a wazero error, not as a structured
  response. The host must treat the instance as dead and discard it.
- **Host EH feature:** wazero must enable
  `experimental.CoreFeaturesExceptionHandling`. Without it, module compilation
  fails on the tag section. This is a hard M0 integration requirement, not an
  optional optimization.
- **`qp_alloc` zero length:** returns `0`. Hosts should not allocate
  zero-length request buffers.
- **Toolchain fragility:** the build depends on Zig 0.17.1's clang 22 and
  binaryen 133 behavior. Pin both. The `setjmp_rt.c` copy must be kept in sync
  with wasi-libc if the toolchain changes.
- **Stress coverage is still thin.** The smoke harness covers boot, exception,
  fresh state, and protocol errors. A manual GC loop and a deep-recursion test
  were run during the spike, but there is no automated stress suite yet.
- **No `_start`:** this is a reactor module. Hosts must not expect a command
  module entry point.

## Files

- `guest/build.sh` — clone pin, build, strip DWARF, translate EH, emit artifact.
- `guest/port/Makefile` — MicroPython build integration.
- `guest/port/mpconfigport.h` — minimal MicroPython configuration.
- `guest/port/mphalport.h` — clock/interrupt stubs.
- `guest/port/qstrdefsport.h` — empty port qstr list.
- `guest/port/adapter.c` — ABI exports, JSON request/response, output capture,
  exception capture, GC root scanning, `_quickpy` call ledger, async response
  shapes.
- `guest/port/setjmp_rt.c` — wasm setjmp/longjmp runtime and `__c_longjmp` tag.
- `guest/port/cpp.sh` — per-file preprocessor wrapper for qstr generation.
- `guest/port/manifest.py` — freezes the M0 Python modules as source strings.
- `guest/port/modules/asyncio/` — custom frozen asyncio package adapted from
  MicroPython v1.29.0.
- `guest/port/modules/quickpy.py` — `quickpy.run`, `quickpy.tools`,
  `_HostFuture`, `ToolError`, `_deliver`, `_resume`.
- `guest/smoke/` — spike-only wazero smoke harness (not the SDK), including
  the real async tests.
- `guest/.gitignore` — ignores `.cache/` and `build/`.
