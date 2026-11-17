# quickpy-spike

Experimental Go/wazero host for the QuickPy M0 feasibility spike. This is not
the public QuickPy SDK and it is not pi-go integration.

## Usage

```sh
go run ./cmd/quickpy-spike -artifact /path/to/quickpy.wasm
```

Useful flags:

| Flag | Default | Meaning |
|---|---|---|
| `-artifact` | required | WASI reactor artifact to compile once |
| `-source` | `print("hello from quickpy")` | source for the primary execution |
| `-source2` | `print("sequential run 2")` | source for the second sequential/concurrent execution |
| `-filename` | `main.py` | filename used in guest tracebacks |
| `-timeout` | `5s` | per-execution timeout (`0` disables) |
| `-max-memory` | `64MiB` | wazero linear-memory cap |
| `-max-message` | `1MiB` | start/response message cap |
| `-max-output` | `4MiB` | captured output cap |
| `-mode` | `all` | `single,sequential,concurrent,cancel,async,async-cancel,stall,sleep0,cancel-isolation,measure,all` |
| `-cancel-timeout` | `250ms` | deadline for the synchronous cancellation smoke |
| `-cancel-source` | `while True:\n    pass\n` | source for the synchronous cancellation smoke |
| `-async-source` | gather smoke | source for the async gather proof |
| `-async-cancel-source` | gather + block smoke | source for the async cancellation proof |
| `-stall-source` | `asyncio.Event().wait()` deadlock | source for the terminal-stall proof |
| `-sleep0-source` | `asyncio.sleep(0)` smoke | source for the sleep(0) proof |
| `-other-source` | echo-tool smoke | source for the unaffected overlapping execution |
| `-cancel-isolation-suspended-source` | block-tool smoke | source for the suspended-tool explicit-cancel isolation proof |
| `-cancel-isolation-repeats` | `3` | repeats for each explicit-cancel isolation scenario |
| `-measure-samples` | `5` | samples for compile/startup measurements |
| `-measure-suspended` | `4` | concurrently live suspended executions for memory measurement |
| `-json` | `false` | emit machine-readable evidence |

`all` means the synchronous proof (`sequential`, `concurrent`, `cancel`). The
async, stall, sleep0, cancel-isolation, and measure proofs are explicit modes so
they can be run against a guest that supports the async surface.

The CLI exits nonzero if a required execution fails. A cancellation smoke that
does not actually loop is reported as `proven: false` (UNPROVEN) rather than
being treated as a host failure. Intentional failing scripts (for example
`raise ValueError("boom")`) exit nonzero and report `status: failed` with
`kind: script_error`.

## Host exception-handling requirement

The real MicroPython guest uses one WebAssembly exception-handling tag for
MicroPython `nlr` (setjmp/longjmp). wazero v1.12.0 does not enable that proposal
by default, so the host configures exactly this feature set:

```go
features := api.CoreFeaturesV2.SetEnabled(
    experimental.CoreFeaturesExceptionHandling, true,
)
rtc := wazero.NewRuntimeConfig().
    WithCoreFeatures(features).
    WithCloseOnContextDone(true).
    WithMemoryLimitPages(pages)
```

Only `CoreFeaturesExceptionHandling` is added on top of the default V2 set.
Threads, tail calls, extended const, and typed function references are not
enabled. The host uses wazero's default engine; the required feature set is
engine-agnostic, and the guest worker's harness proved both compiler and
interpreter compile with it.

## What it demonstrates

- The artifact is compiled once with wazero and instantiated fresh per
  execution (`exec-N` / `inst-N` IDs are host-generated evidence).
- Two sequential executions and two concurrent executions run in separate
  instances.
- The transient ABI is driven: `qp_abi_version`, `qp_alloc`, `qp_invoke`, and
  `qp_free`, with the response copied before freeing.
- Synchronous cancellation while `qp_invoke` is active is host-led: the context
  is cancelled, wazero `CloseOnContextDone` terminates the active entry, and no
  guest export is called after the entry returns. The result records
  `invoke_returned`, `skipped_free_on_cancel`, and per-export call counts
  (`export_calls`), so a cancelled run shows `invoke: 1` and `free: 0`.
- Async tool scheduling uses an internal execution owner: exactly-once dispatch,
  a per-call ledger with tombstones, bounded concurrent handlers, queued excess
  calls, buffered out-of-order completions, partial `tool_responses` delivery,
  `resume` for runnable work, and cancellation of every outstanding handler
  context on terminal/cancel.
