# QuickPy

QuickPy is an experimental, embeddable Python execution runtime: a Go host
drives a fresh WebAssembly instance of MicroPython per execution, with a narrow
host-tool boundary, structured output, and explicit lifecycle and limits.

This repository contains an **M0-accepted feasibility baseline**, a
**full M1-accepted public SDK + persistence milestone**, and an **accepted M2
scripting foundation** (independent verification, 2026-11-19). M1 and M2
acceptance are milestone verdicts, not production GA, a stable 1.0 API, CPython
compatibility, or pi-go integration. See [`docs/M1_STATUS.md`](docs/M1_STATUS.md)
for the accepted M1 evidence and constraints and
[`docs/M2_STATUS.md`](docs/M2_STATUS.md) for the accepted M2 scripting
foundation.

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
- Not the proposed compiled-agent target in
  [`docs/AGENT_TARGET.md`](docs/AGENT_TARGET.md); that is a post-M2 north star,
  not current scope. The user-created `script-agent` test field is the next
  consumer, and the eventual polyglot direction is broader than a single Python
  agent.

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
| `docs/M1_STATUS.md` | Accepted M1 evidence, limits, and constraints. |
| `docs/M2_STATUS.md` | Current M2 scripting-foundation state and open work. |
| `docs/AGENT_TARGET.md` | Proposed post-M2 compiled-agent north star; not current scope. |
| `guest/M0_NOTES.md` | Historical M0 guest build notes, pins, ABI, and evidence. |
| `guest/M1_NOTES.md` | M1 guest state/output slice, ABI 4, and artifact evidence. |
| `guest/SCRIPTING_NOTES.md` | Scripting stdlib baseline and the optional `quickpy.fs` facade. |
| `capability/` | Runtime-neutral filesystem capability core (v1) and `quickpybridge`. |
| `examples/embed/` | `go:embed` reference: one native binary, no writable TMPDIR. |
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

**M1 is accepted (independent verification, 2026-11-18).** All **12 M1 exit
criteria** in [`docs/SPEC.md`](docs/SPEC.md) §16 pass against the real artifact.
The public root package, persistence/state, events, lifecycle, structured
console/image output, tool conformance, and the real-artifact integration suite
form the accepted milestone. This is **not** production GA, a stable 1.0 API,
CPython compatibility, or pi-go integration.

Verified for M1:

- Artifact: **236,119 bytes**, sha256
  `171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`,
  ABI **4**, MicroPython **1.29.0**, protocol envelope **0**, **zero** WASI
  imports, **1** EH tag, **64/1024** memory pages, **4 MiB** guest heap; a
  clean-directory rebuild was byte-identical.
- **118/118** guest smoke checks pass; **78/78** instances closed.
- **72** top-level integration tests plus **74** subtests (~15.4 s; `-race`
  110.2 s); **58** root tests; **126** internal tests total.
- Opt-in soak: **1060** executions in **27.84 s**, zero errors, goroutines
  **2/2**, RSS around **360 MiB** and stable on the reference machine. This is
  a process-level observation, not a per-instance memory guarantee.
- `gofmt`, `go vet`, `go build`, default tests, and default-race pass; repeat
  async/lifecycle runs passed **3×**.

**M2 scripting foundation is accepted (independent verification, 2026-11-19).**
The revision adds a scripting stdlib baseline (`json`, `re`, `math`,
`io.StringIO`, `binascii`), a runtime-neutral filesystem capability core, and an
opt-in `quickpy.fs` facade. Its artifact is **256,486 bytes**, SHA-256
`5fdd351a70e0f65a4b812ad21c34a0885f87e839aee7721164768907ad61f50a`, still ABI
4 with zero WASI imports. Guest smoke passes **135/135** checks with **94/94**
instances closed; the real-artifact workspace tests pass, and the original
confidentiality NO-GO was resolved. This is an accepted **scripting foundation**,
not full M2, a polyglot agent, a general OS sandbox, or a JavaScript facade
inside QuickPy. See [`docs/M2_STATUS.md`](docs/M2_STATUS.md).

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

M0, M1, and the M2 scripting foundation are accepted. The next direction is the
`script-agent` test field and polyglot composition against the runtime-neutral
capability contract, with a new Zig adapter optional. The proposed
compiled-agent target in [`docs/AGENT_TARGET.md`](docs/AGENT_TARGET.md) remains
a future consumer, not current scope. Start with
[`docs/M2_STATUS.md`](docs/M2_STATUS.md) and
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
timeout. The host requires ABI 4 and rejects older artifacts.

The accepted M1 public SDK + persistence increment is covered by the gated
real-artifact suite in [`integration/`](integration/); see
[`integration/README.md`](integration/README.md) for the exact commands and
coverage. `ConsoleEvent` and `ImageEvent` are real ABI 4 output channels; the
guest emits ordered text, console, and image events.

## Scripting foundation and opt-in filesystem (M2, accepted)

The M2 scripting foundation keeps the default posture unchanged: no ambient
`os`, `open`, filesystem, network, environment, or process access. It adds two
separate pieces:

- **Scripting stdlib baseline** — `json`, `re`, `math`, `io.StringIO`, and
  `binascii` are compiled into the guest. They are pure computation and add no
  WASI import. See [`guest/SCRIPTING_NOTES.md`](guest/SCRIPTING_NOTES.md) for
  the exact surface and CPython differences.
