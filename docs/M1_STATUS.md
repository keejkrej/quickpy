# QuickPy M1 status

**Status:** the current `m1/public-sdk` increment is a **coherent public SDK +
persistence increment, integration-tested against the real artifact**. It is
**not a full M1 acceptance or release**. This page records what the increment
contains and which gates remain. It does not change the historical M0 record in
[`../guest/M0_NOTES.md`](../guest/M0_NOTES.md) or the M0 verdict in
[`SPEC.md`](SPEC.md) §16.

**Independent verification (2026-11-18): GO, no blockers.** The verifier
reproduced the real artifact byte-identically, ran the default and race suites,
and repeated the real-artifact integration three times. This is a GO for
publishing the increment as a single PR from `m1/public-sdk`; it is **not** a
full M1 acceptance or production-readiness verdict. See
[`HANDOFF.md`](HANDOFF.md) §9 for the current publication pointer.

## Current pieces

| Piece | Current state | Evidence tier |
|---|---|---|
| Public root package (`quickpy`) | `New`, `RuntimeOptions`, `ArtifactSource`, `RegisterTool`/`UnregisterTool`/`ToolNames`, `Execute`, `Execution.Wait`/`Cancel`/`ID`, `Close`, `Value`, `ToolCall`/`ToolHandler`/`ToolError`, `Event`/`TextEvent`/`ConsoleEvent`/`ImageEvent`/`ToolEvent`, `Result`, `Status`, `Error`/`Kind`, `GuestInfo` | Code + unit tests |
| Public root tests | **48 test functions** pass, including the race suite | Independent verification GO (2026-11-18) |
| Low-level host (`internal/wasmguest`) | ABI 2 handshake, start/response protocol, tool dispatch, value codec, state decode, observer, cancellation, close | Code + unit tests |
| Low-level tests | **83 test functions + 1 fuzz target = 84** | Independent verification GO (2026-11-18) |
| Public real-artifact integration | **40 top-level tests + 14 subtests PASS**, ~7.9 s; race run PASS, ~40.3 s; **3 repeat runs PASS** | Independent verification GO (2026-11-18) |
| Integration skip behavior | Skips cleanly when `QUICKPY_TEST_ARTIFACT` is unset | Independent verification GO (2026-11-18) |
| Guest state slice | `quickpy.get_state()` / `set_state()`, catchable `StateError`, last-write-wins, commit only on `completed`, IEEE 754 binary64 floats, ABI 2 | Real artifact smoke |
| Guest smoke | **66/66 checks** pass (41 M0 + 25 M1), including persistence, IEEE double, and zero WASI imports | Independent verification GO (2026-11-18) |
| Artifact | **220,134 bytes**, sha256 `be1b8e48b6884e3e9e2a59c758a2abceac564adb5162b691e5a6b6cd3eac9c4d`; **byte-identical rebuild** | Independent verification GO (2026-11-18) |
| Guest memory | initial **64 pages / 4 MiB**, maximum 1024 pages / 64 MiB; MicroPython heap 4 MiB | Real artifact |
| WASI imports | **0** | Real artifact |

The independent verifier reproduced the full default and race root and internal
commands, the real-artifact integration suite, and three repeat integration
runs. The artifact rebuilt byte-identically at the size and hash above. No
blocking issues were reported.

The old M0 artifact metrics (195,053 bytes, 100-page suspended memory, ~0.193 ms
warm start) are **stale** for the M1 artifact and must not be quoted as current.
The current guest uses a 4 MiB heap and 4 MiB initial linear memory; M0
startup/RSS measurements must not be reused blindly, and new M1 memory
measurements are not yet recorded here.

## Integration suite

The permanent suite lives in [`../integration/`](../integration/) and is
documented in [`../integration/README.md`](../integration/README.md). It is
gated so a plain `go test ./...` never needs the guest artifact.

Exact gate commands:

```sh
# Default run: integration tests skip cleanly.
go test ./...

# Real artifact, verbose.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -v ./integration

# Real artifact under the race detector.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -race ./integration
```

Covered by the permanent integration suite (independently verified PASS):

- Initial and final state, including all rollback cases: script error, stall,
  cancel, timeout, and final oversize; `set_state(None)` vs no write; deep copy
  and no mutable alias; nested JSON; int/float identity.
