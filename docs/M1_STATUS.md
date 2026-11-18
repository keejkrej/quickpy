# QuickPy M1 status

**Status: full M1 accepted (independent verification, 2026-11-18).** All 12 M1
exit criteria in [`SPEC.md`](SPEC.md) §16 pass against the real artifact. This
is a milestone acceptance, **not** production GA, a stable 1.0 API, CPython
compatibility, or full Prime compatibility. The post-M1 direction is the
proposed compiled-agent target in [`AGENT_TARGET.md`](AGENT_TARGET.md); it is
not current scope.

This page is the historical M1 acceptance record. The M2 scripting foundation
is accepted separately; see [`M2_STATUS.md`](M2_STATUS.md). The M1 artifact
identity below stays as accepted; later branches may produce a different
artifact.

## Final independent verification

The independent verifier reported **GO on all 12 M1 exit criteria** with no
blocking issues. The only stale item was documentation, which this page and the
linked docs now resolve.

- **Artifact:** **236,119 bytes**, SHA-256
  `171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`.
  ABI **4**, MicroPython **1.29.0**, protocol envelope **0**, **zero** WASI
  imports, **1** EH tag, initial memory **64** pages, maximum **1024** pages,
  guest heap **4 MiB**. A clean-directory rebuild was byte-identical and the
  exports were unchanged.
- **Real guest:** **118/118** checks pass; **78/78** instances closed. The
  smoke RSS observation of ~69–72 MiB after the harness fix is historical and
  run-specific, not a per-instance guarantee.
- **Independent full tests:** **72 top-level PASS** plus `TestSoak` default skip
  (**73 top-level total**) and **74 subtests**; real integration **~15.4 s**,
  then `-race` **110.2 s**. Root tests: **58**. Internal: **126 total** (the
  verifier reports 126; use `go test -list` if a test/fuzz split is needed).
- **Soak (opt-in):** **1060 executions** in **27.84 s**, **0 errors**,
  goroutines **2/2**, RSS around **360 MiB** and stable on the reference
  machine. See [`../integration/SOAK.md`](../integration/SOAK.md) for the batch
  explanation. This is a process-level observation, not a per-instance memory
  guarantee.
- **Go gates:** `gofmt`, `go vet`, `go build`, default tests, and default-race
  pass. Repeat async/lifecycle runs passed **3×**.

## Current pieces

| Piece | Current state | Evidence tier |
|---|---|---|
| Public root package (`quickpy`) | `New`, `RuntimeOptions`, `ArtifactSource`, `RegisterTool`/`UnregisterTool`/`ToolNames`, `Execute`, `Execution.Wait`/`Cancel`/`ID`, `Close`, `Value`, `ToolCall`/`ToolHandler`/`ToolError`, `Event`/`TextEvent`/`ConsoleEvent`/`ImageEvent`/`ToolEvent`, `Result`, `Status`, `Error`/`Kind`, `GuestInfo` | Code + unit tests |
| Public root tests | **58** test functions pass, including the race suite | Independent verification GO (2026-11-18) |
| Low-level host (`internal/wasmguest`) | ABI 4 handshake, protocol envelope 0, tool dispatch, value codec, state decode, observer, cancellation, close, structured outputs | Code + unit tests |
| Low-level tests | **126 total** (test/fuzz split via `go test -list`) | Independent verification GO (2026-11-18) |
| Public real-artifact integration | **72 top-level PASS + `TestSoak` default skip (73 total) + 74 subtests**, ~15.4 s; `-race` 110.2 s | Independent verification GO (2026-11-18) |
| Integration skip behavior | Skips cleanly when `QUICKPY_TEST_ARTIFACT` is unset | Independent verification GO (2026-11-18) |
| Guest state and output slice | `get_state`/`set_state`, catchable `StateError`, last-write-wins, commit only on `completed`, IEEE 754 binary64, ABI 4 console/image outputs, distinct tool error classes, engine identity | Real artifact smoke |
| Guest smoke | **118/118 checks** pass; **78/78 instances closed** | Independent verification GO (2026-11-18) |
| Artifact | **236,119 bytes**, SHA-256 `171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`; byte-identical clean-directory rebuild | Independent verification GO (2026-11-18) |
| Guest memory | initial **64 pages / 4 MiB**, maximum **1024 pages / 64 MiB**; MicroPython heap **4 MiB** | Real artifact |
| WASI imports | **0** | Real artifact |
| Protocol envelope | **0** (ABI integer is the capability gate) | Real artifact |

