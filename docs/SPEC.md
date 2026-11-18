# QuickPy Technical Specification

**Status:** M1 implementation, not stable / v0.1
**Scope:** This document defines the intended contract and architecture. M0 feasibility is complete and independently verified (see `cmd/quickpy-spike/README.md` and `guest/M0_NOTES.md` for build hashes, commands, and measurements). The M1 public SDK + persistence increment is integration-tested but not a full M1 acceptance or release. Nothing here should be read as a claim of a finished public SDK, production hardening, full M1 acceptance, or pi-go integration. Support is explicitly experimental.

---

## 1. Purpose

QuickPy is a reusable, embeddable Python execution runtime plus a fun/learning systems project. It is **not** a from-scratch Python competitor. The deliverable value is:

- the high-level embedding and execution API,
- a narrow, explicit capability boundary for host tools,
- isolation and lifecycle guarantees per execution,
- asynchronous host-tool dispatch, cancellation, and resource limits,
- structured output and structured failure classification.

The initial engine is existing **MicroPython C** (parser, bytecode VM, GC, builtins). A guest adapter supplies the host-facing entry points, message/value codec, allocator integration where needed, and the async scheduling bridge. Those build into a **WASM/WASI** artifact. A **Go SDK** in this repository owns wazero integration, fresh-instance lifecycle, tool dispatch, scheduling, cancellation, structured results, and limits. Go does not implement Python.

The M0 spike currently uses a **C** guest adapter as a pragmatic shortcut to prove the MicroPython/WASI path; it is not a completed Zig adapter. **Zig** remains the intended later adapter/core experimentation path, and the architecture below is written for that target. A Zig rewrite of the engine is optional, future, and driven by evidence and enjoyment. MicroPython remains a useful reference even then, and any replacement engine must pass the same conformance suite.

### 1.1 What is intentionally not promised

- CPython or pip compatibility.
- Arbitrary third-party packages.
- A native C ABI in the first iteration.
- Live VM snapshots.
- JavaScript/QuickJS semantics disguised as Python semantics.
- A production-ready SDK, production hardening, or M1 completion. M0 is a verified feasibility baseline; see Section 16.

---

## 2. Goals and non-goals

### 2.1 Goals

1. Stable, implementable **Go-facing embedding contract** and **guest protocol**, independent of Go specifics so other language SDKs are possible later.
2. Execute untrusted or semi-trusted Python-like code in a **fresh WASM instance per execution**, with separate linear memory.
3. Support **async host tools**, including concurrent tool execution and `asyncio.gather`-style fan-out.
4. Serialize all guest entry; never call into one instance concurrently.
5. Provide **whole-execution cancellation** and deadline enforcement, including cancellation of outstanding tool calls.
6. Classify failures distinctly: script error, timeout, cancellation, resource limit, guest/protocol fault, stalled execution, caller misuse.
7. Preserve tool registrations across executions while clearing guest globals every time.
8. Keep the private protocol (call IDs, correlation, resume encoding) invisible to consumers.
9. Provide ordered text/console/image output with bounded size.
10. Make capability exposure default-deny: only explicitly registered tools, no ambient filesystem, network, process, environment, or native FFI access.
11. Provide an explicit JSON-like state store with load/save around executions, not VM snapshots.
12. Enable a MicroPython WASI feasibility spike without committing the architecture to unproven details.

### 2.2 Non-goals (initial implementation)

- CPython compatibility, pip installs, C extensions, `site-packages`.
- Writing a parser, compiler, bytecode VM, or GC from scratch.
- Universal SDKs for many host languages at once.
- Native C ABI or in-process `libmicropython` embedding.
- Process/container-grade OS sandboxing or strong fault isolation of the host process.
- Live "snapshot a running VM and resume it elsewhere".
- Transparent Node-style module/package resolution.
- Full CPython `asyncio`, threads, subprocesses, sockets, or signal semantics.
- Preemptive CPU fairness beyond execution deadlines/cancellation.
- A promise that a future Zig engine replaces MicroPython on any fixed schedule.

---

## 3. Terminology

| Term | Meaning |
|---|---|
| **Host** | The process embedding QuickPy, usually Go running the Go SDK. |
| **Runtime** | A long-lived QuickPy object owning configuration, compiled WASM artifact, and tool registry. |
| **Execution** | One isolated guest instance running one source submission. |
| **Guest** | The WASM artifact: QuickPy adapter + MicroPython + frozen modules. |
| **Tool** | A host capability registered by name and callable from guest Python. Fully trusted host code. |
| **Call** | One guest→host request to invoke a tool, identified privately by a call ID. |
| **Store / state** | An explicit JSON-like value handed to an execution and optionally returned on success. |
| **Owner** | The single goroutine/task allowed to enter and resume a given guest instance. |
| **Consumer** | Code using the QuickPy SDK; e.g. the pi-go integration layer. |

---

## 4. Architecture

### 4.1 Component diagram

```
┌───────────────────────────── pi-go (separate repository) ─────────────────────────────┐
│ agent loop · agent tools → host capabilities · policy/defaults · prompts · sessions   │
│ token accounting · user-facing output rendering                                       │
└───────────────────────────────────────┬───────────────────────────────────────────────┘
                                        │ ExecuteRequest / live Events / Result
┌───────────────────────────────────────▼───────────────────────────────────────────────┐
│ QuickPy (this repository)                                                             │
│                                                                                       │
│  Go SDK                                                                               │
│    Runtime (compiled module cache, tool registry, defaults)                           │
│    per-execution: fresh wazero instance, owner/scheduler, dispatch, limits,           │
│                   cancellation, event ordering, result/error mapping                  │
│                                        │                                              │
│                                        │ wazero: compile once, instantiate per exec  │
│                                        ▼                                              │
│  WASM instance (fresh linear memory per execution)                                    │
│    QuickPy guest adapter (Zig target; C in the M0 spike)                              │
│      exports: ABI handshake, message in/out                                          │
│      message/value codec, allocator integration as needed, async suspension bridge    │
│      MicroPython C: lexer/parser/compiler, bytecode VM, GC, builtins                  │
│      frozen `quickpy` module: tools, state, run() driver                              │
└───────────────────────────────────────────────────────────────────────────────────────┘
```

### 4.2 Layer responsibilities

| Layer | Owns | Must not own |
|---|---|---|
| MicroPython C | Python subset parsing/compilation, VM, GC, builtins, frozen modules | Host lifecycle, host tool policy, WASM import policy |
| Zig guest adapter (C in the M0 spike) | WASM exports, protocol framing/codec, value bridging, allocator hooks needed for the port, suspending/resuming guest work, EOF/abort signaling | Deciding tool policy, filesystem/network access, exposing `mp_obj_t` to hosts |
| Go SDK (low level) | wazero configuration, module cache, per-execution instance, allowlisted imports, memory/output/message limits, dispatch, cancellation, event ordering, result mapping | Python language semantics, pi-go policy, prompts, sessions |
| pi-go integration | Converting agent tools into QuickPy tools, application defaults, policy, token accounting, presentation, prompts, session persistence | WASM exports, guest scheduler bookkeeping, guest memory |

### 4.3 Reference behavior (from pi-go codemode/runtime and internal/quickjs)

The following existing behavior is the reference, adapted to Python:

- QuickJS-NG compiled to WASI is cached/compiled as a **module** but **instantiated fresh per `Execute`**; the low-level adapter is QuickJS-specific.
- The high-level sandbox supports tool/global registration, `Execute`, `Close`, JSON tool values/results, and structured events.
- Host tools may complete asynchronously and concurrently.
- A single owner serializes all guest VM entry.
- Globals are fresh per execution; registered tools are retained.
- State is explicit JSON load/save, never a VM snapshot.
- Cancellation covers the whole execution and outstanding tool contexts.
- Memory and stack limits are enforced.
- Text, image, and console output are ordered.
- There are no host filesystem/network imports.
- Failures are structured: script, timeout, abort/cancel, sandbox/infrastructure.
- QuickJS Promises map **conceptually** to MicroPython `asyncio`. This specification does not claim a Promise-equivalent API, top-level async, or identical scheduling semantics exists today.

---

## 5. Repository and ownership boundary

**QuickPy repository owns:**

- guest adapter sources and build recipe for the WASM artifact,
- MicroPython pin/vendoring and build configuration,
- the private guest protocol and its versioning,
- low-level Go/wazero adapter,
- generic Go SDK and public embedding contract,
- value model, limits, error taxonomy,
- conformance test suite and protocol fixtures,
- documentation of Python-subset deviations.

