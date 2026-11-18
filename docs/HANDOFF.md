# QuickPy handoff

This document is for a new agent picking up QuickPy on a new machine. It
describes what exists, how to verify it, what is still open, and how the
repository is being handed over.

## 1. Start here

1. Read [`../README.md`](../README.md) for the short orientation.
2. Read [`M1_STATUS.md`](M1_STATUS.md) for the accepted M1 evidence, limits, and
   constraints.
3. Read [`SPEC.md`](SPEC.md) for the intended contract and milestone criteria.
4. Read [`../guest/M0_NOTES.md`](../guest/M0_NOTES.md) for historical M0 guest
   build details, pins, the transient ABI, and evidence.
5. Read [`../guest/M1_NOTES.md`](../guest/M1_NOTES.md) for the M1 guest
   state/output slice, ABI 4, and current artifact evidence.
6. Read [`../cmd/quickpy-spike/README.md`](../cmd/quickpy-spike/README.md) for
   the host CLI and the exact evidence commands.
7. Check the current git state and the M1 conformance PR (see §9).
8. Read [`AGENT_TARGET.md`](AGENT_TARGET.md) for the proposed post-M1
   compiled-agent direction; it is not current scope.

QuickPy is an M0-accepted feasibility baseline plus a **full M1-accepted public
SDK + persistence milestone**. M1 acceptance is a milestone verdict, not
production GA, a stable 1.0 API, CPython compatibility, or pi-go integration.
The public entry point is the root `quickpy` package (`api.go`, `runtime.go`,
`execution.go`, `events.go`, `errors.go`, `value.go`).

## 2. What exists

- **Guest:** MicroPython v1.29.0 plus a small C adapter, compiled to a
  reactor-style WASI module. The adapter is C, compiled with Zig as the C
  toolchain. A Zig-language adapter is a possible later path, not current work.
- **Host:** a Go/wazero adapter in `internal/wasmguest` and a CLI in
  `cmd/quickpy-spike`. The host owns instantiation, tool dispatch, scheduling,
  cancellation, limits, and structured results.
- **Public SDK (M1 accepted, pre-1.0):** the root `quickpy` package wraps the
  low-level host and owns the public value, event, error, and lifecycle types.
  It is accepted as an M1 milestone; see [`M1_STATUS.md`](M1_STATUS.md).
- **Spec:** `docs/SPEC.md` defines the public Go contract, guest protocol, error
  taxonomy, limits, and milestones.
- **Tests:** synthetic host-adapter fixtures under
  `internal/wasmguest/testdata/`, public root unit tests, a separate real-guest
  smoke module under `guest/smoke/`, and the gated real-artifact public
  integration suite under `integration/` (see
  [`../integration/README.md`](../integration/README.md)).

The guest artifact is **not committed**. `guest/build/quickpy.wasm` is generated
and gitignored. A clean clone must run `./guest/build.sh` before using the real
guest.

## 3. Prerequisites (exact)

| Tool | Version / note |
|---|---|
| Go | **1.25.0 or newer** (current `go.mod` says `go 1.25.0`) |
| Zig | **0.17.1** (C toolchain and wasi-libc) |
| binaryen / `wasm-opt` | **133** |
| git, make, python3 | Required by `guest/build.sh` |
| Network | Required on first build to clone MicroPython v1.29.0 |
| Go module | `github.com/tetratelabs/wazero v1.12.0` |
| Go indirect | `golang.org/x/sys v0.44.0` |

The current `go.mod` module path is `github.com/keejkrej/quickpy`, matching the
remote. If that changes, update imports and this document together.

`guest/build.sh` warns (but does not fail) if Zig or `wasm-opt` differ from the
tested versions. Treat the pins as required for reproducible evidence.

## 4. Clean-clone build and verification

```sh
git clone https://github.com/keejkrej/quickpy.git
cd quickpy

# 1. Build the guest. Fetches the pinned MicroPython checkout into
#    guest/.cache/micropython and writes guest/build/quickpy.wasm.
./guest/build.sh

# 2. Root Go module.
go build ./...
go vet ./...
go test -count=1 ./...

# 3. Separate real-guest smoke module.
cd guest/smoke
go run . ../build/quickpy.wasm
cd ../..

# 4. Real-guest CLI evidence.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode all -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode async -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode async-cancel -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode stall -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode sleep0 -json
```