- Terminal `suspended` with `reason:"stalled"` and `stalled:true` is classified
  as `Stalled` immediately. The host does not dispatch, does not send `resume`,
  and does not rely on `MaxResumes`. Contradictory combinations
  (`stalled:false`, `stalled` with calls/awaiting, `awaiting_calls` with
  `stalled:true`, `runnable` with `stalled:true`, or a stall while a call is
  still pending) are protocol errors.
- `reason:"runnable"` remains supported for protocol/future/synthetic tests, but
  the corrected real guest does not emit it; real `sleep(0)` completes without a
  host resume.
- Evidence is inspectable in the result: `requests`, `dispatch_order`,
  `completion_order`, `delivered_order`, `resume_count`, `tool_calls`,
  `call_ledger`, `export_calls`, and a bounded protocol `trace`.
- WASI is installed from wazero's prelude, but no filesystem, network, or
  environment capabilities are mounted. The M0 host allowlist is **empty**: the
  real reactor imports zero WASI functions, and any artifact import is rejected
  before instantiation. Synthetic `import_path_open.wasm`,
  `import_environ_get.wasm`, `import_args_get.wasm`, and
  `import_poll_oneoff.wasm` prove the rejection path.

## Synthetic proof (host adapter only)

`internal/wasmguest/testdata` contains tiny synthetic WASM fixtures used only
by host unit tests. They are not the QuickPy guest and are not evidence that
MicroPython-on-WASI works. They prove the host adapter's ABI, decoding,
allowlist, scheduler, and cancellation mechanics:

- `go test -count=1 ./...` passes.
- Synthetic `completed.wasm` runs two sequential and two concurrent instances.
- Synthetic `loop.wasm` with `-mode cancel -cancel-timeout 100ms` reports
  `timed_out`, `invoke_returned: true`, `skipped_free_on_cancel: true`, and
  `proven: true` at about 101 ms.
- Synthetic `failed.wasm` reports `script_error` with a structured
  `ValueError`.
- Synthetic `import_path_open.wasm`, `import_environ_get.wasm`,
  `import_args_get.wasm`, and `import_poll_oneoff.wasm` are rejected with
  `WASI import "<name>" is not in the spike allowlist`; the host allowlist is
  empty.
- Synthetic `async_gather.wasm` proves out-of-order completion and partial
  resume: `dispatch_order [1,2]`, `completion_order [2,1]`,
  `delivered_order [2,1]`, `resume_count 2`, each call dispatched/delivered
  exactly once, and output `A\nB\n`.
- Synthetic `async_serial.wasm` proves `MaxConcurrentToolCalls=1` queueing:
  `dispatch_order [1,2]`, `completion_order [1,2]`, `delivered_order [1,2]`.
- Synthetic `async_cancel.wasm` proves cancellation while suspended: status
  `timed_out`, handler context cancelled, `invoke: 1`, `free: 2`.
- Synthetic `async_duplicate.wasm`, `async_drop.wasm`,
  `async_bad_runnable.wasm`, and `async_runnable_pending.wasm` prove
  duplicate-ID, dropped-call, bad-interleaving, and runnable-with-pending-call
  rejection.
- Synthetic `async_stalled.wasm` proves terminal `stalled:true` classification:
  status `stalled`, `tool_calls 0`, `resume_count 0`, `invoke 1`, no resume.
- Synthetic `async_stalled_after_delivery.wasm` proves a stall after a delivered
  call: status `stalled`, `tool_calls 1`, `resume_count 1`, `invoke 2`.
- Synthetic `async_stalled_with_calls.wasm`, `async_stalled_false.wasm`,
  `async_awaiting_stalled.wasm`, `async_runnable_stalled.wasm`, and
  `async_stalled_pending.wasm` prove contradictory reason/flag combinations are
  protocol errors.
- Synthetic `async_runnable.wasm` and `async_runnable_loop.wasm` prove
  `resume` and `MaxResumes` for the future/synthetic runnable path.
- Synthetic `async_serial.wasm` with a panicking tool or an oversized result
  proves `handler_panic` and `result_too_large` tool error responses.

## Real MicroPython proof

Artifact used for the final integration:

```
guest/build/quickpy.wasm
195,053 bytes
sha256 4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca
```

