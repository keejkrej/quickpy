#!/usr/bin/env bash
# Build the QuickPy M0 guest spike.
#
# Prerequisites:
#   - zig 0.17.1 on PATH
#   - wasm-opt (binaryen) on PATH, version tested: 133
#   - git, make, python3
#
# The MicroPython source is cloned into guest/.cache/micropython at the exact
# pin below.  It is not vendored into the repository.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MPY_PIN="v1.29.0"
MPY_COMMIT="0fd6c573ea815774668bbb16b8e197c8822368b2"
MPY_DIR="${MPY_DIR:-$ROOT/.cache/micropython}"
BUILD_DIR="${BUILD_DIR:-$ROOT/build}"
OBJ_DIR="$BUILD_DIR/obj"
OUT="$BUILD_DIR/quickpy.wasm"

need() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "error: missing required tool: $1" >&2
        exit 1
    }
}

need git
need make
need python3
need zig
need wasm-opt

ZIG_VERSION="$(zig version)"
if [[ "$ZIG_VERSION" != "0.17.1" ]]; then
    echo "warning: tested with zig 0.17.1, found $ZIG_VERSION" >&2
fi

WASM_OPT_VERSION="$(wasm-opt --version 2>/dev/null || true)"
if [[ "$WASM_OPT_VERSION" != *"version 133"* ]]; then
    echo "warning: tested with binaryen/wasm-opt 133, found: $WASM_OPT_VERSION" >&2
fi

if [[ ! -d "$MPY_DIR/.git" ]]; then
    echo "==> cloning MicroPython $MPY_PIN into $MPY_DIR"
    mkdir -p "$(dirname "$MPY_DIR")"
    git clone --depth 1 --branch "$MPY_PIN" \
        https://github.com/micropython/micropython.git "$MPY_DIR"
fi

ACTUAL_COMMIT="$(git -C "$MPY_DIR" rev-parse HEAD)"
if [[ "$ACTUAL_COMMIT" != "$MPY_COMMIT" ]]; then
    echo "error: MicroPython checkout is $ACTUAL_COMMIT, expected $MPY_COMMIT" >&2
    echo "       remove $MPY_DIR and re-run, or set MPY_DIR to the pinned checkout" >&2
    exit 1
fi

if [[ ! -e "$MPY_DIR/lib/micropython-lib/README.md" ]]; then
    echo "==> initialising MicroPython lib/micropython-lib (manifest prerequisite)"
    git -C "$MPY_DIR" submodule update --init --depth 1 lib/micropython-lib
fi

if [[ ! -x "$MPY_DIR/mpy-cross/build/mpy-cross" ]]; then
    echo "==> building host mpy-cross (required by the frozen-module manifest)"
    make -C "$MPY_DIR/mpy-cross" -j"$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
fi

echo "==> building MicroPython objects"
make -C "$ROOT/port" \
    MPY_DIR="$MPY_DIR" \
    BUILD="$OBJ_DIR" \
    -j"$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"

echo "==> stripping DWARF and translating legacy EH to exnref"
mkdir -p "$BUILD_DIR"
wasm-opt --strip-dwarf "$OBJ_DIR/quickpy.wasm" -o "$BUILD_DIR/quickpy.nodwarf.wasm"
wasm-opt --enable-reference-types --enable-exception-handling \
    --translate-to-exnref "$BUILD_DIR/quickpy.nodwarf.wasm" -o "$OUT"

echo "==> artifact: $OUT"
ls -l "$OUT"

if command -v wasm-dis >/dev/null 2>&1; then
    wasm-dis "$OUT" -o "$BUILD_DIR/quickpy.wat"
    echo "==> imports: $(grep -c '(import' "$BUILD_DIR/quickpy.wat" || true)"
    echo "==> tag sections: $(grep -c '(tag ' "$BUILD_DIR/quickpy.wat" || true)"
    echo "==> exports:"
    grep '(export ' "$BUILD_DIR/quickpy.wat" | sed 's/^/    /'
fi
