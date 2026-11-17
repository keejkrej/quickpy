# Synthetic host-adapter fixtures

These `.wasm` files are **not** the QuickPy guest and are not evidence that
MicroPython-on-WASI works. They are tiny hand-written C fixtures used only to
exercise the Go/wazero host adapter in `internal/wasmguest`:

| File | Purpose |
|---|---|
| `completed.wasm` | ABI 0.1, returns a `completed` response with one text output. |
| `failed.wasm` | Returns a `failed` response with a structured `ValueError`. |
| `loop.wasm` | `qp_invoke` spins forever, for host-led timeout/cancel tests. |
| `badversion.wasm` | `qp_abi_version` returns 2, for ABI mismatch tests. |
| `malformed.wasm` | Returns truncated JSON, for protocol-error tests. |
| `import_path_open.wasm` | Imports `wasi_snapshot_preview1.path_open`, for allowlist tests. |
| `import_environ_get.wasm` | Imports `wasi_snapshot_preview1.environ_get`, for empty-allowlist tests. |
| `import_args_get.wasm` | Imports `wasi_snapshot_preview1.args_get`, for empty-allowlist tests. |
| `import_poll_oneoff.wasm` | Imports `wasi_snapshot_preview1.poll_oneoff`, for empty-allowlist tests. |
| `cancel_mixed.wasm` | Source-driven synthetic fixture: `loop` spins, `block` suspends with a blocking tool, otherwise completes. Used for explicit-cancel isolation tests. |
| `async_gather.wasm` | Suspends with calls `a` and `b`, then partial-resumes with only `a` awaiting, then completes. |
| `async_cancel.wasm` | Suspends with a blocking call, for cancellation-while-suspended tests. |
| `async_runnable.wasm` | Suspends with `reason:"runnable"`, then completes after a `resume`. |
| `async_duplicate.wasm` | Suspends with duplicate call IDs, for ledger rejection tests. |
| `async_drop.wasm` | Suspends with two calls, then drops one from `awaiting`, for ledger rejection tests. |
| `async_bad_runnable.wasm` | `runnable` suspension with non-empty `awaiting`, for protocol rejection tests. |
| `async_serial.wasm` | Expects `a` to be delivered before `b`, for `MaxConcurrentToolCalls=1` queueing tests. |
| `async_runnable_loop.wasm` | Two `runnable` suspensions, for `MaxResumes` tests. |
| `async_runnable_pending.wasm` | `runnable` suspension while a call is still pending, for protocol rejection tests. |
| `async_stalled.wasm` | Terminal `stalled:true` suspension with no calls, for Stalled classification tests. |
| `async_stalled_with_calls.wasm` | Contradictory `stalled:true` with calls/awaiting, for protocol rejection tests. |
| `async_stalled_false.wasm` | Contradictory `reason:"stalled"` with `stalled:false`, for protocol rejection tests. |
| `async_awaiting_stalled.wasm` | Contradictory `awaiting_calls` with `stalled:true`, for protocol rejection tests. |
| `async_runnable_stalled.wasm` | Contradictory `runnable` with `stalled:true`, for protocol rejection tests. |
| `async_stalled_pending.wasm` | `stalled:true` while a call is still pending, for protocol rejection tests. |
| `async_stalled_after_delivery.wasm` | `stalled:true` after a call was delivered, for terminal Stalled tests. |

They are compiled from `src/*.c` with Zig's freestanding WASM target. The
committed `.wasm` files are what the tests use; Zig is not required to run the
tests. To regenerate them:

```sh
cd internal/wasmguest/testdata/src
./generate.sh
```

The real integration smoke must use an explicit `-artifact` path (for example
`guest/build/quickpy.wasm`) and must not use these fixtures.