- **Runtime-neutral filesystem capability** — [`capability/`](capability/) is a
  Go-standard-library-only core with eight versioned operations over one
  host-chosen `os.Root`. It is independent of QuickPy and of any Python object
  model. See [`capability/README.md`](capability/README.md) for the v1
  operation/error contract.

QuickPy consumes that core explicitly through
[`capability/quickpybridge`](capability/quickpybridge): `Install` registers the
eight `fs_*` tools on a runtime, using the same runtime tool snapshot as any
other tool. There is no automatic I/O. A writable workspace is explicit, and a
read-only workspace is also explicit (`ReadOnly: true`). The guest
`quickpy.fs` facade is present but inert until those tools are registered; with
no registration every call is a catchable `ToolNotFoundError`.

A minimal enablement looks like this (the full runnable example is
[`examples/embed/`](examples/embed/)):

```go
a, err := capability.NewWorkspace(capability.WorkspaceOptions{
	Root:     "/path/to/workspace", // host-chosen; never guest-chosen
	ReadOnly: false,                // set true for untrusted callers
})
if err != nil {
	log.Fatal(err)
}
defer a.Close()

rt, err := quickpy.New(quickpy.RuntimeOptions{ /* ... */ })
if err != nil {
	log.Fatal(err)
}
if err := quickpybridge.Install(rt, a); err != nil {
	log.Fatal(err)
}
// Now a script may `await quickpy.fs.read_text("notes.txt")`.
```

The filesystem surface is deliberately limited: no full `open`/`pathlib`/
`tempfile`/`subprocess` API, no glob or rich pattern matching, no recursive
remove/copy, no append modes, and no `fsync` (writes are atomic for readers but
not crash-durable). Hardlinks, bind mounts, devices, and FIFOs are host
responsibilities; see the known limitations in
[`capability/README.md`](capability/README.md). This is not a claim of full
secure OS isolation.

The native embedding reference in [`examples/embed/`](examples/embed/) shows
the artifact embedded with `go:embed` and passed as `ArtifactSource.Bytes`, so
the binary needs no artifact file and no writable `TMPDIR`. The build script
uses a temporary output directory, but the resulting binary itself is
self-contained and runs from an unrelated directory with a nonexistent
`TMPDIR`. The generated `quickpy.wasm` asset stays gitignored; the example is a
build recipe, not an artifact distribution contract. A stripped darwin/arm64
build measured 6,507,490 bytes with an earlier 251,590-byte artifact; that is a
historical, run-specific observation, not a current size guarantee.

## Known hard requirements and limitations

M0, M1, and the M2 scripting foundation are accepted milestones, not
production-readiness claims. The following are accepted MicroPython/runtime
constraints and post-M2 work, not open conformance gates.

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
- No ambient host filesystem, network, or environment access. No OS process
  isolation. An opt-in filesystem capability exists (see the M2 section above),
  but it is explicit, workspace-scoped, and not ambient `os`/`open` access.
- The M2 scripting foundation is not full M2, a polyglot agent, a general OS
  sandbox, or a JavaScript facade inside QuickPy. Hardlinks, bind mounts,
  devices, and FIFOs remain host responsibilities; there is no `fsync` or
  crash durability; CAPv1 is an in-process data ABI, not a network protocol.
  The `script-agent` test field is the next consumer for new language facades.
- Tool implementations are trusted; a malicious trusted tool can do anything
  the host permits.
- Frozen-module coverage is narrow: `uos` and `gc` are not available in the
  current guest build. `json`, `re`, `math`, `io.StringIO`, and `binascii` are
  available; see [`guest/SCRIPTING_NOTES.md`](guest/SCRIPTING_NOTES.md).
- `MaxMemoryBytes` is applied at the wazero runtime level, not per instance.
  Per-execution memory tightening is rejected, not silently ignored. A
  configured cap hit during guest allocation is a guest-visible
  `MemoryError`/`ScriptError`, not `LimitExceeded`; see
  [`docs/adr/0001-guest-allocation-failures.md`](docs/adr/0001-guest-allocation-failures.md).
- Positive timers are unsupported and fail loudly.
- Output is observed when a guest invocation returns, not streamed during a
  long-running yield-free C/Python section.
- No stable artifact distribution/embedding contract yet; the `go:embed`
  example is a build recipe, not a distribution guarantee. That remains future
  packaging work.
- No pi-go integration and no Zig rewrite.
- Not CPython or pip-compatible; it is a MicroPython subset.
- Not production GA and not a stable 1.0 API; the guest protocol and Go API are
  pre-1.0 and may change.
- The proposed post-M2 compiled-agent direction is
  [`docs/AGENT_TARGET.md`](docs/AGENT_TARGET.md). OS wrappers are future
  `AgentScript` work and remain default-deny.
- The current guest uses a 4 MiB heap and 4 MiB initial linear memory. M0
  startup/RSS measurements must not be reused blindly for M1; see
  [`guest/M1_NOTES.md`](guest/M1_NOTES.md) and
  [`integration/SOAK.md`](integration/SOAK.md) for current evidence.

## License status

No project license has been chosen yet. Original QuickPy code is therefore not
licensed by this repository's third-party notices. Third-party components and
their notices are recorded in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
