# QuickPy guest scripting stdlib baseline

Status: implemented in the guest artifact. ABI stays **4**; no host protocol
changed. This note covers only `guest/**`. Host/integration adjustments are
owned elsewhere.

## Purpose

Give agent Python-c-style scripts a small, safe standard-library surface
comparable to basic QuickJS built-ins, without adding ambient OS capabilities.
Every module below is pure computation or in-memory data handling. None adds a
WASI import, filesystem, network, environment, process, clock, or random
source.

`quickpy.fs` is the one exception in shape, not in ambient authority: it is an
optional async facade over host tools. It has no effect unless the application
explicitly registers those tools and supplies a workspace/policy. The guest
never chooses a root, resolves `..`, or grants permissions.

## Modules available

All are compiled into the artifact and importable with `import <name>`.

### `json`

- `dumps(obj)`, `loads(str_or_bytes)`, `dump(obj, stream)`, `load(stream)`
- Nested dict/list/tuple, `str`, `int`, `float`, `bool`, `None`.
- `int`/`float` are distinct: `loads("1")` is `int`, `loads("1.0")` is `float`;
  `dumps(1)` is `1`, `dumps(1.0)` is `1.0`.
- Unicode: non-ASCII printable characters are emitted as raw UTF-8; `\uXXXX`
  escapes in input decode to valid UTF-8 (requires the Unicode `str` config
  below).
- `dump`/`load` need a stream object; `io.StringIO` works.

### `re`

Built-in C engine (`lib/re1.5`), no external library.

- Module: `compile`, `match`, `search`, `sub`
- Compiled pattern: `match`, `search`, `split`, `sub`
- Match object: `group(n)`, `groups()`, `span([n])`, `start([n])`, `end([n])`
- `sub` accepts a string or a callable replacement; the callable receives the
  match object.
- Invalid patterns raise `ValueError("error in regex")`; uncaught it is a clean
  `Script` failure with type `ValueError`.

### `math`

Double precision (`MICROPY_FLOAT_IMPL_DOUBLE`), wasi-libc libm.

- Constants: `pi`, `e`
- Functions: `sqrt`, `pow`, `exp`, `log`, `cos`, `sin`, `tan`, `acos`, `asin`,
  `atan`, `atan2`, `ceil`, `copysign`, `fabs`, `floor`, `fmod`, `frexp`,
  `ldexp`, `modf`, `isfinite`, `isinf`, `isnan`, `trunc`, `radians`, `degrees`
- Not enabled: `factorial`, `isclose`, `tau`, `inf`, `nan`, special functions
  (`erf`, `gamma`, ...). Use `float("inf")`/`float("nan")` if needed.

### `io`

- `StringIO` for in-memory text (`write`, `read`, `getvalue`, ...)
- `BytesIO`, `IOBase` are also present from the same object file.
- `open()` is still overridden in `adapter.c` to raise
  `OSError("open is not supported")`. No filesystem access is added.

### `binascii`

- `hexlify`, `unhexlify`, `a2b_base64`, `b2a_base64`
- `b2a_base64` appends a trailing newline (same as CPython).
- `crc32` is **not** enabled (it would pull in uzlib).

### `quickpy.fs` (optional host capability)

`quickpy.fs` is an async facade over eight host tools. It is present in the
artifact but inert by default: if the host did not register a tool, the call
raises `ToolNotFoundError` with code `tool_not_found`. The application must
construct the capability adapter with its own workspace and permissions and
install the tools before `Execute` (the Go bridge is
`capability/quickpybridge`).

Exact methods and return values:

| Method | Host tool | Returns |
| --- | --- | --- |
| `await quickpy.fs.read_text(path, encoding="utf-8")` | `fs_read_text` | `str` |
| `await quickpy.fs.write_text(path, text)` | `fs_write_text` | `int` bytes written |
| `await quickpy.fs.read_bytes(path)` | `fs_read_bytes` | `bytes` |
| `await quickpy.fs.write_bytes(path, data)` | `fs_write_bytes` | `int` bytes written |
| `await quickpy.fs.exists(path)` | `fs_exists` | `bool` |
| `await quickpy.fs.listdir(path=".")` | `fs_listdir` | `list` of `{"name": str, "type": str, "size": int}` |
| `await quickpy.fs.mkdir(path, parents=False)` | `fs_mkdir` | `bool` created |
| `await quickpy.fs.remove(path)` | `fs_remove` | `bool` removed |

- `path` must be a `str`; `text` must be a `str`; `data` must be `bytes` or
  `bytearray`; `parents` must be a `bool`; `encoding` must be a `str`. Local
  type failures raise `TypeError`. Unexpected keyword arguments raise
  `ToolArgumentError` (code `tool_argument`), matching the tool facade.