Expected artifact identity for the current M1 build:

- size: **236,119 bytes**
- sha256: `171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`
- ABI: **4** (packed 0.4)
- MicroPython: **1.29.0**
- protocol envelope: **0**
- memory: initial **64 pages / 4 MiB**, maximum **1024 pages / 64 MiB**
- guest heap: **4 MiB**
- WASI imports: **0**
- EH tags: **1**
- exports: unchanged (`memory`, `_initialize`, `qp_abi_version`, `qp_alloc`,
  `qp_free`, `qp_invoke`)

A clean-directory rebuild was byte-identical. The M0 artifact identity (195,053
bytes, sha256
`4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`) is
historical and stale for M1. If a rebuild produces a different hash, record the
new hash and treat reproducibility as an open item rather than silently
updating the number. See [`../guest/M1_NOTES.md`](../guest/M1_NOTES.md).

## 5. Evidence status (M0 historical, M1 accepted)

**M0:** independent verification returned **GO on all 11 M0 exit criteria**. M0
is an accepted feasibility baseline, not a production SDK. The M0 numbers below
are historical and stale for the M1 artifact.

**M1:** **full M1 accepted (independent verification, 2026-11-18).** All 12 M1
exit criteria in [`SPEC.md`](SPEC.md) §16 pass against the real artifact. The
public root package, persistence/state, events, lifecycle, structured
console/image output, tool conformance, and the real-artifact integration suite
form the accepted milestone. This is not production GA, a stable 1.0 API,
CPython compatibility, or pi-go integration. See [`M1_STATUS.md`](M1_STATUS.md)
for the accepted evidence, limits, and constraints.

Verified for M1:

- Artifact: **236,119 bytes**, sha256
  `171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`,
  ABI **4**, MicroPython **1.29.0**, protocol envelope **0**, **zero** WASI
  imports, **1** EH tag, **64/1024** memory pages, **4 MiB** guest heap; a
  clean-directory rebuild was byte-identical and exports were unchanged.
- **118/118** guest smoke checks pass; **78/78** instances closed. The smoke
  RSS observation of ~69–72 MiB after the harness fix is historical and
  run-specific, not a per-instance guarantee.
- **72** top-level integration tests plus **74** subtests (~15.4 s; `-race`
  110.2 s); **58** root tests; **126** internal tests total.
- Opt-in soak: **1060** executions in **27.84 s**, zero errors, goroutines
  **2/2**, RSS around **360 MiB** and stable on the reference machine. This is
  a process-level observation, not a per-instance memory guarantee; see
  [`../integration/SOAK.md`](../integration/SOAK.md).
- `gofmt`, `go vet`, `go build`, default tests, and default-race pass; repeat
  async/lifecycle runs passed **3×**.

Verified for M0 on the development machine:

- MicroPython v1.29.0 builds to a reactor WASI module with Zig 0.17.1 and
  wasm-opt 133.
- The artifact imports **zero** WASI functions and exports `memory`,
  `_initialize`, `qp_abi_version`, `qp_alloc`, `qp_free`, `qp_invoke`.
- Artifact: **195,053 bytes**, sha256
  `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`.
- **41 guest smoke checks** pass, covering real `print`, structured Python
  errors, `try/except`, fresh-instance isolation, async `gather` with
  out-of-order completion and partial resume, GC-pressure preservation of
  locals/`finally`, terminal `stalled`, nonterminal `awaiting_calls`,
  `sleep(0)` interleaving, positive-timer rejection, catchable unknown tool,
  memory/output limits, and no stdout leak.
- Root `go build`, `go vet`, and `go test` pass; the real CLI modes `all`,
  `async`, `async-cancel`, `stall`, and `sleep0` pass.
- Explicit cancellation with an unaffected overlapping execution: **3/3**.
- Host WASI import allowlist is **empty**; the guest imports zero WASI
  functions.
- Warm startup: **~0.193 ms mean** in the independent run.
- Suspended guest linear memory: **100 pages = 6,553,600 bytes per instance**
  (exact for the measured instances).