## M1 exit criteria

All 12 criteria in [`SPEC.md`](SPEC.md) §16 pass:

1. **Public Go contract** — implemented for `Runtime`, `RegisterTool`,
   `Execute`, `Execution.Wait`/`Cancel`, `Close`, events, results, and errors.
2. **Conformance suites against the real artifact** — value, protocol,
   execution semantics, async tools, lifecycle, limits, and stall detection
   pass.
3. **Fresh globals / retained tools** — 100 sequential executions with no
   observable resource growth.
4. **Ten concurrent executions** — distinct globals and tools complete
   correctly.
5. **State commit only on success** — failed, cancelled, timed-out, stalled,
   and limited runs leave prior state untouched.
6. **Output ordering and truncation** — deterministic and tested, including
   text, console, and image events.
7. **`Close`** — idempotent; in-flight executions terminate; no SDK-owned
   goroutine or instance leaks in tests.
8. **Security assertions** — default-deny imports, no host filesystem/network
   access, no stdout/stderr leakage, and the exact WASI allowlist tested.
9. **Documentation** — supported Python subset, deviations, limits, and error
   kinds are documented.
10. **Normative `asyncio` subset and failure matrix** — documented in
    [`../integration/ASYNCIO.md`](../integration/ASYNCIO.md) and tested.
11. **Call-ledger conformance** — partial delivery, duplicate/unknown/late
    responses, tombstones, and no redispatch.
12. **6.6.1 resource/trap matrix** — covered by tests, including `StateWritten`
    and `ErrWaitTimeout` behavior.

The previously open items are now verified: trailing `ToolCause`, engine
version, distinct `ToolNotFoundError`/`ToolArgumentError`/`ToolLimitError`
classes, real remaining request caps, the `asyncio` matrix, the soak trend, and
full text/console/image output. ADR-0001 records the accepted memory
classification; there is no remaining target mismatch.

## Structured output and limits

These are the current M1 limits. See
[`../guest/M1_NOTES.md`](../guest/M1_NOTES.md) for the guest-side details.

- **Console text** is emitted without a trailing newline.
- **Images** are raw **1 MiB per image and 1 MiB total** in the guest; the
  base64 response can reach up to **2.4 MiB**.
- **Public `MaxMessageBytes` defaults** may reject an oversized but otherwise
  valid frame explicitly as `LimitExceeded`; the implementation does not
  silently pretend a larger frame works.
- **Guest text** is clipped at **1 MiB** with a truncation flag; output events
  are sensitive to the **1024**-event bound.
- A valid image over the output cap is decoded safely under `MaxMessageBytes`,
  then clipped and reported as `LimitExceeded`, not `ProtocolError`. Invalid
  base64 and malformed image frames are `ProtocolError`.
- Public cap `0` means inherit; there is no test that treats a literal `0` as a
  one-byte cap. The smallest explicit cap is `1`.

## Integration suite

The permanent suite lives in [`../integration/`](../integration/) and is
documented in [`../integration/README.md`](../integration/README.md). It is
gated so a plain `go test ./...` never needs the guest artifact.

```sh
# Default run: integration tests skip cleanly.
go test ./...

# Real artifact, verbose.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -v ./integration

# Real artifact under the race detector.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -race ./integration

# Opt-in soak/resource evidence.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm QUICKPY_SOAK=1 \
  go test -count=1 -run TestSoak -v ./integration
```

Covered by the permanent integration suite (independently verified PASS):

- Initial and final state, including all rollback cases: script error, stall,
  cancel, timeout, and final oversize; `set_state(None)` vs no write; deep copy
  and no mutable alias; nested JSON; int/float identity.
- Ordered `TextEvent`/`ConsoleEvent`/`ImageEvent`/`ToolEvent` delivery through
  both the request sink and `Result.Outputs`, out-of-order `asyncio.gather`,
  deterministic output truncation, and single/cumulative oversized images.
- Tool float fidelity, handler arguments/deadlines, unknown tools, distinct
  catchable tool error classes, trailing `ToolCause`, and opaque call IDs.
- Registry snapshot stability, deterministic `ToolNames`, and tool retention
  across fresh guest globals.
- Repeated/concurrent `Wait`, wait-local `ErrWaitTimeout` without cancellation,
  idempotent `Cancel`, and parent-context cancellation.
- `Close` with running and suspended executions, concurrent/idempotent closers,
  short-context early return, and a noncooperative tool that `Close` must not
  wait for.
- 100 immediate sequential executions with `MaxExecutions=1` and 10 concurrent
  distinct executions.