**pi-go owns:**

- `ExecuteCodemode` integration,
- converting agent tools into QuickPy tool registrations,
- application defaults and policy (which tools, limits, deadlines),
- token accounting and user-facing output presentation,
- system prompts and session storage.

**Rules:**

- QuickPy must not import pi-go.
- pi-go must not need to understand WASM exports, memory layout, call IDs, or implement guest scheduling/tool bookkeeping.
- Guest and Go SDK start in the same repository while the protocol is unstable. Splitting is allowed later only behind a versioned protocol.
- Consumers see `Runtime`, tools, requests, events, results, and errors only.

---

## 6. Public Go contract (M1 implementation, not stable)

This section describes the **M1 implementation**. Names are now concrete in the
root `quickpy` package, but the API is **pre-1.0** and may still change. The
semantics here are normative unless a status deviation is listed in
[`M1_STATUS.md`](M1_STATUS.md). M1 is not accepted as a full release yet.

### 6.1 Core types

```go
type Runtime struct { /* unexported */ }

type ArtifactSource struct {
    Path  string   // exactly one of Path or Bytes
    Bytes []byte
}

type RuntimeOptions struct {
    Artifact          ArtifactSource     // required; Path or Bytes
    MaxMemoryBytes    int64              // required > 0; runtime-level linear-memory cap
    MaxExecutions     int                // required > 0; simultaneous in-flight executions
    DefaultTimeout    time.Duration      // 0 explicitly means no default timeout
    MaxSourceBytes    int                // 256 KiB
    MaxMessageBytes   int                // 1 MiB per protocol message
    MaxOutputBytes    int                // 4 MiB text+console+image
    MaxToolResultBytes int               // 1 MiB
    MaxStateBytes     int                // 1 MiB
    MaxToolCalls      int                // 0 explicitly means no cap
    MaxConcurrentToolCalls int           // 8
    MaxPendingCalls   int                // 64; ledger safety cap
    MaxResumes        int                // 10000; scheduler safety cap
    EventSink         func(Event)        // optional default sink
}

type ExecuteRequest struct {
    Source        string
    Filename      string        // required; used in tracebacks and syntax errors
    InitialState  Value         // optional; copied into the fresh instance
    Timeout       time.Duration // 0 = inherit runtime default; may only tighten
    MaxMemoryBytes int64        // 0 = inherit; nonzero must equal the runtime cap
    MaxOutputBytes int          // 0 = inherit; may only lower
    MaxToolCalls   int          // 0 = inherit; may only lower
    EventSink      func(Event)  // optional; overrides runtime default
}

type Execution struct { /* unexported */ }

func New(opts RuntimeOptions) (*Runtime, error)
func (r *Runtime) RegisterTool(name string, h ToolHandler) error
func (r *Runtime) UnregisterTool(name string) error
func (r *Runtime) ToolNames() []string
func (r *Runtime) Execute(ctx context.Context, req ExecuteRequest) (*Execution, error)
func (r *Runtime) Close(ctx context.Context) error

func (e *Execution) ID() string
func (e *Execution) Wait(ctx context.Context) (Result, error)
func (e *Execution) Cancel()   // idempotent
```

Tool types:

```go
type ToolHandler func(ctx context.Context, call ToolCall) (Value, error)

type ToolCall struct {
    Name        string
    Args        Value        // decoded object; kwargs only
    ExecutionID string
    Deadline    time.Time    // zero if none
}

// ToolError supplies a structured, guest-safe tool failure.
// Any other non-nil handler error is also surfaced to the guest as a ToolError
// with a generic code, unless it reflects execution cancellation/deadline.
type ToolError struct {
    Code    string   // short machine code, e.g. "permission_denied"
    Message string   // safe for the guest
}

func (e *ToolError) Error() string
```

Value types:

```go
// Value is restricted to: nil, bool, int64, float64, string, []Value, map[string]Value.
// It is not a generic escape hatch for arbitrary Go values.
type Value any
```

Consumers must not have to encode/decode protocol frames or touch WASM memory.

### 6.2 Execution lifecycle

1. `New` validates options, loads/compiles the artifact **once**, and prepares the tool registry. Compilation may be lazy but is shared and concurrency-safe.
2. `Execute` validates the request (filename, source size, limits, runtime state), snapshots the current tool registry, allocates a **new** WASM instance with fresh memory, configures a fresh guest limits/policy set, starts the execution owner, and returns a handle.
3. Validation failures return an immediate `error` from `Execute` and no handle.
4. `Wait` observes the execution. It may be called multiple times; concurrent and repeated calls return the same terminal result/error. `Wait`'s context bounds only the waiting, not the execution. If that context expires before the execution is terminal, `Wait` returns `ErrWaitTimeout` (wrapping the context error); this is not cached as the execution's terminal error, and a later `Wait` can still obtain completion. To stop execution use `Cancel` or cancel the context passed to `Execute`.
5. Cancelling the `Execute` context requests cancellation of the execution.
6. `Cancel` is idempotent. Cancelling a finished execution is a no-op.
7. `Close` stops accepting new executions, cancels in-flight executions, and is idempotent. It waits until guest entries and SDK-owned bookkeeping have stopped or its context expires; it does not await handlers that ignore cancellation. If the context expires, shutdown continues in the background and `Close` returns the context error. The compiled module and instances are released only once guest entries have actually stopped. Subsequent `Execute` calls return a `RuntimeClosed` error.

Terminal statuses: `Completed`, `Failed`, `Cancelled`, `TimedOut`, `LimitExceeded`, `Stalled`. A guest trap or protocol violation is a `Failed` result with kind `GuestFault`/`ProtocolError`; it never hangs the handle. On any non-nil terminal error, `Result` may still carry partial ordered outputs, `Status`, and call counts, but `State` is nil and `StateWritten` is false; callers must check the error before using the result. A wait-local `ErrWaitTimeout` returns a zero `Result` and does not change execution state.

### 6.3 Registration and mutation semantics

- Tool names match `^[A-Za-z_][A-Za-z0-9_]{0,63}$` and are unique per runtime.
- Duplicate registration returns an error and leaves the previous handler in place.
- Registry mutation is concurrency-safe.
- An execution uses a **snapshot** taken at `Execute` time. Register/unregister during a run affects only later executions; it never changes the current run's tool set.
- A tool removed after an execution started remains callable in that execution, because the execution uses its snapshot. This is deliberate: tool sets are stable for an execution's lifetime.
- Tools are trusted capabilities. Registering a tool is equivalent to granting that capability to every execution that snapshots it. Host applications should scope runtimes accordingly.

### 6.4 Runtime options vs per-execution options

- Runtime options are policy ceilings and defaults: memory ceiling, output ceiling, default timeout, tool-call cap, max concurrency, execution cap, artifact, default event sink.
- `MaxExecutions` means **simultaneous in-flight executions**, not total executions over the runtime's lifetime. When the cap is reached, `Execute` returns `CapacityExceeded` immediately; it does not block.
- Per-execution options select source, filename, initial state, stricter limits/timeout, and a sink override.
- Per-execution options may only tighten, never loosen, runtime ceilings. A looser request is rejected as `InvalidRequest`. A zero per-execution limit means "inherit the runtime value", not "unlimited". A per-execution timeout cannot disable or extend a runtime default; if the runtime default is 0 (explicitly no default), a per-execution timeout may set one.
- **Memory is the exception to tightening.** The current engine enforces only the runtime-level `MaxMemoryBytes`; it cannot apply a tighter per-execution cap. `ExecuteRequest.MaxMemoryBytes` must therefore be zero (inherit) or exactly equal to `RuntimeOptions.MaxMemoryBytes`. Any other value is rejected with `InvalidRequest` before admission. This is an explicit unsupported rejection, not a silent no-op.
- `MaxMemoryBytes` and `MaxExecutions` are required and must be > 0. All other numeric limits must be > 0 except `MaxToolCalls` (0 explicitly means no cap) and `DefaultTimeout` (0 explicitly means no default timeout). QuickPy bounds guest linear memory by the runtime-level `MaxMemoryBytes`; the current engine does not enforce a separate per-instance cap. It does not bound all host memory (Go heap, wazero overhead, tool handlers). Hosts must budget those separately to support a no-host-OOM claim.
- Limits are resolved before the instance starts and passed to the guest in the start message so the guest can reject oversized tool results early.