- Events, ordering, and truncation: ordered `TextEvent`/`ToolEvent` through both
  the request sink and `Result.Outputs`, out-of-order `asyncio.gather`,
  deterministic output truncation, and no synthetic console/image events.
- Tool float fidelity, handler arguments/deadlines, unknown tools, catchable
  handler errors, and opaque call IDs.
- Registry snapshot stability, deterministic `ToolNames`, and tool retention
  across fresh guest globals.
- Repeated/concurrent `Wait`, wait-local `ErrWaitTimeout` without cancellation,
  idempotent `Cancel`, and parent-context cancellation.
- `Close` with running and suspended executions, concurrent/idempotent closers,
  short-context early return, and a noncooperative tool that `Close` must not
  wait for.
- 100 immediate sequential executions with `MaxExecutions=1` and 10 concurrent
  distinct executions.
- Empty guest imports, unavailable `os`/`uos`/`gc`/`math` modules, and no guest
  stdout leak to the host process.
- Per-execution memory lower-than-runtime rejection before admission.

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
- **`ArtifactSource.Bytes`:** currently written to a temporary file for
  compilation and cleaned up afterwards. This may be optimized later; it is not
  part of the contract.
- **`DefaultTimeout == 0`:** means no default timeout, as the spec allows; there
  is no hidden 30-second default.
- **ABI 2 required:** the host requires ABI 2 and rejects 0.1 artifacts rather
  than silently losing initial state.

## Known deviations from SPEC targets

These are current implementation constraints, recorded honestly. They do not
rewrite the normative SPEC targets; the SPEC remains the target contract.

- **Distinct tool error classes:** SPEC names `ToolNotFoundError`,
  `ToolArgumentError`, and `ToolLimitError` as distinct Python-visible classes.
  The current guest exposes a single `quickpy.ToolError` with a `code` field
  (for example `tool_not_found`). A `MaxToolCalls` overrun is currently
  classified by the host as `LimitExceeded`, not as a catchable
  `ToolLimitError`. The SPEC target is unchanged; this is an implementation gap
  to close before full M1 acceptance.
- **Runtime WASM cap trap:** a trap caused by the runtime-level WASM memory cap
  currently maps to `GuestFault`, not `LimitExceeded`, because the host cannot
  attribute the trap to a configured per-execution limit at that boundary. The
  SPEC classification target is unchanged.
- **Nonblocking future-engine robustness notes:** the verifier noted that a
  future engine may need an explicit state-commit status check and a bounded
  response buffer at instance close. The current implementation is unchanged;
  these are not blockers for this increment.

## Open M1 gates

- Request-specific `MaxSourceBytes`, `MaxToolCalls`, `MaxPendingCalls`,
  `MaxResumes`, and tool-result caps are not all covered with the real artifact;
  synthetic tests cover some.
- Real protocol/guest-fault coverage is synthetic.
- An uncaught structured `ToolCause` is **not populated** on `Error.Tool` yet.
- `GuestInfo.Version` is **blank** even though the type has the field.
- `ConsoleEvent` and `ImageEvent` are exposed types, but the real guest does not
  yet emit console or image output; only text output is produced. The image
  types are documentation/API surface only for now.
- No per-instance memory enforcement; `MaxMemoryBytes` is runtime-level only,
  and a lower per-execution value is rejected rather than applied.
- No host wall-clock timers and no top-level `await`/`return`.
- Imports and frozen modules remain narrow.
- No stable artifact distribution/embedding contract yet.
- A complete normative `asyncio` subset/failure matrix is still needed.
- A dedicated soak/resource-leak trend is still needed. The basic close
  scenarios are covered; comprehensive close-under-load maturity is deferred.
- No pi-go integration and no Zig rewrite.
- Execution event streaming happens when a guest invocation returns, not during
  a long-running yield-free C/Python section.

## What is still needed for full M1 acceptance

1. ~~Independent verification of the integration and race results.~~ **Done:
   independent verification GO, 2026-11-18.**
2. Real-artifact coverage for the remaining request-specific caps and real
   protocol/guest-fault cases.
3. A normative supported `asyncio` subset and failure matrix.
4. A dedicated soak/resource-leak trend.
5. Resolution or explicit acceptance of the deviations above.
6. A committed, reviewed, and published M1 branch/PR.

Until those land, M1 is **not accepted as a full release**.