- Empty guest imports, unavailable `os`/`uos`/`gc` modules, and no guest
  stdout leak to the host process. Language-only modules such as `math` are
  allowed and are covered by the scripting corpus.
- Per-execution memory lower-than-runtime rejection before admission.
- The opt-in soak workload: 1060 executions, zero errors, stable goroutines.

## Regression found and fixed

The integration work found a real capacity race: an execution could close its
`done` channel **before** it was removed from the runtime's active set. A caller
that observed `Wait` returning could then issue an immediate `Execute` and
spuriously receive `CapacityExceeded` even though the previous execution was
terminal.

The fix removes the execution from the active set **before** closing `done`, so
observing terminal completion also observes the capacity release. Direct tests
cover normal completion, script error, cancel, timeout, and close, plus 100
immediate admissions with no retries. This is recorded as readiness evidence and
honest history, not as a remaining risk.

## Public semantics implemented

These are implemented in the public package and covered by unit and integration
tests. They are pre-1.0 and not a stability promise.

- **Repeat `Wait`:** `Wait` may be called repeatedly and concurrently; all
  callers see the same terminal result. A wait-context expiry returns a local
  `ErrWaitTimeout` wrapping the context error, with a zero `Result`; it does not
  change execution state, and a later `Wait` can still obtain the result.
- **Tool registry snapshot:** `Execute` snapshots the registry. Registering or
  unregistering during a run affects only later executions.
- **Close workflow:** `Close` stops admission, cancels in-flight executions,
  waits for execution owner goroutines, then closes the lower runtime. It is
  idempotent and safe for concurrent callers. A short context may return early
  while shutdown continues in the background.
- **Event sinks:** a runtime-level `EventSink` is the default; a per-execution
  `EventSink` overrides it. Sink panics are recovered and become a host fault.
- **Last state `None`:** `set_state(None)` commits `State == nil` with
  `StateWritten == true`, distinct from no write (`StateWritten == false`).
- **Per-execution memory:** only equality with `RuntimeOptions.MaxMemoryBytes`
  or zero (inherit) is accepted. A tighter per-execution value is rejected with
  `InvalidRequest` before admission; it is not silently ignored. The runtime
  cap is the only enforced memory cap.
- **Zero `MaxToolCalls`:** maps to "no cap" at the public boundary. Internally
  the engine receives an effectively unbounded sentinel; that is an
  implementation detail, not a consumer concern.
- **`ArtifactSource.Bytes`:** compiles directly from memory; the earlier
  temporary-file workaround is gone. This is a post-M1 implementation
  improvement and is not part of the M1 contract. The M1 artifact identity
  above remains the historical acceptance record.
- **`DefaultTimeout == 0`:** means no default timeout, as the spec allows; there
  is no hidden 30-second default.
- **ABI 4 required:** the host requires ABI 4 and rejects older artifacts rather
  than silently losing state, tool caps, or structured output.

## Accepted classifications and nonblocking notes

- **Accepted classification (not a deviation):** a configured runtime memory cap
  hit during guest allocation is guest-visible as `MemoryError` (or
  `ScriptError` if uncaught), not `LimitExceeded`; arbitrary traps remain
  `GuestFault`, and host compile/instantiate memory-limit errors are
  `LimitExceeded`. This is the accepted contract in
  [ADR-0001](adr/0001-guest-allocation-failures.md), not a pending bug. The
  SPEC §6.6.1 row matches it.
- **Nonblocking future-engine robustness notes:** a future engine may need an
  explicit state-commit status check and a bounded response buffer at instance
  close. The current implementation is unchanged; these are not blockers for
  M1.

## Accepted constraints and post-M1 non-goals

Full M1 acceptance is a milestone verdict, not production GA, a stable 1.0 API,
CPython compatibility, or full Prime compatibility.

- No host wall-clock timers; no top-level `await` or top-level `return`.
- Narrow MicroPython module surface; no ambient `os`, `open`, filesystem,
  network, or environment access.
- Per-execution memory below the runtime cap is rejected, not applied.
- Output is observed when a guest invocation returns, not streamed during a
  long-running yield-free C/Python section.
- OS wrappers are future `AgentScript` work and remain default-deny.
- Stable artifact distribution/embedding is future packaging work, not silently
  claimed complete.
- No pi-go integration and no Zig rewrite.
- The proposed post-M1/M2 compiled-agent direction is
  [`AGENT_TARGET.md`](AGENT_TARGET.md); it is not current scope.
