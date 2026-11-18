# QuickPy

QuickPy is an experimental, embeddable Python execution runtime: a Go host
drives a fresh WebAssembly instance of MicroPython per execution, with a narrow
host-tool boundary, structured output, and explicit lifecycle and limits.

This repository contains an **M0-accepted feasibility baseline** and an
**in-progress M1 public SDK**. M1 is not accepted as a full release yet. It is
not a stable public SDK, not CPython, and not integrated with pi-go. See
[`docs/M1_STATUS.md`](docs/M1_STATUS.md) for the current M1 snapshot.

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
| `docs/SPEC.md` | Technical specification and milestone criteria. |
| `docs/HANDOFF.md` | Start-here guide for a new agent on a new machine. |
| `docs/M1_STATUS.md` | Current M1 increment, integration evidence, and open gates. |
| `guest/M0_NOTES.md` | Historical M0 guest build notes, pins, ABI, and evidence. |
| `guest/M1_NOTES.md` | M1 guest state slice, ABI 2, and artifact evidence. |
| `api.go`, `runtime.go`, `execution.go`, `events.go`, `errors.go`, `value.go` | Public `quickpy` SDK package. |
| `guest/build.sh` | Fetches the pinned MicroPython checkout and builds the artifact. |
| `guest/port/` | C adapter, MicroPython port config, frozen Python modules. |
| `guest/smoke/` | Separate Go module: real-guest smoke harness. |
| `integration/` | Gated real-artifact public SDK integration suite. |
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

# Root Go module: build, vet, and unit tests. The integration package skips
# cleanly here because QUICKPY_TEST_ARTIFACT is unset.
go build ./...
go vet ./...
go test -count=1 ./...

# Real-artifact public SDK integration suite (requires the built guest).
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -v ./integration
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -race ./integration

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

## Current status

**M0 is accepted and historical.** Independent verification returned **GO on
all 11 M0 exit criteria**. M0 is an accepted feasibility baseline, not a
production SDK.

**M1 is in progress and not accepted as a full release.** The public root
package, persistence/state, events, lifecycle, and the real-artifact integration
suite now form a coherent, integration-tested increment. The integration suite
is worker-reported passing (40 top-level tests plus 14 subtests, ~7.9 s; race
run ~40.3 s) and skips cleanly when `QUICKPY_TEST_ARTIFACT` is unset.
Independent verification is being started; until its report lands, treat these
as worker-reported results, not independent confirmation. See
[`docs/M1_STATUS.md`](docs/M1_STATUS.md) for the exact current pieces, open
gates, and deviations.

The M0 evidence below is retained as history. Its artifact size, hash, and
memory numbers are **stale** for the M1 artifact; use
[`guest/M1_NOTES.md`](guest/M1_NOTES.md) for current guest metrics.

Verified for M0 on the development machine:

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

M0 is accepted. The current milestone is **M1**: the public Go SDK surface,
persistence/state, events, and the conformance suites. Start with
[`docs/M1_STATUS.md`](docs/M1_STATUS.md) and
[`docs/HANDOFF.md`](docs/HANDOFF.md) §6.

## Public SDK (M1, pre-1.0)

The public entry point is the root `quickpy` package. Names are concrete now but
are **not stable before 1.0**; the semantics in `docs/SPEC.md` are the target.
There is no pi-go integration yet.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/keejkrej/quickpy"
)

func main() {
	rt, err := quickpy.New(quickpy.RuntimeOptions{
		Artifact:       quickpy.ArtifactSource{Path: "guest/build/quickpy.wasm"},
		MaxMemoryBytes: 64 << 20, // required, > 0
		MaxExecutions:  4,        // required, > 0
	})
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(context.Background())

	err = rt.RegisterTool("search", func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		// call.Args is a JSON-like quickpy.Value (nil, bool, int64, float64,
		// string, []Value, map[string]Value).
		return map[string]any{"query": call.Args, "hits": []any{}}, nil
	})
	if err != nil {
		log.Fatal(err)
	}

	exec, err := rt.Execute(context.Background(), quickpy.ExecuteRequest{
		Filename: "main.py",
		Source: `
import asyncio
import quickpy

async def main():
    result = await quickpy.tools.search(query="hello")
    print(result)

quickpy.run(main())
`,
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := exec.Wait(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Status, res.Outputs, res.StateWritten)
}
```

`Execute` validates and admits the request, then returns an `*Execution` handle;
the work runs in the background. `Wait(ctx)` waits for the terminal `Result` and
error. `Cancel()` requests cancellation. `Result` carries `Status`, ordered
`Outputs`, committed `State`/`StateWritten`, `ToolCalls`, `Duration`, and
`GuestInfo`. Events can also be streamed through `RuntimeOptions.EventSink` or a
per-execution `ExecuteRequest.EventSink`.

`MaxMemoryBytes` is the only enforced memory cap. A per-execution
`ExecuteRequest.MaxMemoryBytes` must be zero (inherit) or exactly equal to the
runtime value; a tighter value is rejected rather than silently ignored.
`MaxToolCalls == 0` means no cap. `DefaultTimeout == 0` means no default
timeout. The host requires ABI 2 and rejects 0.1 artifacts.

The public SDK + persistence increment is covered by the gated real-artifact
suite in [`integration/`](integration/); see
[`integration/README.md`](integration/README.md) for the exact commands and
coverage. `ImageEvent` is an exported type, but the real guest has no image
transport yet; it is documentation/API surface only for now.

## Known hard requirements and limitations

M0 acceptance is a feasibility verdict, not a production-readiness claim. M1 is
not accepted as a full release yet; the current increment is integration-tested
but still has open gates.

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
  Per-execution memory tightening is rejected, not silently ignored.
- Positive timers are unsupported and fail loudly.
- The guest protocol and Go API are not stable.
- `GuestInfo.Version` is currently blank; `ConsoleEvent`/`ImageEvent` are
  exposed but the real guest does not emit console or image output yet.
- An uncaught structured `ToolCause` is not populated yet.
- Event streaming occurs when a guest invocation returns, not during a
  long-running yield-free C/Python section.
- The integration suite covers the main public SDK and persistence scenarios,
  but not every request-specific cap with the real artifact: `MaxSourceBytes`,
  `MaxToolCalls`, `MaxPendingCalls`, `MaxResumes`, and tool-result caps are only
  partly covered by synthetic tests. Real protocol/guest-fault coverage is also
  synthetic.
- No host wall-clock timers and no top-level `await`/`return`.
- No stable artifact distribution/embedding contract yet.
- A complete normative `asyncio` subset/failure matrix and a dedicated
  soak/resource-leak trend are still needed.
- No pi-go integration.
- The current guest uses a 4 MiB heap and 4 MiB initial linear memory. M0
  startup/RSS measurements must not be reused blindly for M1; new M1 memory
  measurements are not yet recorded here.

## License status

No project license has been chosen yet. Original QuickPy code is therefore not
licensed by this repository's third-party notices. Third-party components and
their notices are recorded in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