### 6.5 Events and outputs

```go
type Event interface{ isEvent() }

type TextEvent    struct { Text string }               // guest stdout
type ConsoleEvent struct { Level string; Text string }  // guest diagnostics/log API
type ImageEvent   struct { MIME string; Data []byte }   // via the dedicated quickpy.image() output API
type ToolEvent    struct { Phase string; Name string; CallID string; Error string } // CallID is opaque diagnostic only
```

- Events are delivered to the sink **serially per execution** by the SDK; the sink is never invoked concurrently for one execution. A slow sink backpressures execution progress; a sink must not call `Wait` or `Cancel` on the same execution in a blocking way.
- The sink is optional. Regardless of sink presence, the terminal `Result` contains the ordered outputs produced so far (subject to limits), so consumers that only need final output can ignore events. On failure, outputs are partial and `State` is nil.
- Text, console, and image ordering is the guest's emission order. Tool events are interleaved at the point the SDK observes the call start/finish. Interleaving from concurrent guest tasks is whatever the guest runtime produced; the host must not reorder by timestamp.
- `ToolEvent.CallID` is an opaque diagnostic identifier only. Consumers cannot use it to resume, correlate, or address calls, and it is never exposed to Python. It may be omitted in a future revision without changing the contract.

`Result` shape:

```go
type Status string // Completed, Failed, Cancelled, TimedOut, LimitExceeded, Stalled

type Result struct {
    ExecutionID  string
    Status       Status      // set for every terminal result
    Outputs      []Event     // ordered text/console/image events, bounded
    State        Value       // committed state; nil if no write or write was None
    StateWritten bool        // true iff set_state() was called and committed
    ToolCalls    int
    Duration     time.Duration
    Guest        GuestInfo   // engine name/version, protocol version
}
```

`StateWritten` distinguishes `set_state(None)` (State nil, StateWritten true) from no state write (State nil, StateWritten false). `Result.HasState()` is a convenience returning `StateWritten`; it is not a separate source of truth. State writes are last-write-wins and commit only on successful completion.

### 6.6 Errors

`Execute` and `Wait` return `*Error` with a `Kind`:

| Kind | Meaning | Terminal status | Guest-visible? |
|---|---|---|---|
| `InvalidRequest` | Caller misuse, rejected before/without a run | none | No |
| `CapacityExceeded` | `MaxExecutions` reached; no instance started | none | No |
| `Reentrant` | Handler attempted to enter the same execution; rejected deterministically | none | No |
| `ScriptError` | Guest raised an uncaught Python exception | `Failed` | N/A (it is the exception) |
| `TimedOut` | Execution timeout exceeded | `TimedOut` | No |
| `Cancelled` | Explicit cancel or parent context cancellation | `Cancelled` | No |
| `LimitExceeded` | Configured memory, output, message, pending-call, resume, or tool-result limit hit; state limit detected at finalization | `LimitExceeded` | Sometimes catchable before hard abort; see 6.6.1 |
| `Stalled` | Guest cannot progress: no runnable work, no awaiting calls, no timers, not complete | `Stalled` | No |
| `GuestFault` | WASM trap not attributable to a configured limit, internal guest fault | `Failed` | No |
| `HostFault` | Recovered panic or internal bug in the host adapter/tool dispatch | `Failed` | No |
| `ProtocolError` | Malformed/truncated protocol message, ABI mismatch, unknown message type, ledger violation | `Failed` | No |
| `RuntimeClosed` | Runtime already closed | none | No |

`Wait` may also return the sentinel `ErrWaitTimeout` when only its own context expires; that is not a terminal `*Error` and does not change execution state.

Error shape:

```go
type Error struct {
    Kind        Kind
    Status      Status            // zero for non-terminal errors
    ExecutionID string
    Op          string            // "execute", "wait", "close", ...
    Message     string
    Cause       error
    Script      *ScriptErrorInfo  // Kind == ScriptError
    Tool        *ToolCause        // uncaught tool failure, if any
    Diagnostics map[string]string // bounded, non-sensitive debug context
}

type ScriptErrorInfo struct {
    Type      string // Python exception type
    Message   string
    Traceback string // bounded, mapped to original filename/lines
    Filename  string
    Line      int
}

type ToolCause struct {
    Tool    string
    Code    string
    Message string
}
```

`ScriptError` includes the Python exception type, message, formatted traceback mapped to the provided `Filename` and original line numbers, and optionally a `ToolCause` when an uncaught `ToolError` ended the script.

Host tool mistakes are distinct from script failures:

- Returning `*ToolError` → catchable `quickpy.ToolError` in Python with the supplied code/message. Any other non-nil handler error is also catchable, with code `tool_error` and a sanitized message. If uncaught, the execution ends as `ScriptError` with `ToolCause` set; the host also sees the corresponding `ToolEvent`.
- Returned `context.Canceled`/`context.DeadlineExceeded` while the execution context is done → cancellation/timeout path, not catchable.
- Unknown tool name (e.g. a script uses a tool not in its snapshot) → catchable `quickpy.ToolNotFoundError` plus a `ToolEvent`; it does not by itself abort the run.
- A panic in a handler, an invalid/unsupported result value, or an oversized result → host failure: execution aborts with `HostFault` (panic/internal bug), `ProtocolError` (invalid value), or `LimitExceeded` (oversized) as applicable. Panics are recovered and must never crash the host process.
- Invalid tool arguments from the script → catchable `quickpy.ToolArgumentError`; the handler is not invoked.

#### 6.6.1 Resource and trap classification

| Condition | Python-visible | Terminal `Kind` |
|---|---|---|
| Guest heap allocation fails and the VM can raise | catchable `MemoryError` | none if caught; `ScriptError` if uncaught |
| Configured guest heap/WASM memory cap hit and cannot be raised | none | `LimitExceeded` |
| Unattributed guest OOM or trap | none | `GuestFault` |
| Output cap exceeded | none (hard abort) | `LimitExceeded` |
| Source exceeds `MaxSourceBytes` | none (pre-execution) | `InvalidRequest` |
| Tool arguments exceed value/message limits | catchable `ToolArgumentError` | none if caught; `ScriptError` if uncaught |
| Tool result exceeds `MaxToolResultBytes` | none | `LimitExceeded` |
| Total tool-call cap exceeded | catchable `ToolLimitError` | none if caught; `ScriptError` if uncaught |
| Pending-call cap exceeded | none (host abort) | `LimitExceeded` |
| Resume cap exceeded | none (host abort) | `LimitExceeded` |
| Stack overflow the VM can raise | catchable `RecursionError` | none if caught; `ScriptError` if uncaught |
| Stack overflow that cannot be raised | none | `LimitExceeded` if a stack cap is configured, else `GuestFault` |
| Guest rejects a state value (unsupported type, cycle, depth, non-finite float, etc.) | catchable `StateError` | none if caught; `ScriptError` if uncaught |
| Committed state exceeds the host `MaxStateBytes` at finalization | none | `LimitExceeded`; state is not committed |
| Malformed or truncated protocol frame | none | `ProtocolError` |
| Configured protocol message cap exceeded | none | `LimitExceeded` |
| Arbitrary WASM trap | none | `GuestFault` |

Partial outputs are retained on every terminal error. State is never committed on error, including `LimitExceeded` during finalization. `GuestFault` is reserved for traps not attributable to a configured limit; configured memory exhaustion is `LimitExceeded`, not `GuestFault`.

### 6.7 Tool call semantics

- Python-facing callables are exposed under `quickpy.tools.<name>` and are **async** functions: `await quickpy.tools.search(query="x")`.
- Only keyword arguments are accepted; they form a string-keyed object. Positional arguments are rejected as `ToolArgumentError`.
- Arguments are validated/encoded before dispatch; cycles, unsupported types, depth overflow, or size overflow fail in the guest as a catchable error and never produce a partial host call.
- The handler receives an execution-derived `context.Context` that is cancelled when the execution is cancelled/times out/closes. It also receives the deadline.
- A handler may run concurrently with other handlers of the same execution (bounded by `MaxConcurrentToolCalls`). A handler may still be running when the guest resumes with other responses; it must never call into the guest. Handlers for one execution may run in any order.
- Handler calls for different executions are independent.
- Re-entry into the same execution from a handler is rejected deterministically with a `Reentrant` error; the SDK never blocks waiting for itself. Starting a different execution from a handler is allowed but subject to `MaxExecutions` and host policy.
- Call IDs and response correlation are private. Guest resume is serialized by the owner. `ToolEvent.CallID` is diagnostic only and cannot be used to resume or correlate calls.

