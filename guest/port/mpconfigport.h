// QuickPy M0 guest spike configuration.
//
// This is deliberately a *minimal* MicroPython configuration: compiler + GC +
// core builtins, no filesystem, no network, no environment, no REPL, no frozen
// modules.  It is not a production configuration.

#include <stdint.h>

#define MICROPY_CONFIG_ROM_LEVEL (MICROPY_CONFIG_ROM_LEVEL_MINIMUM)

// We need the compiler: the host sends Python source, not .mpy bytecode.
#define MICROPY_ENABLE_COMPILER (1)
#define MICROPY_ENABLE_GC (1)

// WebAssembly has no native NLR implementation.  Use the setjmp-based NLR and
// provide the wasm setjmp runtime in setjmp_rt.c.
#define MICROPY_NLR_SETJMP (1)

// Keep tracebacks useful for the spike.
#define MICROPY_ENABLE_SOURCE_LINE (1)

// Raise RecursionError before the wasm stack overflows.  The limit is set in
// adapter.c and is smaller than the linker's 1 MiB wasm stack.
#define MICROPY_STACK_CHECK (1)

// No ambient capabilities.
// Needed for the frozen-module import path.  mp_import_stat() still returns
// NO_EXIST, so there is no filesystem import capability.
#define MICROPY_ENABLE_EXTERNAL_IMPORT (1)
#define MICROPY_HELPER_REPL (0)
#define MICROPY_PY_SYS_MODULES (0)
#define MICROPY_PY_SYS_EXIT (0)
// sys.path is needed so the import system searches the ".frozen" entry.
// mp_import_stat() still returns NO_EXIST, so no filesystem import is possible.
#define MICROPY_PY_SYS_PATH (1)
#define MICROPY_PY_SYS_ATTR_DELEGATION (1)
#define MICROPY_PY_SYS_ARGV (0)
#define MICROPY_PY_IO (1)
#define MICROPY_PY_OS (0)
#define MICROPY_PY_THREAD (0)

// quickpy.image() accepts bytes or bytearray.  The minimum ROM level omits
// bytearray, so enable just that builtin; no array/memoryview surface is added.
#define MICROPY_PY_BUILTINS_BYTEARRAY (1)

// M0 async experiment: stock _asyncio Task/TaskQueue, frozen asyncio package,
// built-in json for tool argument/result values, and time only so that
// time.sleep can be patched to raise UnsupportedError.
#define MICROPY_PY_ASYNCIO (1)
#define MICROPY_PY_ASYNC_AWAIT (1)
#define MICROPY_PY_JSON (1)
#define MICROPY_PY_TIME (1)
#define MICROPY_PY_TIME_GMTIME_LOCALTIME_MKTIME (0)
#define MICROPY_PY_TIME_TIME_TIME_NS (0)

// Safe language modules for agent scripting.  These are pure computation and
// in-memory data handling: none of them add WASI imports or ambient
// capabilities (no filesystem, network, environment, process, or clock).
//
// re: built-in C engine (lib/re1.5), no external library.  sub() accepts a
// callable replacement; groups()/span()/start()/end() are enabled because the
// default ROM level would otherwise omit them.
#define MICROPY_PY_RE (1)
#define MICROPY_PY_RE_SUB (1)
#define MICROPY_PY_RE_MATCH_GROUPS (1)
#define MICROPY_PY_RE_MATCH_SPAN_START_END (1)
// math: double-precision libm from wasi-libc.  pi/e are always present; the
// extra constants (tau/inf/nan) and special functions stay off.
#define MICROPY_PY_MATH (1)
// binascii: base64 and hex helpers for upcoming filesystem wrappers.  CRC32
// needs uzlib and is not part of this baseline.
#define MICROPY_PY_BINASCII (1)
#define MICROPY_PY_BINASCII_CRC32 (0)
// bytes.hex()/bytes.fromhex() back binascii.hexlify()/unhexlify().
#define MICROPY_PY_BUILTINS_BYTES_HEX (1)
// Unicode-aware str: len/index/slice count code points and \uXXXX escapes
// (including json.loads) produce valid UTF-8.  Without this the minimum ROM
// level stores \u escapes as single invalid bytes.
#define MICROPY_PY_BUILTINS_STR_UNICODE (1)
// io.StringIO for in-memory text.  open() is still overridden to raise OSError
// in adapter.c, so this does not add filesystem access.
#define MICROPY_PY_IO_IOBASE (1)

// M1 state and tool values are IEEE float64 on the wire.  Use double
// precision so values that single precision cannot distinguish survive the
// guest round trip.
#define MICROPY_FLOAT_IMPL (MICROPY_FLOAT_IMPL_DOUBLE)

// M1 state values are int64 on the wire.  wasm32 small ints are 31-bit, so
// enable the 64-bit long-int representation for values outside that range.
#define MICROPY_LONGINT_IMPL (MICROPY_LONGINT_IMPL_LONGLONG)

#define MICROPY_ALLOC_PATH_MAX (256)
#define MICROPY_ALLOC_PARSE_CHUNK_INIT (16)

// Heap is allocated from wasi-libc malloc on first qp_invoke.  Each WASM
// instance gets its own copy because linear memory is per-instance.  M1
// raises this from 1 MiB to 4 MiB so a 1 MiB state value plus its validated
// snapshot fit without turning ordinary state writes into MemoryError.
#define MICROPY_HEAP_SIZE (4 * 1024 * 1024)

typedef long mp_off_t;

// Zig's wasi-libc does not ship <alloca.h>; clang provides the builtin.
#ifndef alloca
#define alloca __builtin_alloca
#endif

#define MICROPY_HW_BOARD_NAME "quickpy-m0"
#define MICROPY_HW_MCU_NAME "wasm32-wasi"

#define MP_STATE_PORT MP_STATE_VM