This is the corrected guest build. It exports exactly `memory`, `_initialize`,
`qp_abi_version`, `qp_alloc`, `qp_free`, and `qp_invoke`, imports zero WASI
functions, and uses one EH tag. The earlier `runnable`+`stalled:true` anomaly is
fixed: this build emits terminal `reason:"stalled"` with `stalled:true` for a
deadlocked main task, and does not emit `runnable` for `sleep(0)`.

The guest worker's earlier stated artifact was 138,328 bytes with sha256
`88d3fffb2db0415fe3d2176e161a4a39c7267411f98b14ddcf618d0399f13b61`; the
corrected 195,053-byte build above was used for all results in this section.

### Synchronous real proof

```sh
# Hello, completed response, zero imports, 64 memory pages.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source 'print("hello from real MicroPython")' -json
# exit 0; status completed; output "hello from real MicroPython\n";
# imports []; memory_pages 64; abi_version 1.

# Sequential fresh-instance isolation.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode sequential \
  -source 'x = 42; print("set x", x)' \
  -source2 $'try:\n    print("x =", x)\nexcept NameError:\n    print("x is not defined")' -json
# exit 0; exec-1/inst-1 completed "set x 42\n";
# exec-2/inst-2 completed "x is not defined\n".

# Concurrent fresh-instance isolation.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode concurrent \
  -source 'x = 1; print("A set x")' \
  -source2 $'try:\n    print("B sees x", x)\nexcept NameError:\n    print("B no x")' -json
# exit 0; one instance prints "A set x\n"; the other prints "B no x\n".

# Structured Python exception.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source 'raise ValueError("boom")' -json
# exit 1; status failed; kind script_error;
# exception type ValueError, message "boom", traceback points at main.py line 1.

# try/except works (EH path).
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single \
  -source $'try:\n    raise ValueError("caught")\nexcept ValueError as e:\n    print("caught:", e)\nprint("after")' -json
# exit 0; status completed; output "caught: caught\nafter\n".

# Real synchronous infinite-loop cancellation.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode cancel -cancel-timeout 100ms -json
# exit 0; status timed_out; duration ~101 ms;
# invoke_returned true; skipped_free_on_cancel true; proven true;
# export_calls {initialize:1, abi:1, alloc:1, invoke:1, free:0}.
# The zero free count and single invoke count are the host-side proof that
# cancellation did not call qp_free or re-enter qp_invoke after force-close.

# Import and stdout evidence.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source 'import sys; print(sys.version)' -json
# exit 0; output "3.4.0; MicroPython v1.29.0 on 2026-11-17\n".

go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source 'print("STDOUT_EVIDENCE")' -json
# exit 0; output "STDOUT_EVIDENCE\n" inside the JSON report;
# no raw STDOUT_EVIDENCE line on host stdout; captured_stdout is empty
# because the guest adapter captures print output itself.

# Limits.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source 'print("hi")' -max-memory 1048576
# exit 1; limit_exceeded: section memory min 64 pages (4 Mi) over limit of 16 pages (1 Mi).

go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source 'print("0123456789ABCDEF")' -max-output 10 -json
# exit 1; status limit_exceeded; "guest response output exceeds 10 bytes".
```

### Real async gather / partial resume / out-of-order proof

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode async -json
```

Observed result:

- exit 0; `async.ok: true`; status `completed`.
- output `['A', 'B']\n` (gather input order preserved).
- requests: `a` (id 1, delay 200 ms) and `b` (id 2, delay 50 ms).
- `dispatch_order [1,2]`, `completion_order [2,1]`, `delivered_order [2,1]`.
- `resume_count 2`, `tool_calls 2`.
- ledger: both calls `completed`, `dispatched: true`, `delivered: true`.
- `export_calls {initialize:1, abi:1, alloc:3, invoke:3, free:6}`.
- protocol trace shows the partial resume:
  - start -> `suspended`
  - `tool_responses` with only `{id:2, value:"B"}` -> `suspended`
  - `tool_responses` with only `{id:1, value:"A"}` -> `completed`

This proves out-of-order completion, partial response delivery, exactly-once
dispatch/delivery, and gather-order output against the real MicroPython guest.

### Real async cancellation while suspended proof

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode async-cancel -cancel-timeout 100ms -json
```

Observed result:

- exit 0; `proven: true`; `handler_cancelled: true`.
- status `timed_out`; duration ~101 ms.
- requests: `a` (id 1, 10 ms) and `block` (id 2, 10 s).
- `dispatch_order [1,2]`, `completion_order [1]`, `delivered_order [1]`.
- ledger: `a` completed/delivered; `block` still `dispatched`, not completed,
  not delivered at cancellation.