- Process RSS: approximately **11.5–11.8 MiB per instance** in the independent
  run. This is a process-level, approximate measurement; the spike CLI's own
  run reported ~13.1–13.6 MiB per instance.

For current M1 status, use [`M1_STATUS.md`](M1_STATUS.md) and
[`../guest/M1_NOTES.md`](../guest/M1_NOTES.md). Re-read them before quoting
status, but do not treat a stale “pending” line as a new blocker without
re-running the evidence.

## 6. Immediate next steps for a new agent (post-M1)

1. Confirm the M1 conformance PR is merged and `main` contains the accepted M1
   source (see §9).
2. Run the clean-clone build and verification commands in §4, including the
   gated integration suite and the opt-in soak, to confirm the artifact on the
   new machine.
3. Start **post-M1 corpus packaging** per
   [`AGENT_TARGET.md`](AGENT_TARGET.md):
   - define the acceptance corpus and the supported module facade;
   - build the separate Go agent application composing `pi-go/agent`, selected
     `pi-go/ai` provider code, and the QuickPy SDK;
   - keep **CodeMode** default-deny and **AgentScript** opt-in;
   - do not rewrite the engine or add unbounded wrappers.
4. Keep the M0/M1 evidence reproducible; do not regress the accepted baselines.
5. Stable artifact distribution/embedding remains future packaging work.
6. Only after the post-M1 corpus and M2 hardening should pi-go integration be
   treated as shipped.

## 7. Known hard requirements and limitations

- **WASM exception handling:** wazero must enable
  `experimental.CoreFeaturesExceptionHandling`. The artifact contains one
  required EH tag for MicroPython `nlr`. Without it, compilation fails.
- **Zero WASI imports (current build):** the guest imports no WASI functions.
  The host is default-deny. If a future build adds imports, each must be
  justified and allowlisted explicitly.
- **Forced cancellation while running:** while a guest entry is active,
  cancellation is host-led via wazero `CloseOnContextDone`. The host must not
  call another guest export or free buffers after the forced close, and must
  wait for the active entry to return before releasing the instance.
- **`quickpy.run` semantics:** submit-and-drive-to-quiescence. It must be the
  final top-level statement. There is no top-level `await` and no top-level
  `return`. Statements after `quickpy.run(...)` run before pending tools finish.
- **Serialized guest entry:** concurrent async tools are dispatched by the
  host, but only the owner re-enters the guest. No concurrent export entry.
- **No host capabilities:** no filesystem, network, environment, or process
  access. No OS process isolation; WASM is a memory/capability boundary.
- **Trusted tools:** tool implementations are trusted. A malicious trusted tool
  can do anything the host permits.
- **Narrow frozen modules:** `math`, `uos`, and `gc` are not available in the
  current guest build. `json`, `time` (for loud timer rejection), `sys`, and
  `io.StringIO` are available.
- **Memory cap scope:** `MaxMemoryBytes` is applied at the wazero runtime level,
  not per instance (wazero API limitation). A per-execution
  `ExecuteRequest.MaxMemoryBytes` must be zero (inherit) or exactly equal to the
  runtime value; a tighter value is rejected with `InvalidRequest`, not silently
  ignored. A configured cap hit during guest allocation is a guest-visible
  `MemoryError`/`ScriptError`, not `LimitExceeded`; see
  [ADR-0001](adr/0001-guest-allocation-failures.md).
- **Timers:** positive `asyncio.sleep` and `time.sleep` raise a catchable
  `UnsupportedError`. Only `asyncio.sleep(0)` is a supported yield.
- **Output timing:** output is observed when a guest invocation returns, not
  streamed during a long-running yield-free C/Python section.
- **Accepted constraints:** no host wall-clock timers; no top-level
  `await`/`return`; narrow MicroPython module surface; per-execution memory
  below the runtime cap is rejected; no OS process isolation; tool
  implementations are trusted.
- **Post-M1 work (not M1 conformance gates):** stable artifact
  distribution/embedding, the proposed [`AGENT_TARGET.md`](AGENT_TARGET.md)
  corpus/agent application, OS wrappers as future `AgentScript` default-deny
  work, and pi-go integration.
