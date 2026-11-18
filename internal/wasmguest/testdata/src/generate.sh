#!/bin/sh
# Regenerate the synthetic host-adapter fixtures. Requires Zig on PATH.
# These are not the QuickPy guest; see ../README.md.
set -eu
cd "$(dirname "$0")"

for f in completed completed_with_error failed loop badversion oldversion malformed import_path_open import_environ_get import_args_get import_poll_oneoff cancel_mixed async_gather async_cancel async_runnable async_duplicate async_drop async_bad_runnable async_serial async_runnable_loop async_runnable_pending async_stalled async_stalled_with_calls async_stalled_false async_awaiting_stalled async_runnable_stalled async_stalled_pending async_stalled_after_delivery async_pending_reuse output_deltas state_value state_null state_false state_missing state_out_of_range state_oversize state_on_failed state_on_suspended; do
  zig cc -target wasm32-freestanding -nostdlib -Oz -fno-sanitize=all \
    -Wl,--no-entry \
    -Wl,--export=qp_abi_version \
    -Wl,--export=qp_alloc \
    -Wl,--export=qp_free \
    -Wl,--export=qp_invoke \
    -Wl,--export-memory \
    -Wl,--strip-all \
    -o "../$f.wasm" "$f.c"
done