### 6.8 Guest protocol (private, v0)

Transport: JSON messages over WASM linear-memory buffers, length-prefixed. Exports are private implementation details; hosts must not use them directly.

Exports:

- `qp_abi_version() -> i32` — major/minor packed; checked at instance start.
- `qp_alloc(len) -> ptr`, `qp_free(ptr, len)` — message buffer management; callable only while no guest entry is active.
- `qp_invoke(ptr, len) -> i64` — the sole guest entry point; returns pointer/length of a response buffer (or an error sentinel).

`qp_invoke` is the only execution entry. `qp_abi_version` is called once before execution; `qp_alloc`/`qp_free` are called only while no entry is active. While an entry is active, the host must not call any guest export or send any message. Cancellation while `Running` is host-led forced termination (Section 7.4), not a guest message. Cooperative abort is permitted only while `Suspended`.

Messages carry `v`, `type`, `seq`, and correlation IDs where relevant. Guest→host message types: `started`, `output`, `suspended`, `state_write`, `completed`, `failed`, `aborted`. Host→guest: `start` (source, filename, resolved limits, tool catalog metadata, initial state), `tool_responses`, `resume`, `abort` (only while `Suspended`).

Every entry is synchronous from the WASI perspective: the guest runs until it completes, faults, or returns control with a suspension. No host→guest re-entry occurs while the guest is executing. `qp_invoke` must distinguish "source returned" from "main task done": a source return with rooted tasks still pending is a suspension, not completion (Section 8.1).

### 6.9 Call ledger and suspension protocol (protocol v0)

The host and guest maintain a per-execution call ledger. The protocol is private, but v0 must be implementable independently by both sides. The names below match the current implementation; a separate `call_requests` wire type is a possible future refactor, not part of v0.

- A `suspended` response carries `reason` (`runnable`, `awaiting_calls`, `stalled`, or future `awaiting_timer`), `calls` (a **delta** of calls newly requested since the previous guest→host message), `awaiting` (the complete list of call IDs requested but not yet delivered to the guest), and an `output` delta. Each `calls` entry has `call_id`, `name`, and `args`. `call_id` is opaque, unique for the execution, never reused, and generated by the guest.
- Reason semantics are normative: `awaiting_calls` is **nonterminal** and requires a non-empty `awaiting`; a resume while a call is still pending returns `awaiting_calls` again. `stalled` is **terminal**, carries `stalled: true`, and requires the main task unfinished with no runnable work, no undelivered calls, and no armed timers; a stalled suspension must never be resumed. `runnable` requires an empty `awaiting`; it is reserved but not emitted by the current real build.
- Host ledger states: `requested` → `dispatched` → `response_ready` → `delivered` → `completed`. `requested` on a `suspended.calls` entry; `dispatched` when the handler starts; `response_ready` when the handler returns and the response is buffered; `delivered` when included in `tool_responses`; `completed` when a later guest suspension no longer lists it or the execution terminates. Terminal entries are tombstoned until execution terminal.
- Exactly-once dispatch: each call is dispatched at most once and is never redispatched after a later suspension. Calls above `MaxConcurrentToolCalls` remain `requested`/queued and are dispatched as slots free.
- Responses may become ready while the guest is `Running` or `runnable`; they are buffered as `response_ready` and never cause guest entry. Only the serialized owner delivers them, and only while the guest is `Suspended`, via a `tool_responses` entry or a `resume` entry.
- The owner may deliver a subset of ready responses. The guest must tolerate partial delivery and re-list undelivered calls in its next `suspended.awaiting`.
- `tool_responses` may contain only call IDs currently in `awaiting` and in `response_ready`; anything else is a `ProtocolError`.
- Duplicate guest call IDs are a `ProtocolError`. A response for an unknown call ID is a `ProtocolError`. A duplicate response for a call already `response_ready` or `delivered` is a `ProtocolError`. A response arriving after terminal is discarded. Tombstones remain until terminal.
- The guest must not drop an awaiting call without a response. Cancellation of an in-flight tool await is unsupported in MVP; doing so is a `ProtocolError`. A future `cancel_calls` message may relax this.
- `abort` is host→guest and may be sent only while `Suspended`; it is a cooperative normal abort, not forced cancellation.
- The guest's `stalled` reason is the authoritative terminal stall signal. Host-side detection (no runnable work, no awaiting calls, no timers, no completion) remains a fallback for a guest that fails to report it.

---

## 7. Execution state machine and scheduling

### 7.1 States

```
Validate ──▶ Starting ──▶ Running ⇄ Suspended ──▶ Finalizing ──▶ Completed
                              │          │
                              └──────────┴──────────────▶ Failed | Cancelled | TimedOut
                                                        | LimitExceeded | Stalled
```

- **Validate:** host-side request checks; no instance.
- **Starting:** instantiate, ABI handshake, send `start`.
- **Running:** guest is executing; owner is inside `qp_invoke`. No other entry allowed.
- **Suspended:** guest returned control without completing. The guest reports why: `runnable` (immediate resume, e.g. a zero-delay yield; reserved, not emitted by the current real build), `awaiting_calls` (one or more awaiting host calls), `stalled` (terminal: main unfinished, no runnable work, no undelivered calls, no timers), or later `awaiting_timer`. Owner dispatches calls; guest resume is serialized.
- **Finalizing:** guest completed; host commits state only if status is success, releases instance.
- **Terminal:** exactly one result; instance is closed.

### 7.2 Scheduling rules

1. At most one active guest entry per execution, always by the owner.
2. When the guest awaits host work, it returns a `suspended` response with `calls` deltas and `awaiting` metadata. The host validates the ledger (Section 6.9) before dispatching.
3. The host dispatches newly requested calls up to `MaxConcurrentToolCalls`; excess calls remain queued. A call is never redispatched after a later suspension.
4. The owner resumes the guest when at least one response is ready or the guest reported `runnable`; responses may arrive out of order and may be delivered partially. The guest scheduler decides which Python tasks become runnable.
5. The host must not resume with a response for an unknown, duplicate, or non-awaiting call ID; that is a `ProtocolError`.
6. A tool call whose response arrives after the execution reached a terminal state is discarded; its context is already cancelled. Tombstones remain until terminal.
7. On script completion, the host cancels outstanding call contexts, rejects late responses, and closes the instance.
8. **Cancellation:** while `Running`, cancellation is host-led forced termination; no guest message or extra export entry is used. While `Suspended`, the host may send a cooperative `abort` on the next entry. See Section 7.4.
9. **Stalled detection:** a `Suspended` response with reason `stalled` (and `stalled: true`) is terminal: the host must fail the execution as `Stalled` and must never resume it. As a fallback, a suspension with no runnable work, zero awaiting calls, zero armed timers, and no completion is also `Stalled`. A `runnable` suspension is resumed immediately (subject to `MaxResumes`). A run that never returns is covered by the timeout/forced cancellation path.
10. `MaxPendingCalls` and `MaxResumes` are configured defaults and are enforced as `LimitExceeded`; they are not optional hardening.

### 7.3 Timers

Timer support is deliberately conservative and **settled for the M0/M1 baseline**:

- Positive timers are deliberately unsupported and rejected. `time.sleep`, `asyncio.sleep(delay>0)`, and similar positive-delay waits raise a clear catchable `quickpy.UnsupportedError` rather than silently hanging or busy-waiting. This rejection has independent evidence.
- Zero-delay yields (`asyncio.sleep(0)`) and task switching are allowed.
- No host-driven clock is built for M0/M1: the current artifact imports zero WASI functions, and reliable timer-free scheduling meets the spike's needs. A host-clock feature is deferred until a concrete protocol and cancellation tests are designed; no future clock is promised.
- A suspension with no runnable work, no awaiting calls, and no timers is a hard error, not an invitation to sleep.

### 7.4 Cancellation

- Whole-execution cancellation covers guest execution and every outstanding tool context.
- `Cancel`/context cancellation is idempotent and races safely with natural completion.
- **While `Running` (including a synchronous `while True: pass` loop):** cancellation is host-led forced termination:
  1. Atomically latch the terminal cause (`Cancelled`, `TimedOut`, or `LimitExceeded`) and mark the execution terminal-pending.
  2. Cancel every outstanding tool context.
  3. Do not call any guest export (including `qp_alloc`, `qp_free`, or `qp_invoke`) and do not send `abort` while the active entry is running.
  4. Cancel the active invocation context with wazero `CloseOnContextDone` enabled, forcing the active entry to terminate.
  5. Wait for the active entry to return before releasing the instance, buffers, or module resources.
  6. Classify the result from the latched host cause, not from the forced trap.