- `export_calls {initialize:1, abi:1, alloc:2, invoke:2, free:4}`; no free after
  cancellation (`free == 2*invoke`), and `skipped_free_on_cancel` is false
  because cancellation happened while suspended, not during an active invoke.
- the `block` handler's context was cancelled.

### Real explicit-cancel isolation proof

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode cancel-isolation -cancel-isolation-repeats 3 -json
```

This uses `context.WithCancel` (not a deadline) for the canceled execution and
runs a second ordinary execution on the **same** compiled artifact/wazero
runtime while the first is still in flight. The second execution calls the
`echo` tool and prints `unaffected`.

Observed result (3 repeats per scenario):

| Scenario | Canceled status | Canceled export calls | Other status | Other output | Handler ctx canceled | Latency min/mean/max |
|---|---|---|---|---|---|---|
| `running-loop` | `cancelled` | `invoke:1, free:0` | `completed` | `unaffected\n` | n/a | 0.014 / 0.030 / 0.060 ms |
| `suspended-tool` | `cancelled` | `invoke:1, free:2` | `completed` | `unaffected\n` | true | 0.008 / 0.009 / 0.010 ms |

For `running-loop`, the active entry returned, `skipped_free_on_cancel` was
true, and no `qp_free` occurred after cancellation. For `suspended-tool`, the
outstanding `block` handler's context was canceled, the ledger entry remained
`dispatched` (not completed/delivered), and the start invoke's two frees were
the only frees. Execution/instance IDs were distinct in every sample; the
second execution was not a separate runtime.

### M0 measurements

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode measure -measure-samples 5 -measure-suspended 4 -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode measure -measure-samples 5 -measure-suspended 8 -json
```

Methodology:

- `compile_ms`: `NewRuntime` (read artifact + compile + WASI prelude + import
  check), 5 samples, one fresh runtime per sample.
- `startup_ms`: warm instantiation + `_initialize` only, 5 samples after one
  warm-up; no start message, source, or tool work.
- `suspended`: N concurrently live suspended executions using the real
  `block` tool; guest linear memory is read from each instance at suspension.
  RSS is process-level `ps -o rss=` in KiB, measured at stable quiescence
  (100 ms after all handlers start) and reported as a delta from the
  pre-execution baseline. It includes Go/wazero/host overhead and is not exact
  resident bytes.

Observed results on this machine:

| Metric | Result |
|---|---|
| Artifact size | 195,053 bytes |
| Compile (5 samples) | min 42.323 ms, mean 43.806 ms, max 45.214 ms |
| Warm startup (5 samples) | min 0.073 ms, mean 0.195 ms, max 0.505 ms |
| Suspended guest memory, N=4 | 100 pages = 6,553,600 bytes per instance (min=mean=max) |
| Suspended guest memory, N=8 | 100 pages = 6,553,600 bytes per instance (min=mean=max) |
| Process RSS delta, N=4 | 54,368 KiB total, ~13,592 KiB per instance (approximate) |
| Process RSS delta, N=8 | 104,832 KiB total, ~13,104 KiB per instance (approximate) |

The guest linear-memory number is exact for the measured instances. The RSS
slope is an approximate process-level bound, not a claim of exact resident
bytes; it includes wazero/Go/host overhead and varies with allocator state.
Independent verification observed similar ranges; exact compile/startup/RSS
numbers vary per run, host, and allocator state.

### Real terminal-stall proof

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode stall -json
```

Default source:

```python
import asyncio
import quickpy

async def main():
    ev = asyncio.Event()
    await ev.wait()

quickpy.run(main())
```

Observed result:

- exit 0; `proven: true`; status `stalled`; `stalled: true`.
- `tool_calls 0`; `resume_count 0`.
- `export_calls {initialize:1, abi:1, alloc:1, invoke:1, free:2}` — exactly one
  `qp_invoke`, no resume after the terminal stall.
- `invoke_returned: true`.
- error kind/status `stalled`; message `guest reported a terminal stalled run`.
- trace: start → `suspended`; no further request.

This is the corrected terminal-stall path: the host classifies `Stalled`
immediately, does not dispatch, does not send `resume`, and does not rely on
`MaxResumes`.

### Real sleep(0) proof

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode sleep0 -json
```

Default source:

```python
import asyncio
import quickpy

async def main():
    await asyncio.sleep(0)
    print("after sleep0")

quickpy.run(main())
```

Observed result:

