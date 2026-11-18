#!/usr/bin/env bash
# Prepare the ignored example asset, build the embedded-artifact example, and
# prove the resulting binary runs from an unrelated directory with a
# nonexistent TMPDIR.
#
# Usage:
#   QUICKPY_WASM=guest/build/quickpy.wasm ./examples/embed/build.sh
#
# The guest artifact is copied to examples/embed/quickpy.wasm, which is
# git-ignored. The binary is written to a temporary directory outside the
# repository. No generated artifact is ever tracked.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
WASM="${QUICKPY_WASM:-$ROOT/guest/build/quickpy.wasm}"
ASSET="$HERE/quickpy.wasm"

if [[ ! -f "$WASM" ]]; then
    echo "error: artifact not found: $WASM" >&2
    echo "       set QUICKPY_WASM to a built guest artifact (for example guest/build/quickpy.wasm)" >&2
    exit 1
fi

sha256() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        sha256sum "$1" | awk '{print $1}'
    fi
}

cp "$WASM" "$ASSET"
want="$(sha256 "$ASSET")"
echo "prepared $ASSET"
echo "artifact sha256=$want size=$(wc -c <"$ASSET" | tr -d ' ')"

OUTDIR="$(mktemp -d "${TMPDIR:-/tmp}/quickpy-embed.XXXXXX")"
OUT="$OUTDIR/embed-example"
echo "building $OUT"
CGO_ENABLED=0 go build -tags quickpy_embed_example -trimpath -ldflags '-s -w' -o "$OUT" "$HERE"
echo "binary size=$(wc -c <"$OUT" | tr -d ' ') bytes"

RUNDIR="$(mktemp -d "${TMPDIR:-/tmp}/quickpy-embed-run.XXXXXX")"
echo "running from $RUNDIR with TMPDIR=$RUNDIR/no-such-tmp"
output="$(cd "$RUNDIR" && TMPDIR="$RUNDIR/no-such-tmp" "$OUT")"
printf '%s\n' "$output"
if ! grep -q "sha256=$want" <<<"$output"; then
    echo "error: binary did not report the prepared artifact sha256" >&2
    exit 1
fi
if ! grep -q "abi=4" <<<"$output"; then
    echo "error: binary did not report ABI 4" >&2
    exit 1
fi
if ! grep -q "status=completed" <<<"$output"; then
    echo "error: binary did not report a completed execution" >&2
    exit 1
fi
if ! grep -q "generated total: 91" <<<"$output"; then
    echo "error: binary did not run the generated Python source" >&2
    exit 1
fi
if ! grep -q "engine=micropython" <<<"$output"; then
    echo "error: binary did not report the MicroPython guest identity" >&2
    exit 1
fi
echo "ok: embedded artifact identity verified"