- **While `Suspended`:** cooperative normal abort is permitted. The host sends `abort` on the next entry; the guest returns an aborted result; classification comes from the latched cause. Forced close is used only if the guest fails to return. The M0 guest does not implement cooperative abort; host-led forced close is the verified M0 path.
- Any grace period concerns only host handler cleanup; handlers need not be awaited. There is no guest cooperative grace period while `Running`.
- Tool handlers observing their context should return promptly; if they do not, their results are discarded and they may continue in the background. The SDK does not await handlers that ignore cancellation; it waits only for the guest entry and SDK-owned bookkeeping. The execution still reaches a terminal state after the active entry returns.
- Timeout is cancellation with classification `TimedOut`; explicit cancel is `Cancelled`.

---

## 8. Python source semantics

### 8.1 Baseline (MVP proposal)

- Source is executed as ordinary module-like top-level code in a fresh namespace: `__name__ == "__main__"`, `__file__` is the provided filename.
- Import statements behave like MicroPython module imports for the allowlisted/frozen modules. There is no package manager, no `sys.path` from the host, and no implicit host modules.
- **Top-level `await` is not supported** in the baseline because ordinary Python does not allow it. A top-level `await` is a syntax error reported against the original source.
- **Top-level `return` is not supported.**
- Async host tools require an async context. The driver is:

```python
import asyncio
import quickpy

async def main():
    results = await asyncio.gather(
        quickpy.tools.search(query="a"),
        quickpy.tools.search(query="b"),
    )
    print(results)

quickpy.run(main())
```

`quickpy.run(coro)` is **submit-and-drive-to-quiescence**, not a literally blocked Python call. It submits one main task, drives the guest scheduler until every task is either awaiting a host response or complete, then returns normally through the scheduler, NLR, C, and adapter stack. Later `tool_responses`/`resume` protocol entries drive the rooted tasks. This is the MicroPython analogue of a top-level async driver, not JavaScript top-level `await` and not `asyncio.run`.

Consequences that are part of the baseline:

- Statements after `quickpy.run(main())` execute **before** pending tools finish. For the current M0/MVP baseline, `quickpy.run(...)` must be the **final top-level statement**; code after it is not a supported way to observe tool results.
- `qp_invoke` must distinguish "source returned" from "main task done". A source return with rooted tasks still pending is a suspension, not completion.
- C coroutine/generator frames, tasks, and futures remain rooted across the export boundary. No active C scheduler or NLR frame survives an export.
- The stock `asyncio.run_until_complete` cannot be repeatedly driven for this; the current implementation customizes the loop. The exact primitives and support matrix remain M0 work (Section 8.2).
- Tool callables require an active guest scheduler. Calling one outside a running `quickpy.run` context raises `RuntimeError`; creating a coroutine without awaiting it performs no host interaction (standard Python behavior).
- Errors, syntax errors, and tracebacks must report the provided filename and original source line numbers.
- There is exactly one guest thread. Threads and multiprocessing are unsupported.

### 8.2 Resolved M0 questions and remaining M1 work

1. The spike demonstrates suspend/resume of rooted tasks across `quickpy.run` for the basic `gather` path, independently verified, including corrected `awaiting_calls`/`stalled` reasons and forced-GC allocation pressure with pending calls. Broader GC/NLR/task-rooting stress and lifetime remain M1 hardening.
2. Top-level `await`/`return` are not supported. If top-level `await` is later desired, prefer MicroPython compiler support (`MICROPY_COMP_ALLOW_TOP_LEVEL_AWAIT` / `mp_compile_allow_top_level_await`) and root/drive the returned module generator (cf. upstream `ports/webassembly/proxy_c.c`) over naive indent/wrap transforms. It preserves module names, imports, and line numbers better, but is **not implemented or accepted**; top-level `return` remains unsupported.
3. The stock `asyncio` loop cannot be repeatedly driven for this model; the current implementation customizes it. The exact supported primitives and failure matrix remain M1 work.
4. What exact subset of `asyncio` is supported (`gather`, `wait_for`, `sleep(0)`, `create_task`, `Lock`, `Queue`)? Unsupported primitives must fail loudly at call time. A normative subset/failure matrix is still required for M1 acceptance.
5. `quickpy.run` is once per execution; a second call raises. Whether it can be re-entered after a suspension is not part of the baseline.
6. How are imports of the `quickpy` module resolved (frozen module vs generated bootstrap)?
7. Should the execution expose a final structured result (e.g. `quickpy.result(value)`) or only outputs? Not in MVP unless the spike makes it cheap.

No source transformation is promised. If it is adopted, its semantic deltas and line mapping must be documented and tested before M1.

---

## 9. Value model and tool payloads

Initial payloads are JSON-like only. This is a deliberate capability and complexity boundary, not a claim that Python values are JSON.

### 9.1 Mapping

| Python | Wire/SDK |
|---|---|
| `None` | null |
| `bool` | bool |
| `int` | signed 64-bit integer; outside range is rejected |
| `float` | finite IEEE-754 double; NaN/±Inf rejected |
| `str` | UTF-8 string; unpaired surrogates rejected |
| `list`, `tuple` | array (tuples normalize to lists on decode) |
| `dict` | object; keys must be strings, otherwise rejected |
| `bytes`, `bytearray`, `set`, `frozenset`, custom objects, exceptions | unsupported, rejected |

### 9.2 Normalization and constraints

- Unsupported types are rejected, never silently stringified.
- Cycles are rejected. Shared references (a DAG) are serialized by value; identity is not preserved.
- Object key order is preserved by the codec but is not semantic.
- Duplicate keys cannot occur after Python `dict` construction; on the host side, duplicate JSON keys are a protocol error.
- Defaults (configurable): maximum depth 64, maximum message size 1 MiB, maximum string length 256 KiB, maximum total nodes 100k. Exceeding them fails safely before dispatch or before resume.
- Integers and floats stay distinct: an integer token decodes to int64, a token with `.`/exponent decodes to float64. The encoder must emit a decimal point or exponent for every float64 (including integral values such as `1.0`) so the distinction survives. Consumers needing exact integer semantics must use `int64`.
- Tool results are subject to the same rules; an invalid result is a host failure, not a script error.
- `mp_obj_t`, pointers, and raw guest structs must never appear in the host-facing API or be accepted from consumers.

---

## 10. Async tools and concurrency

- Guest tool functions are awaitables. Awaiting one yields control to the host with a private call request.
- Multiple awaits may be pending concurrently (`asyncio.gather`), and the host dispatches them up to `MaxConcurrentToolCalls`.
- Responses are correlated by private call ID; completion order is irrelevant to correctness.
- Guest resume remains serialized. Concurrency exists only among host tool executions and among guest tasks as scheduled by the guest runtime.
- Ordering of tool calls as **observed by the host** is not a language guarantee; ordering of tool results as **observed by the script** follows the awaited API.
- A tool call must not block the owner goroutine. Handlers that do CPU-heavy work must be bounded by the host application; the execution timeout is the only QuickPy-level backstop.
- Nested/re-entrant guest entry from a tool handler is forbidden. A handler that calls back into the same execution is rejected deterministically with a `Reentrant` error (Kind `Reentrant`); the SDK never blocks waiting for itself.
- Tools are dispatched by name from the execution snapshot; name lookups are case-sensitive.

---

## 11. Output and errors

### 11.1 Output

- Guest `print`/stdout becomes ordered `TextEvent`s, captured through the adapter or a strictly redirected WASI `fd_write`. Guest stdout must never leak to the host process stdout.
- Guest diagnostics/logging become `ConsoleEvent`s with a small level enum (`debug`, `info`, `warn`, `error`).
- Images are emitted only through a dedicated guest API, `quickpy.image(data, mime="image/png")`, which accepts `bytes` and sends a separate binary output message. This channel is distinct from JSON tool values; arbitrary guest binary writes and binary tool payloads remain unsupported.
- Output is bounded by `MaxOutputBytes`; exceeding it aborts with `LimitExceeded`. Partial output delivered before the limit remains available in events; the result's `Outputs` is truncated consistently and the error says so.
- Output ordering is the guest's emission order. The host must not sort or merge concurrent chunks by wall clock.