- **M1 memory numbers:** the current guest uses a 4 MiB heap and 4 MiB initial
  linear memory. See [`../guest/M1_NOTES.md`](../guest/M1_NOTES.md) and
  [`../integration/SOAK.md`](../integration/SOAK.md). Do not reuse M0
  startup/RSS measurements for M1.
- **Unstable protocol/API:** the guest protocol and Go API are pre-1.0 and may
  change.

## 8. Repository gotchas

- `guest/smoke` is a **separate Go module**. Root `go test ./...` does not run
  it. Use `cd guest/smoke && go run . ../build/quickpy.wasm`.
- `integration/` is gated by `QUICKPY_TEST_ARTIFACT`. Without it, every test
  skips; with it, use
  `QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -v ./integration`
  (add `-race` for the race run). See `integration/README.md`.
- `guest/build/` and `guest/.cache/` are gitignored. The MicroPython checkout is
  fetched at the pinned commit, not vendored.
- `internal/wasmguest/testdata/*.wasm` are **intentional synthetic fixtures**,
  not the real guest. Do not delete them or add a blanket `*.wasm` ignore.
- The real guest artifact is `guest/build/quickpy.wasm` (post-`wasm-opt
  --translate-to-exnref`), not the intermediate `guest/build/obj/quickpy.wasm`.
- The host must not instantiate `wasi_snapshot_preview1` for the current
  artifact.
- `guest/build.sh` requires network access on first run and builds a host
  `mpy-cross` binary as a manifest prerequisite.

## 9. Git workflow and handoff status

The earlier "unborn repository / pending workers" description is **historical
and no longer current**. The M0 handoff is complete:

- PR **#1** (`handoff/m0-baseline`) was **merged** on **Nov 17**.
- Merge commit: `11006181aa2452b5c9c7d7f3f6c186d08e7a1602`.
- `main` now contains the M0 source; the base is no longer unborn and the M0
  status-doc workers are no longer pending.

M1 history:

- PR **#2** (`m1/public-sdk`) delivered the public SDK + persistence increment
  and was merged into `main` at `459e9642055380e4d8e946d6c06418c91eed89ae`.
  That PR was an increment, not full M1 acceptance.

Current M1 conformance work:

- Branch: `m1/conformance`, based on `main` at `459e964` (the PR #2 merge).
- Independent verification returned **GO on all 12 M1 exit criteria** on
  **2026-11-18**: the real artifact rebuilt byte-identically, the default and
  race suites passed, the real-artifact integration suite passed, guest smoke
  passed 118/118 with 78/78 instances closed, and the opt-in soak passed. No
  blocking issues were reported.
- The increment is published as PR **#3**:
  <https://github.com/keejkrej/quickpy/pull/3> (base `main`, head
  `m1/conformance`). At the time of this amendment the PR is open and pending
  merge; use `gh pr view 3 --json state,mergeCommit` to resolve the final merge
  result.
- This is **full M1 acceptance as a milestone**, not production GA, a stable
  1.0 API, CPython compatibility, or full Prime compatibility.

Staging must use explicit paths, never `git add -A`. Exclude `guest/build/`,
`guest/.cache/`, logs, temp files, and local binaries. The synthetic
`internal/wasmguest/testdata/*.wasm` fixtures are intentional and must be
included.

## 10. Third-party and licensing status

No project license has been chosen. Original QuickPy code is not licensed by
the third-party notices file. Third-party components, provenance, and required
notices are recorded in [`../THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

Key points:

- MicroPython v1.29.0 is MIT licensed and is fetched at build time, not
  vendored.
- `guest/port/setjmp_rt.c` is adapted from wasi-libc's
  `libc-top-half/musl/src/setjmp/wasm32/rt.c`; wasi-libc is multi-licensed and
  the musl-derived portion is MIT. The musl `COPYRIGHT` notice is reproduced in
  `THIRD_PARTY_NOTICES.md` §2.
- The frozen `asyncio` modules are copied/adapted from MicroPython v1.29.0
  (MIT).
- Go dependencies: wazero (Apache-2.0) and `golang.org/x/sys` (BSD-3-Clause).

Before publishing a release, choose a project license and confirm the notices
file is complete.
