#!/usr/bin/env bash
# Preprocessor wrapper for the QuickPy M0 guest spike.
#
# `zig cc -E` loses its wasi-libc include search paths when given more than one
# input file, and MicroPython's qstr collector batches sources.  This wrapper
# runs the preprocessor once per input and concatenates the output.
#
# Usage: cpp.sh <flags...> <file...>
# A trailing "-" means read from stdin.

set -euo pipefail

flags=()
files=()
for arg in "$@"; do
    if [[ "$arg" == "-" ]]; then
        files+=("$arg")
    elif [[ "$arg" == -* ]]; then
        flags+=("$arg")
    else
        files+=("$arg")
    fi
done

if [[ ${#files[@]} -eq 0 ]]; then
    files=("-")
fi

for f in "${files[@]}"; do
    if [[ "$f" == "-" ]]; then
        zig cc -E "${flags[@]}" -x c -
    else
        zig cc -E "${flags[@]}" "$f"
    fi
done