### 11.2 Error presentation

- Script errors show the original filename and line numbers, the Python exception type/message, and a traceback bounded by a configurable size.
- Host tool failures appear in `ToolEvent` diagnostics and, if uncaught, as `ToolCause` on the script error.
- Host-side misuse (bad limits, missing filename, registration errors, executing after `Close`) is a normal Go error, not a guest failure, and must not be wrapped as `ScriptError`.
- Guest/protocol faults are `*Error` with a diagnostic string plus a stable kind. They must include enough context (phase, call ID where safe, last protocol message type) to debug without exposing guest memory.
- A panic anywhere in the host adapter is recovered and converted to `HostFault` where possible; it must not escape `Wait`.

---

## 12. Persistence

- Persistence is an **explicit JSON-like state value**, never a live VM image.
- `InitialState` is deep-copied into the fresh guest before source execution and exposed via `quickpy.get_state()`.
- A script may replace state with `quickpy.set_state(value)`. There is no `quickpy.state` attribute; the functions are normative. An invalid value raises a catchable `quickpy.StateError` and leaves the previous valid state unchanged.
- State is written to the host only if the execution completes successfully **and** a state write occurred. Failed, cancelled, timed-out, and limited executions must not commit state.
- The result contains the committed state or nil. The store is schema-less from QuickPy's perspective; the consumer owns its shape and versioning.
- State size is bounded by `MaxStateBytes` and value depth limits. The guest rejects an invalid value with a catchable `StateError`; if the committed value exceeds the host `MaxStateBytes` at finalization, the execution ends as `LimitExceeded` and the state is **not** committed. The previous state remains untouched.
- There is no snapshot, no serialization of Python objects, no resumption of a partially run program.
- Tool registrations are runtime configuration, not state; they survive executions. Globals, imported modules' mutable state, and the heap do not.

---

## 13. Limits and security

### 13.1 Isolation model

- Each execution gets a fresh WASM instance with its own linear memory. Compiled code may be shared; memory and globals are not.
- Tool registration survives; guest globals, module caches, and heap do not.
- Simultaneous executions do not share guest memory and cannot observe each other's Python objects.
- WASM is a **guest memory and capability boundary, not OS process fault isolation**. A guest bug should not corrupt Go memory, but the host, wazero, and trusted tool implementations remain in the trusted computing base (TCB). A malicious trusted tool can do anything the host permits.
- Native pointer/GC bugs in the guest are contained inside the WASM instance, which also suits the project's experimental nature; a host crash caused by MicroPython C running natively is explicitly out of scope for the WASM path.

### 13.2 Capabilities

- Default-deny imports. Only the minimal WASI surface needed to boot is allowlisted, determined and documented during M0. The real guest artifact imports **zero** WASI functions, and the host `allowedWASIFuncs` set is empty; `path_open`, `environ_get`, `args_get`, and `poll_oneoff` are rejected before instantiation. No filesystem, network, process, environment, clock-with-side-effects, or native FFI imports.
- Wazero is configured with defaults plus `experimental.CoreFeaturesExceptionHandling` only, because MicroPython `nlr` requires one EH tag. No broad feature or capability expansion.
- stdout/stderr, if imported, are redirected to capture or discarded; they never reach real host descriptors.
- Arguments/env are controlled by the host, not inherited.
- Only registered tools are callable. There is no ambient Python access to host APIs.
- Frozen/VFS modules, if added later, are an explicit, reviewed allowlist. No host path mounting by default.
- Tool arguments/results are data only; no executable handles, pointers, or host objects cross the boundary.

### 13.3 Resource limits

- **Time:** execution timeout enforced via context. While `Running`, cancellation is host-led forced termination using wazero `CloseOnContextDone`; the host waits for the active entry to return and classifies from the latched cause. No guest interrupt export is required. While `Suspended`, cooperative `abort` is the intended path; the M0 guest does not implement it, so forced close is used.
- **Memory:** `MaxMemoryBytes` is required and is applied at the wazero runtime level; the current engine does not enforce a separate per-instance cap. A per-execution `MaxMemoryBytes` must be zero (inherit) or exactly equal to the runtime value; a tighter value is rejected with `InvalidRequest`. A configured cap hit is `LimitExceeded`; a catchable `MemoryError` is possible only when the VM can still raise. Unattributed OOM/traps are `GuestFault`. QuickPy bounds guest linear memory, not all host memory; hosts must budget wazero overhead and tool handlers.
- **Stack:** guest recursion/stack limit enforced by MicroPython configuration or the adapter; overflow becomes a catchable `RecursionError` where possible, else `LimitExceeded` if a stack cap is configured, otherwise `GuestFault`.
- **Output:** total output bytes, per-message size, per-tool-result size, state size.
- **Calls:** `MaxToolCalls`, `MaxConcurrentToolCalls`, `MaxPendingCalls`, and `MaxResumes` are configured defaults and are enforced. Exceeding `MaxToolCalls` raises catchable `ToolLimitError`; exceeding `MaxPendingCalls` or `MaxResumes` is `LimitExceeded`.
- **Instance:** `MaxExecutions` is required and bounds simultaneous in-flight executions. Guest linear memory is bounded by the runtime-level `MaxMemoryBytes`, not by a per-instance cap.
- Stack traces and protocol buffers are bounded to avoid host OOM on a hostile guest.

### 13.4 Threats explicitly not mitigated

- A malicious host application or malicious trusted tool.
- CPU side channels, speculative execution, or OS-level escapes from wazero bugs.
- Denial of service through host-side tool implementations that ignore cancellation.
- Perfect fairness or preemptive scheduling inside the guest.
- Untrusted native code loaded into the host process.

---

## 14. Compatibility and versioning

- **Guest protocol:** integer major/minor version, checked via `qp_abi_version()` during startup. The current host requires **ABI 2** (packed 0.2) and rejects 0.1 artifacts with a protocol/ABI error rather than silently losing initial state. A future minor mismatch is accepted only if the Go SDK declares support.
- **Artifact:** embeds engine name/version and protocol version; `Result.Guest` reports them for diagnostics. `GuestInfo.Version` is currently blank (see [`M1_STATUS.md`](M1_STATUS.md)).
- **Python compatibility:** the language is "MicroPython subset", not CPython. Document supported syntax/modules, known deviations, and the exact MicroPython pin. `sys.version` must identify the engine honestly.
- **SDK API stability:** pre-1.0, names may change. Semantics in this document (lifecycle, isolation, error taxonomy, value rules) are the compatibility target.
- **Protocol portability:** the protocol must not depend on Go. A future SDK in another language should be able to implement the same control flow, even though only Go is targeted initially.
- **Engine replacement:** a new engine (Zig or otherwise) is compatible only by passing the conformance suite and reporting its identity; consumers may opt in or pin a backend. The WASM sandbox assumptions must not silently degrade.

---

## 15. Conformance and testing strategy

M0 acceptance is complete and independently verified. The M1 public SDK + persistence increment now has a permanent real-artifact integration suite in [`../integration/`](../integration/), documented in [`../integration/README.md`](../integration/README.md). Current evidence tiers are: **guest smoke** (66 checks against the real artifact, 41 M0 + 25 M1), **host synthetic** (Go unit tests, including the public root package and the low-level `internal/wasmguest` suite), and **real-artifact public integration** (40 top-level tests plus 14 subtests, ~7.9 s; race run ~40.3 s; skips cleanly when `QUICKPY_TEST_ARTIFACT` is unset). These integration results are worker-reported; independent verification is being started. The suite covers state rollback, events/order/truncation, tool float fidelity, registry snapshot/retention, repeated wait/wait-local timeout, close running/suspended/short-context/noncooperative, 100 sequential and 10 concurrent executions, empty imports/security, and per-execution memory rejection. Remaining gaps are request-specific caps not all exercised with the real artifact, real protocol/guest-fault coverage, a normative `asyncio` matrix, and a dedicated soak/resource-leak trend. Items already covered by M0 evidence are noted in Section 16; current M1 pieces and open gates are tracked in [`M1_STATUS.md`](M1_STATUS.md).