- `read_bytes` decodes the host's base64 string with
  `binascii.a2b_base64`. `write_bytes` encodes with `binascii.b2a_base64` and
  strips the trailing newline that `b2a_base64` appends, because the host
  decoder is strict about whitespace. The payload is canonical padded base64.
- Text is UTF-8 only. `read_text` accepts only `encoding="utf-8"`
  (case-insensitive); `write_text` requires valid UTF-8. There is no
  newline translation and no encoding conversion. Reads and writes are
  whole-file operations: there is no streaming, partial read, seek, or append
  mode.
- `listdir` returns the host's entry maps directly. `type` is `"file"`,
  `"directory"`, or `"symlink"`. `size` is the file size for files;
  directories may report `0`.
- Relative paths are forwarded unchanged. The host enforces the workspace root
  and path policy; the guest does not add a fake path sandbox.
- Concurrency: each method awaits `quickpy.tools.fs_*`, so
  `asyncio.gather(quickpy.fs.read_text("a"), quickpy.fs.exists("b"))` uses the
  existing tool scheduler. Deadlines, cancellation, the call ledger, and
  out-of-order delivery are unchanged. The host adapter is concurrency-safe;
  the guest does not serialize calls.
- Side effects are immediate and are **not** rolled back if a later Python
  statement raises; `TestWorkspaceSideEffectsNotRolledBack` covers this.
  `write_text`/`write_bytes` rely on the backend's atomic write behavior;
  there is no multi-operation transaction.
- Error mapping: `not_found` -> `OSError(2, message)` (`.errno == 2`);
  `permission_denied` -> `OSError(13, message)` (`.errno == 13`);
  `invalid_path` -> `ValueError("invalid_path: " + message)`. Other
  `ToolError` codes propagate unchanged, including `tool_not_found` ->
  `ToolNotFoundError`. This ROM level does not define CPython's
  `FileNotFoundError` or `PermissionError` subclasses, so the facade uses
  `OSError` with the conventional errno numbers. This is not a CPython `os`
  emulation layer and does not claim errno parity beyond those two codes.
  Host operation messages contain only caller-relative paths and static
  context; the host workspace root is not disclosed. See the error
  sanitization section in [`../capability/README.md`](../capability/README.md).

## Intentionally unsupported

`os`, `uos`, `pathlib`, `subprocess`, `tempfile`, `errno`, `random`, `gc`,
`time`-as-clock, and any filesystem/network/environment/process module remain
unimportable (`ImportError`). `open()` raises `OSError`. There is no public
`gc` exposure. `time.sleep` still raises `UnsupportedError` via the frozen
`quickpy` patch. `quickpy.fs` is not `os`: it exposes only the eight methods
above, has no ambient authority, and does not add `open`, `stat`, `rename`,
`chdir`, environment, or process access.

## Differences from CPython (recorded, not silently changed)

- `re.split` is **not** a module-level function. Use
  `re.compile(pattern).split(text)`. This is upstream MicroPython behavior.
- `json.dumps(float("inf"))` emits the bare token `inf` and
  `json.dumps(float("nan"))` emits `nan`. These are **not valid JSON**.
  `json.loads("Infinity")` and `json.loads("NaN")` raise
  `ValueError("syntax error in JSON")`. The public state/tool value model stays
  strict-finite; this only affects values a script builds and serializes
  itself.
- `math` has no `isclose`/`factorial`/`tau`/`inf`/`nan` at this ROM level.
- `str` is Unicode-aware: `len`/index/slice count code points and `\uXXXX`
  produces valid UTF-8. This was enabled for JSON/Unicode correctness and is a
  behavior change from the previous byte-oriented minimum config.
- `str.encode()` and `bytes.decode()` are **not** available at this ROM level
  (`MICROPY_CPYTHON_COMPAT` is off). Use `bytes(text, "utf-8")` and
  `str(data, "utf-8")` instead. The `quickpy.fs` facade uses those forms
  internally.
- The guest executes source in REPL-like mode, so a bare expression statement
  prints its value. This is pre-existing adapter behavior, not new.

## Artifact evidence

Built with the pinned toolchain from `guest/build.sh` (Zig 0.17.1, wasm-opt
133, MicroPython v1.29.0 commit
`0fd6c573ea815774668bbb16b8e197c8822368b2`).

