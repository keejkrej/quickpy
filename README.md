# QuickPy

QuickPy is an experimental, embeddable Python execution runtime: a Go host
drives a fresh WebAssembly instance of MicroPython per execution, with a narrow
host-tool boundary, structured output, and explicit lifecycle and limits.

This repository is an **M0-accepted feasibility baseline plus a proposed
specification**. It is not a stable public SDK, not CPython, and not integrated
with pi-go.

## What this is

- A fun/learning systems project exploring an embeddable Python runtime.
- MicroPython's parser, bytecode VM, and GC, borrowed as the guest engine.
- A temporary **C guest adapter** compiled to WASI with Zig.
- A **Go/wazero host** that owns instantiation, tool dispatch, scheduling,
  cancellation, and limits.
- A written contract in [`docs/SPEC.md`](docs/SPEC.md) for the intended public
  API and guest protocol.

## What this is not

- Not CPython or pip-compatible; it is a MicroPython subset.
- Not a finished or stable public SDK; names and protocol shapes may change.
- Not pi-go integration; that is a later milestone.
- Not a Zig-language adapter yet. Zig is used as the C toolchain; a Zig core is
  an optional later path.
- Not OS process isolation. WASM is a guest memory/capability boundary, not a
  process sandbox.

## Architecture at a glance

```
Go application
  └─ Go host (wazero)            cmd/quickpy-spike, internal/wasmguest
       └─ WASM instance          guest/build/quickpy.wasm (generated, ignored)
            └─ C adapter         guest/port/adapter.c
                 └─ MicroPython  v1.29.0, pinned and fetched at build time
```

The guest exposes a small transient ABI (`qp_abi_version`, `qp_alloc`,
`qp_free`, `qp_invoke`) and currently imports **zero** WASI functions. The host
must enable wazero's experimental WebAssembly exception-handling feature because
MicroPython's non-local-return implementation uses one EH tag.

## Repository layout

| Path | Purpose |
|---|---|
| `docs/SPEC.md` | Proposed technical specification and milestone criteria. |
| `docs/HANDOFF.md` | Start-here guide for a new agent on a new machine. |
| `guest/M0_NOTES.md` | Guest build notes, pins, ABI, and evidence. |
| `guest/build.sh` | Fetches the pinned MicroPython checkout and builds the artifact. |
| `guest/port/` | C adapter, MicroPython port config, frozen Python modules. |
| `guest/smoke/` | Separate Go module: real-guest smoke harness. |
| `cmd/quickpy-spike/` | Go/wazero CLI used for M0 evidence. |
| `internal/wasmguest/` | Host adapter package and synthetic-fixture tests. |

## Prerequisites

- **Go 1.25.0 or newer** (see `go.mod`).
- **Zig 0.17.1** (provides the C toolchain and wasi-libc).
- **binaryen / `wasm-opt` 133**.
- `git`, `make`, `python3`.
- Network access on first build to clone the pinned MicroPython v1.29.0
  checkout into `guest/.cache/micropython` (not vendored).
- Go dependency: `github.com/tetratelabs/wazero v1.12.0`, with
  `experimental.CoreFeaturesExceptionHandling` enabled by the host.

## Build and verify from a clean clone

```sh
git clone https://github.com/keejkrej/quickpy.git
cd quickpy

# Build the guest artifact. This writes guest/build/quickpy.wasm, which is
# intentionally gitignored and must be regenerated locally.
./guest/build.sh

# Root Go module: build, vet, and unit tests (synthetic fixtures only).
go build ./...
go vet ./...
go test -count=1 ./...

# Separate Go module: real-guest smoke harness.
cd guest/smoke
go run . ../build/quickpy.wasm
cd ../..

# Real-guest CLI evidence modes.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode all -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode async -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode async-cancel -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode stall -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode sleep0 -json
```

`guest/smoke` is a **separate Go module**; root `go test ./...` does not cover
it. The synthetic `.wasm` files under `internal/wasmguest/testdata/` are
intentional committed fixtures, not the real guest.

## Current status (M0 accepted)

Independent verification returned **GO on all 11 M0 exit criteria**. M0 is an
accepted feasibility baseline, not a production SDK.

Verified on the development machine:

- MicroPython v1.29.0 builds to a reactor WASI module with Zig 0.17.1 and
  wasm-opt 133.
- Artifact: **195,053 bytes**, sha256
  `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`.
- **41 guest smoke checks** pass, including real `print`, structured Python
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

M0 is accepted. The next milestone is **M1**: the public Go SDK surface,
persistence/state, events, and the conformance suites. See
[`docs/HANDOFF.md`](docs/HANDOFF.md) §6 for the start-at-M1 roadmap.

`docs/SPEC.md` and `guest/M0_NOTES.md` are owned by separate status-doc workers
and may lag this README until their final updates land.

## Known hard requirements and limitations

M0 acceptance is a feasibility verdict, not a production-readiness claim.

- wazero must enable `experimental.CoreFeaturesExceptionHandling`; the artifact
  contains one required EH tag.
- The guest currently imports zero WASI functions; the host is default-deny.
- While a guest entry is running, cancellation is host-led forced termination
  via wazero `CloseOnContextDone`; the host does not call another guest export
  or free buffers after the forced close.
- `quickpy.run(main())` is submit-and-drive-to-quiescence and must be the final
  top-level statement. There is no top-level `await` or top-level `return`.
- Concurrent async tools are dispatched by the host, but only the owner
  re-enters the guest; guest entry is serialized.
- No host filesystem, network, or environment access. No OS process isolation.
- Tool implementations are trusted; a malicious trusted tool can do anything
  the host permits.
- Frozen-module coverage is narrow (`math`, `uos`, and `gc` are not available
  in the current guest build).
- `MaxMemoryBytes` is applied at the wazero runtime level, not per instance.
- Positive timers are unsupported and fail loudly.
- The guest protocol and Go API are not stable.

## License status

No project license has been chosen yet. Original QuickPy code is therefore not
licensed by this repository's third-party notices. Third-party components and
their notices are recorded in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