1. **Value conformance:** round-trip matrix for null/bool/int boundaries/float edges/strings (including Unicode and rejected surrogates)/nested containers; rejection of NaN, Inf, int overflow, cycles, non-string keys, unsupported types, depth/size overflow; DAG duplication; int/float distinction.
2. **Protocol conformance:** ABI handshake, unknown message, duplicate response, unknown call ID, oversized frame, truncated frame, malformed JSON, sequence mismatch, ledger transitions, duplicate guest call IDs, partial response delivery, no redispatch after later suspensions, and tombstone behavior. All must produce a structured error, not a hang or panic.
3. **Execution semantics:** fresh globals across runs; registrations retained; imports restricted; filename/line mapping in syntax errors and tracebacks; state load/commit-only-on-success.
4. **Async tools:** `gather` of several tools completing out of order; one slow tool plus one fast tool; cancellation while suspended with outstanding calls; cancellation of a running synchronous infinite loop; the other execution unaffected in both cases; no concurrent export entry during forced cancellation (a suspended cooperative abort is the single next entry); tool raising catchable errors; uncaught tool error mapping; unknown tool; invalid arguments; concurrent-call cap.
5. **Lifecycle:** concurrent executions isolated; simultaneous `Execute` with distinct globals; `Cancel` idempotence; `Close` idempotence and behavior under load; no goroutine/fd/instance leaks over many runs.
6. **Limits:** memory cap, output cap, message cap, tool-call cap, pending-call cap, resume cap, state cap, timeout; classification correctness against the 6.6.1 matrix; timeout/cancel latency bound.
7. **Stall detection:** a guest `stalled` reason is terminal and never resumed; a guest that suspends without awaiting calls or timers also fails as `Stalled`; a guest that loops without returning is caught by the timeout/forced cancellation path.
8. **Security:** attempts to import filesystem/network/process modules fail; no host stdout/stderr leakage; the exact WASI import allowlist is asserted, including proof that stdout/stderr capture does not write to host descriptors.
9. **Backend conformance:** the same suite runs against any future guest backend. Engine-specific tests may exist but cannot replace protocol tests.
10. **Fuzzing:** value codec and protocol decoder fuzz targets; guest source fuzzing is optional until stability.

Tests should run the real guest artifact, not a mock, for protocol and execution suites. Unit tests may mock the transport for scheduler edge cases.

---

## 16. Milestones and acceptance criteria

### M0 — Feasibility spike (MicroPython on WASI)

Questions: does MicroPython build to WASI; can the adapter link; can Go/wazero instantiate fresh instances; can a guest suspend on host tools and resume; what are size, startup, and memory costs; what are the top-level async and timer constraints?

**Evidence status (M0 final, independently verified):** MicroPython v1.29.0 builds into a reactor WASI/WASM artifact using Zig 0.17.1 as the C toolchain plus binaryen/wasm-opt 133; the spike adapter is C, not Zig. Wazero v1.12.0 requires `experimental.CoreFeaturesExceptionHandling` in addition to defaults because MicroPython `nlr` uses one required EH tag; no-EH was investigated and is not viable without redesigning `nlr`. No broad features or WASI capabilities are enabled. The real guest artifact imports **zero** WASI functions and exports `memory`, `_initialize`, `qp_abi_version`, `qp_alloc`, `qp_free`, `qp_invoke`. The artifact was reproduced twice on this machine: **195,053 bytes**, sha256 `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`, with exact Zig 0.17.1 / wasm-opt 133 / MicroPython v1.29.0 pins.

The final independent verifier reports **all 11 M0 exit criteria PASS** and a **GO** for merge as an M0 feasibility baseline (not a production SDK). Evidence: 41 guest smoke checks; `gofmt`, `go test`, `go vet`, and `go build` pass; real async, stall, limit, cancellation-isolation, and measurement runs pass. Real `context.WithCancel` running-loop and suspended-tool cancellation each passed 3/3 as `Cancelled` (not timeout), with the other execution in the same runtime completing `unaffected\n`; running invoke count 1 / free 0 / invoke-returned true; suspended invoke 1 / free 2 / handler context canceled; request-to-return latency 0.008–0.060 ms in an independent run. Host `allowedWASIFuncs` is empty; `path_open`, `environ_get`, `args_get`, and `poll_oneoff` are rejected before instantiation; guest imports are zero.

Independent measurements (informational, run-specific): compile mean ~39.8 ms (read + compile + prelude + import check); warm instantiate + `_initialize` mean ~0.193 ms, range 0.060–0.426 ms over the final N=8 run; suspended linear memory exactly 100 pages / 6,553,600 bytes per instance; process RSS approximately 11.5–11.8 MiB per instance in the independent N=4/8 run. RSS is approximate host-process memory, not an exact per-instance figure; suspended quiescence was not a long soak. Commands, hashes, and measurements live in `cmd/quickpy-spike/README.md` and `guest/M0_NOTES.md`.

Exit criteria (measurable):

1. Reproducible build of a WASI artifact containing MicroPython plus the adapter, with the exact toolchain and MicroPython pin recorded. *(PASS: reproduced twice, 195,053 bytes, sha256 `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`, Zig 0.17.1 / wasm-opt 133 / MicroPython v1.29.0)*
2. Go program using wazero compiles the artifact once and runs two simultaneous executions in separate instances with separate memory; a global set in one is not visible in the other. *(PASS: independently reproduced)*
3. A script performs two tool calls in `asyncio.gather`; handlers complete out of order (controlled delay); the script receives correct correlated results. A true deadlock terminates as `Stalled`; a resume while a call is still pending returns nonterminal `awaiting_calls` and later completes. *(PASS: independently verified against the real guest)*
4. **Cancellation while suspended with an outstanding tool:** one execution ends as `Cancelled`; the tool's context is cancelled; the other execution is unaffected; no concurrent export entry occurs during cancellation (a suspended cooperative abort, if used, is the single next entry). Target latency is **≤250 ms**, measured on the development machine with a warm compiled module, forced wazero close, and no handler drain. If not achievable, record the actual latency and treat it as an M0 finding requiring a decision before M1. *(PASS: 3/3 `Cancelled` not timeout, other execution `Completed` `unaffected\n`, handler context canceled, request-to-return 0.008–0.060 ms)*
5. **Cancellation/timeout of a running synchronous loop** (`while True: pass`): forced wazero close ends the execution as `Cancelled`/`TimedOut`; the other execution is unaffected; no extra export entry occurs while the active entry is running; the active entry returns before instance release. Use the same ≤250 ms target and measurement conditions as item 4. *(PASS: 3/3 `Cancelled` not timeout, other execution `Completed` `unaffected\n`, invoke 1 / free 0 / invoke-returned true)*
6. A memory cap is enforced and an allocation bomb returns a structured `LimitExceeded`/`MemoryError` path without crashing the host. *(PASS: memory bomb and output cap reproduced without host crash)*
7. The exact WASI import allowlist is recorded (not a category). The real guest artifact imports zero WASI functions; if a later build imports any, each must be justified, with no filesystem, network, process, or environment imports. Prove stdout/stderr capture and non-leakage to host descriptors. *(PASS: guest imports zero; host `allowedWASIFuncs` empty; `path_open`/`environ_get`/`args_get`/`poll_oneoff` rejected preinstantiation; no stdout leak)*
8. A written answer to the top-level async question (Section 8.2) with either a working `quickpy.run` suspension design or a concrete transform proposal including line-mapping plan. *(PASS for the submit-and-drive-to-quiescence design; full asyncio subset remains M1 work)*
9. A decision on initial timer behavior and evidence for or against a host-driven clock. *(PASS: positive timers deliberately rejected for M0/M1; no host clock built; defer until a concrete protocol and cancellation tests exist)*
10. Startup latency, artifact size, and steady-state memory per instance are measured and recorded. These are **informational in M0**; before M1 they become viability targets and inform default limit selection. M0 does not invent pass/fail thresholds for them. *(PASS: artifact 195,053 bytes; compile mean ~39.8 ms; warm instantiate+`_initialize` mean ~0.193 ms, range 0.060–0.426 ms, N=8; suspended linear memory 100 pages / 6,553,600 bytes per instance; process RSS ~11.5–11.8 MiB per instance, approximate and run-specific)*
11. No blocker requiring a native C ABI. If one appears, document it and stop to reassess. *(PASS: no blocker reported)*

**M0 verdict:** all 11 criteria PASS; **GO** for merge as an M0 feasibility baseline. This is not a production SDK and does not complete M1. Remaining hard limits and production TODOs are M1+: public SDK surface, persistent state/events, graceful `Close` under load, per-execution memory configuration given the runtime-level limitation, a narrow frozen-module set, positive timers unsupported, no top-level `await`, `quickpy.run` must be the final top-level statement, no OS sandbox or pi-go integration, and the experimental wazero EH requirement.