- size: **256,486 bytes** (previous 251,590; +4,896 / +1.9%)
- sha256: `5fdd351a70e0f65a4b812ad21c34a0885f87e839aee7721164768907ad61f50a`
- ABI: **4** (unchanged; no protocol change)
- engine: `micropython` / `1.29.0`
- imports: **0** (`grep -c '(import' guest/build/quickpy.wat` → `0`)
- tag sections: **1** (`grep -c '(tag ' guest/build/quickpy.wat` → `1`)
- memory: initial **64 pages / 4 MiB**, maximum **1024 pages / 64 MiB**
  (`(memory $0 64 1024)`); MicroPython heap **4 MiB**
- exports: `memory`, `_initialize`, `qp_abi_version`, `qp_alloc`, `qp_free`,
  `qp_invoke`
- reproducibility: two clean rebuilds (`rm -rf guest/build && ./guest/build.sh`)
  produced the same size and sha256.

The app should pin the artifact by content (size + sha256), not by ABI alone.
ABI 4 identifies the protocol; the hash identifies this exact capability set.

## Memory / performance impact

- The new stdlib modules are compiled C code only; they add no per-instance
  state and no extra WASM instances. `quickpy.fs` is a small frozen Python
  object; it adds no host state and no extra WASM instances.
- Smoke harness peak RSS with `/usr/bin/time -l` (macOS arm64, Go 1.27.4,
  wazero 1.12.0): **~107-108 MiB** for 94 scenarios. The M1 baseline was
  ~69-72 MiB for 78 scenarios. The delta is consistent with additional
  per-scenario 4 MiB linear memories that wazero/Go retain after close, not a
  guest leak. The harness still reports `created == closed` (94/94).
- Per-instance linear memory and heap are unchanged (4 MiB initial / 64 MiB
  max, 4 MiB heap).

## Tests

```sh
./guest/build.sh
cd guest/smoke
go run . ../build/quickpy.wasm
go vet ./...
```

Result: **135 checks pass** (129 previous + 6 new `quickpy.fs` checks). The
new checks cover the facade's plain return values, exact tool names and
arguments (including canonical base64 without the `b2a_base64` newline),
common host error mapping, unregistered-tool `ToolNotFoundError`, local
type/keyword validation, and `asyncio.gather` with out-of-order delivery. The
previous 129 checks still cover JSON, `re`, `math`, `io.StringIO`, binascii,
`open` still `OSError`, and no `os`/`pathlib`/`subprocess` imports. The harness
reports `created == closed` (94/94).

Real-artifact integration evidence for this baseline:

- `TestScriptingCorpusJSON`, `TestScriptingCorpusRegex`,
  `TestScriptingCorpusMath`, `TestScriptingCorpusStringIOAndText`,
  `TestScriptingCorpusBinary`, `TestScriptingCorpusDefaultDeny`,
  `TestScriptingCorpusIdentityAndFreshness`,
  `TestScriptingCorpusFilesystemDeferred`, and
  `TestScriptingCorpusModuleMatrix` (nine top-level corpus checks).
- `TestWorkspaceFilesystemOperationsPersist`,
  `TestWorkspaceReadOnlyAndContainment`, and
  `TestWorkspaceSideEffectsNotRolledBack` (three full-stack workspace checks).

The native embedding reference in `examples/embed/` passes the artifact as
`ArtifactSource.Bytes` and needs no writable `TMPDIR`. As a run-specific
example, a stripped darwin/arm64 build measured 6,507,490 bytes when the
generated artifact was 251,590 bytes earlier on this branch. That number is an
observation, not a current artifact size or a distribution guarantee. Linux and
Windows cross-builds were built but not run.

## Host / integration follow-up

- `integration/security_test.go` `TestGuestImportsAndModules` now expects
  `math` to be importable; the earlier expectation that `math` raises
  `ImportError` was updated with this baseline. `os`, `uos`, and `gc` still
  raise a clean `ImportError`.
- The Go bridge lives in `capability/quickpybridge`. It registers the eight
  `fs_*` tools from a `*capability.Adapter`; the application still owns the
  workspace root, read-only policy, size limits, and when to call `Install`
  (before `Execute`). The guest facade is inert without that registration.
- The full stack is now exercised against the real artifact by
  `TestWorkspaceFilesystemOperationsPersist`,
  `TestWorkspaceReadOnlyAndContainment`, and
  `TestWorkspaceSideEffectsNotRolledBack` in `integration/workspace_test.go`.
  They cover persistence across executions, Unicode and binary round-trips,
  `asyncio.gather`, read-only denial, traversal/absolute-path rejection, an
  outside sentinel, and a write that persists after a later Python exception.
  Symlink containment is covered at the capability layer by
  `capability/security_test.go`, not by the integration test. This is
  real-workspace evidence, not a future plan.
- No ABI bump is needed. If the app pins the artifact hash, update the pinned
  size/hash to the values above.