- exit 0; status `completed`; output `after sleep0\n`.
- `tool_calls 0`; `resume_count 0`.
- `export_calls {initialize:1, abi:1, alloc:1, invoke:1, free:2}`.
- trace: start → `completed`.

The corrected guest does not emit `runnable` for `sleep(0)`; the host's
`runnable` support remains for protocol/future/synthetic tests only.

### Real unknown-tool error proof

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source $'import quickpy\n\nasync def main():\n    try:\n        await quickpy.tools.missing(value="X")\n    except quickpy.ToolError as e:\n        print("caught", e.code, e.message)\n\nquickpy.run(main())\n' -json
```

Observed result:

- exit 0; status `completed`.
- output `caught tool_not_found unknown tool "missing"\n`.
- ledger: call `missing` delivered with `error: "tool_not_found"`.
- trace: `tool_responses` with `{id:1, ok:false, error:{code:"tool_not_found", ...}}`.

Guest-side limitation observed, not a host defect:

```sh
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm \
  -mode single -source 'import math; print(math.sqrt(9))' -json
# exit 1; status failed; ImportError: module not found.
```

`import sys` works; `import math`, `import uos`, and `import gc` currently fail
with `ImportError: module not found` in this guest build. That is a guest
module/frozen-module issue, not a host protocol issue.

## Real integration status

Real host/guest integration is **proven** for the M0 transient ABI against
`guest/build/quickpy.wasm`:

- completed/hello,
- sequential and concurrent fresh instances with isolated globals,
- structured Python exceptions and `try/except`,
- real synchronous infinite-loop cancellation with `invoke_returned: true` and
  no guest free/second entry after cancellation,
- real async `asyncio.gather` with out-of-order completion, partial resume,
  exactly-once dispatch/delivery, and gather-order output,
- real async cancellation while suspended with an outstanding handler and
  handler-context cancellation,
- real explicit `context.WithCancel` cancellation for both a running
  `while True` loop and a suspended blocked tool, each with a second ordinary
  execution completing on the same runtime while the first is in flight,
- real terminal `stalled:true` classification for an `asyncio.Event.wait()`
  deadlock: status `stalled`, zero tool calls, zero resumes, exactly one
  `qp_invoke`, no resume after stall,
- real `sleep(0)` completion with zero tool calls and zero resumes,
- real catchable `tool_not_found` guest error,
- zero WASI imports and no host stdout leakage.

The M0 host allowlist is empty; the real reactor imports zero WASI functions.
Synthetic `path_open`, `environ_get`, `args_get`, and `poll_oneoff` imports are
rejected before instantiation.

The previously observed `runnable`+`stalled:true` anomaly is fixed in the
corrected guest and is now rejected as a protocol error by the host.

`guest/dist/quickpy.wasm` was not present; the proven artifact is the
intermediate `guest/build/quickpy.wasm` identified above.

## Remaining M0 blockers / risks

- The guest build requires the experimental exception-handling feature. This is
  an explicit host requirement for M0, not a temporary workaround.
- `MaxMemoryBytes` is applied at the wazero runtime level, not per instance
  (wazero API limitation); all instances share the same cap.
- `Close` while executions are in flight is not gracefully coordinated; wazero
  runtime close may interrupt them. Out of scope for M0.
- The M0 host allowlist is empty. Any future WASI import requires an explicit
  capability decision and evidence; do not add filesystem, network, environment,
  fd, or timer imports for convenience.
- Guest frozen-module coverage is incomplete: `import math`, `import uos`, and
  `import gc` fail with `ImportError: module not found`. This does not block the
  transient ABI or async proofs but is a guest capability gap.
- The process-RSS memory numbers are approximate process-level deltas, not exact
  resident bytes; the exact guest linear-memory number is 100 pages
  (6,553,600 bytes) per suspended instance in the measured build.
- The proven artifact is the intermediate `guest/build/quickpy.wasm`; no
  `guest/dist/quickpy.wasm` was present.
- No public SDK, persistence, event sink, or pi-go integration was implemented;
  intentionally out of scope for this node.

## Independent verification status (2026-11-17)

Independent verification completed on 2026-11-17:

- root `gofmt`, `go test`, `go vet`, and `go build` passed;
- 41 guest smoke checks passed;
- real `async`, `stall`, `all`, `cancel-isolation`, and `measure` modes passed;
- all 11 SPEC M0 criteria passed for the M0 baseline.

This is an M0 feasibility baseline, not a production-readiness claim.
