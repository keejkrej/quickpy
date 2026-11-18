# Real-artifact integration tests

These tests exercise the public QuickPy Go SDK (`github.com/keejkrej/quickpy`)
against the real MicroPython guest artifact. They are intentionally gated so a
plain `go test ./...` on a clean checkout never needs `guest/build/quickpy.wasm`
or the Zig/MicroPython build toolchain.

## Gate

`QUICKPY_TEST_ARTIFACT` selects the artifact:

- **unset** — every test calls `t.Skip`; the package is a no-op for the default
  test run.
- **set** — the value is a path to a real ABI 2 MicroPython artifact. Relative
  paths are resolved from the repository root, so `guest/build/quickpy.wasm`
  works from any working directory.

The current verified artifact is 220,134 bytes with SHA-256
`be1b8e48b6884e3e9e2a59c758a2abceac564adb5162b691e5a6b6cd3eac9c4d`. The tests
log the observed size and hash and require ABI 2 with zero WASI imports; they do
not hard-code the hash so a rebuilt artifact can still be tested.

## Commands

```sh
# Default run: integration tests skip cleanly.
go test ./...

# Real artifact, verbose.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -v ./integration

# Real artifact under the race detector.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -race ./integration
```

## What is covered

- Explicit state: initial int/float identity, get/set, deep copy, last write,
  `set_state(None)` vs no write, nested JSON, and no state commit on script
  error, stall, cancel, timeout, or final oversize.
- Ordered `TextEvent`/`ToolEvent` delivery through both the request sink and
  `Result.Outputs`, including controlled out-of-order `asyncio.gather`
  completion, opaque call IDs, unknown tools, handler arguments, and deadlines.
- Deterministic output truncation across guest output deltas, with the retained
  prefix matching the sink and never exceeding `MaxOutputBytes`.
- Repeated/concurrent `Wait`, wait-local `ErrWaitTimeout` without cancellation,
  idempotent `Cancel`, parent-context cancellation, and no state on cancel.
- Registry snapshot stability, deterministic `ToolNames`, tool retention across
  fresh guest globals, 100 sequential executions, and 10 concurrent distinct
  executions.
- `Runtime.Close` with running and suspended executions, concurrent/idempotent
  closers, short-context early return, and a noncooperative tool that `Close`
  must not wait for.
- Capacity, per-execution memory rejection before admission, empty guest
  imports, unavailable `os`/`uos`/`gc`/`math` modules, and no guest stdout leak
  to the host process.