M0 could have failed; a failure would have been a valid outcome that redirects the design (e.g. transform-only semantics, native-only trusted backend, or engine choice) before M1. It did not fail.

### M1 — SDK MVP

**Status: coherent public SDK + persistence increment, integration-tested; not
a full M1 acceptance or release.** The checklist below is the acceptance target,
not a claim that every item is done. The permanent real-artifact integration
suite is worker-reported passing (40 top-level tests plus 14 subtests, ~7.9 s;
race run ~40.3 s) and skips cleanly without `QUICKPY_TEST_ARTIFACT`; independent
verification is being started. Current coverage, the capacity-race regression
that was found and fixed, and open gates are tracked in
[`M1_STATUS.md`](M1_STATUS.md); do not read an unchecked item as either passed
or failed without checking that page.

Exit criteria:

1. Public Go contract implemented for `Runtime`, `RegisterTool`, `Execute`, `Execution.Wait/Cancel`, `Close`, events, results, and errors as specified (names may evolve).
2. Value conformance, protocol conformance, execution semantics, async tool, lifecycle, limit, and stall-detection suites pass against the real artifact.
3. Fresh-globals/retained-tools behavior verified across at least 100 sequential executions with no observable resource growth.
4. Ten concurrent executions with distinct globals and tools complete correctly.
5. State commits only on success; failed runs leave prior state untouched.
6. Output ordering and truncation are deterministic and tested.
7. `Close` is idempotent; in-flight executions terminate; no SDK-owned goroutine or instance leaks in tests (handlers that ignore cancellation are not awaited).
8. Security assertions: default-deny imports, no host filesystem/network access, no stdout/stderr leakage, and the exact WASI allowlist is tested.
9. Documentation of supported Python subset, deviations, limits, and error kinds.
10. A normative supported `asyncio` subset and failure matrix is documented; `gather` was demonstrated in the M0 spike.
11. Call-ledger conformance passes, including partial delivery, duplicate/unknown/late responses, tombstones, and no redispatch.
12. The 6.6.1 resource/trap matrix is covered by tests, including `StateWritten` and `ErrWaitTimeout` behavior.

### M2 — pi-go integration and hardening

- pi-go implements `ExecuteCodemode` on the SDK, converts agent tools, applies policy/defaults, renders outputs, and accounts usage, without importing WASM details.
- Soak tests, crash/trap classification, timeout/cancel latency, and protocol fuzz coverage.
- Revisit timers only if a concrete protocol and cancellation tests justify it; source transformation and additional `asyncio` primitives based on real usage.

### M3 — Optional engine work

- A Zig or other engine is considered only after M1/M2 evidence: startup/size/performance limits, maintainability, or learning goals.
- Any replacement must pass the full conformance suite and preserve sandbox assumptions.

---

## 17. Open questions

Items marked **(M0)** are resolved historical questions. The rest are M1
remaining work unless noted as settled.

1. MicroPython's scheduler returns to the host at await points and resumes rooted tasks for the basic `gather` path, independently verified. Broader GC/NLR/task-rooting stress and lifetime remain M1 hardening. (M0)
2. Top-level `await` is unsupported. If later desired, prefer MicroPython compiler support (`MICROPY_COMP_ALLOW_TOP_LEVEL_AWAIT` / `mp_compile_allow_top_level_await`) and root/drive the returned module generator over naive transforms; not implemented or accepted. Top-level `return` remains unsupported. (M0)
3. The real guest artifact imports zero WASI functions and the host allowlist is empty; `path_open`, `environ_get`, `args_get`, and `poll_oneoff` are rejected before instantiation. If later builds import any, the exact allowlist and controlled stdout capture without filesystem access must be recorded. (M0)
4. Exact supported `asyncio` subset and its failure modes. Still required for M1 acceptance.
5. Timers: settled for M0/M1 as positive timers rejected and no host clock; revisit only with a concrete protocol and cancellation tests. Stall detection distinguishes "sleeping" from "stuck" by rejecting positive sleeps rather than treating them as progress. (settled)
6. Whether guest `print`/console output should be separate streams or a single ordered stream with levels.
7. Should executions expose a final structured result beyond output/state?
8. Default limit values and measurement-based tuning. The no-loosening rule is settled: per-execution limits may only tighten runtime ceilings, except memory, where only inherit-or-equal is supported until the backend can enforce a tighter per-execution cap.
9. State API shape: resolved for M1 as `quickpy.get_state()`/`set_state()` with no `quickpy.state` attribute. Multiple named stores remain out of scope unless a concrete need appears.
10. Whether tool metadata/schemas belong in QuickPy or only pi-go.
11. Distribution of the artifact (embedded via `go:embed`, external file, cache) and reproducibility of builds.
12. Multiple guest backends in one Runtime and negotiation rules.
13. Debug/tracing hooks and whether they can exist without weakening isolation.
14. Whether a separately proven non-entry/poll cancellation mechanism is worth adding. It is not required for MVP and must not reintroduce concurrent guest entry.

---

## 18. Alternatives considered

| Alternative | Why not now | When it could return |
|---|---|---|
| **Native C adapter to MicroPython in-process** | Fast and easy to debug, but cannot reproduce WASM memory/capability assumptions; guest pointer bugs crash the host; no per-execution linear-memory isolation; undermines the security story. | Later as a trusted-code-only backend, explicitly opted in, after the WASM contract is stable. It cannot be assumed equivalent. |
| **Subprocess with CPython** | Mature async and ecosystem, but heavy startup, OS process/container dependence, weaker embeddability, and pulls the project toward CPython/pip compatibility rather than the embedding API. Also less fun as a systems project. | Possibly a comparison backend or tooling aid, never the default contract. |
| **Adopt MicroPython directly and stop there** | MicroPython already runs Python; the missing value is exactly the embedding/lifecycle/capability/scheduling layer QuickPy provides. Direct adoption would not define a stable consumer contract. | MicroPython remains the initial engine and a reference, not the product surface. |
| **Rewrite parser/VM/GC in Zig immediately** | Unbounded effort, unproven payoff, and would delay the actual differentiator. Learning value is real but can be pursued incrementally. | Optional later, gated on evidence and enjoyment, must pass conformance. |
| **Reuse the QuickJS runtime design wholesale** | JavaScript semantics (Promises, top-level await) do not transfer to Python; pretending they do would create a misleading contract. | The lifecycle, isolation, dispatch, and error patterns are reused as reference; the language semantics are not. |
| **Native C ABI first** | Consumers do not need it; it adds a second unstable boundary before the protocol and SDK settle. | Only if a non-Go, non-WASM host is a real requirement. |

---

## 19. Recommended implementation order

1. **Spike the guest build**: get MicroPython plus an adapter compiling to WASI; record toolchain, pin, artifact size, and boot behavior. (M0 used a C adapter with Zig as the C toolchain; a Zig adapter remains the later target.)
2. **Spike minimal protocol**: ABI handshake, start, one print, completion, one failure, one trap; prove fresh instances and memory limits in wazero.
3. **Spike tool suspension and cancellation**: one `await` tool call, then two concurrent calls with out-of-order responses; implement the call ledger; test cancellation while suspended with an outstanding tool and cancellation of a running `while True: pass` loop, including the unaffected second execution and no concurrent export entry (a suspended cooperative abort is the single next entry).
4. **Answer the semantics questions**: `quickpy.run` submit-and-drive semantics (basic path demonstrated), top-level await/return policy, line mapping, timers, supported `asyncio` subset.
5. **Freeze protocol v0 and the public Go shape** for the MVP, including the call ledger and host-led forced cancellation, then implement value codec and its conformance tests first.
6. **Implement the Go SDK lifecycle**: Runtime, registry snapshot, fresh instance, owner, events, Wait/Cancel/Close, error taxonomy.
7. **Implement limits and security assertions** before adding conveniences: memory, output, messages, timeout, stalls, import allowlist.
8. **Implement state, filename/line mapping, and ordering tests.**
9. **Run the full conformance suite; fix scheduling and cancellation edge cases.**
10. **Integrate with pi-go** (`ExecuteCodemode`, tool conversion, policy, presentation) without leaking WASM/protocol details.
11. **Soak/fuzz/harden**, then revisit timers, transforms, and optional engine work only with evidence.
