# capability — runtime-neutral filesystem capability core (v1)

`capability` is a small Go package that exposes a stable, JSON-like filesystem
operation surface over one host-chosen workspace root. It is the shared core
for polyglot agent execution: a Zig, Python, or JavaScript bridge can forward
an operation name plus `map[string]any` arguments and receive a
`map[string]any` result or a structured `*Error`.

This package is deliberately independent of QuickPy, of any Python object
model, and of any guest runtime. It imports only the Go standard library.

**Status:** v1 is an in-process data ABI, not a network protocol. There is no
wire framing, version negotiation, or transport in this package. A future
protocol must carry this same contract.

## Public API

```go
type Adapter struct{ /* unexported */ }

func NewWorkspace(opts WorkspaceOptions) (*Adapter, error)
func (a *Adapter) Handle(ctx context.Context, operation string, args map[string]any) (any, error)
func (a *Adapter) Operations() []string
func (a *Adapter) Close() error

type Error struct {
    Code    string
    Message string
}
func (e *Error) Error() string

type WorkspaceOptions struct {
    Root       string
    ReadOnly   bool
    MaxBytes   int64
    MaxEntries int
}
```

- `NewWorkspace` opens `Root` once with `os.OpenRoot` and binds the capability
  to that directory. Later working-directory changes do not move it.
- `Handle` is safe for concurrent use. A nil `ctx` is treated as
  `context.Background()`.
- `Operations` returns the eight operation names in deterministic sorted
  order. The returned slice is a fresh copy.
- `Close` releases the root handle and is idempotent. After `Close`, `Handle`
  returns `io_error`; `Operations` still reports the static surface.
- `Error` is the only error type returned by `Handle`. Use `errors.As` to
  inspect `Code`.

### Options and defaults

| Field | Zero value | Meaning |
| --- | --- | --- |
| `Root` | required | Host-chosen directory. Empty is rejected with `invalid_argument`. |
| `ReadOnly` | `false` | `false` permits writes; `true` denies every mutation. Set `true` explicitly for untrusted callers. |
| `MaxBytes` | `1 MiB` | Bound for text and binary reads and writes. Negative is rejected. |
| `MaxEntries` | `10000` | Bound for `fs_listdir` entries. Negative is rejected. |

A zero `MaxBytes` or `MaxEntries` selects the default; it never means
"unbounded". `MaxBytes == math.MaxInt64` and `MaxEntries == math.MaxInt` are
rejected to keep the internal `+1` bound from overflowing.

`NewWorkspace` errors are host-facing diagnostics and may name the root path;
they are outside the operation contract. Every error returned by `Handle` is
an `*Error` whose message contains only static text and caller-relative paths.
See [Error sanitization](#error-sanitization).

## Wire values

Arguments and results use JSON-like Go values only:

`nil`, `bool`, `int64`, `float64`, `string`, `[]any`, `map[string]any`.

There is no Python object model and no implicit conversion. Integers are
`int64`; a `float64` is never coerced to an integer. Unknown argument keys,
wrong value types, and missing required arguments are rejected with
`tool_argument` before any filesystem operation runs.

## Operations (v1)

| Operation | Arguments | Result |
| --- | --- | --- |
| `fs_read_text` | `path` string; `encoding` optional string, default `"utf-8"` | `{"text": string}` |
| `fs_write_text` | `path` string; `text` string | `{"written": int64}` (UTF-8 bytes) |
| `fs_read_bytes` | `path` string | `{"base64": string}` (standard padded base64) |
| `fs_write_bytes` | `path` string; `base64` string | `{"written": int64}` (decoded bytes) |
| `fs_exists` | `path` string | `{"exists": bool}` |
| `fs_listdir` | `path` optional string, default `"."` | `{"entries": []any}` |
| `fs_mkdir` | `path` string; `parents` optional bool, default `false` | `{"created": bool}` |
| `fs_remove` | `path` string | `{"removed": bool}` |

### `fs_read_text`

Reads a regular file and returns its contents as a UTF-8 string. The only
accepted `encoding` is `"utf-8"` (case-insensitive); anything else is
`tool_argument`. Invalid UTF-8 is `invalid_utf8`. A directory is
`is_directory`.

### `fs_write_text`

Writes a UTF-8 string atomically. `text` must be valid UTF-8; otherwise
`invalid_utf8`. `written` is the number of UTF-8 bytes, not runes. Parent
directories are not created; use `fs_mkdir` first. A missing parent is
`not_found`.

### `fs_read_bytes`

Reads a regular file and returns standard padded base64
(`base64.StdEncoding`). The decoded size is bounded by `MaxBytes`.

### `fs_write_bytes`

Decodes standard padded base64 and writes the bytes atomically. Whitespace is
rejected even though Go's decoder would skip newlines, and non-canonical
trailing bits are rejected. The decoded length is computed from the input
length and padding **before** allocation, and is checked against `MaxBytes`
before decoding. Invalid input is `invalid_base64`; over-limit input is
`too_large`.

### `fs_exists`

Reports whether the directory entry exists. It uses `lstat`, so a dangling
symlink exists and a symlink to an outside target is not traversed. A missing
path is `{"exists": false}`. A path whose parent is not a directory is
`not_directory`; a permission failure is `permission_denied`.

### `fs_listdir`

Lists a directory. Entries are sorted by name (byte-wise) and never include
`.` or `..`. Each entry is:

```json
{"name": "file.txt", "type": "file", "size": 12}
```

`type` is one of `"file"`, `"directory"`, or `"symlink"`. Metadata uses
`lstat` semantics: a symlink is reported as `"symlink"` and is never followed
for its type or size. `size` is the platform-reported size; for a symlink it
is the link target length, and for a directory it is platform-dependent and
not meaningful. Non-directory, non-symlink special files are reported as
`"file"`.

The implementation reads at most `MaxEntries+1` entries. More than
`MaxEntries` is `too_large`; entries are never silently truncated. A file
path is `not_directory`.

### `fs_mkdir`

Creates a directory with mode `0755`.

- `parents: false` (default): the parent must exist. An existing target is
  `exists`; a missing parent is `not_found`.
- `parents: true`: creates missing parents. An existing directory returns
  `{"created": false}` and is the only idempotent case. An existing
  non-directory is `exists`. A symlink is not treated as a directory.

### `fs_remove`

Removes a file, a symlink, or an **empty** directory. There is no recursive
deletion. A non-empty directory is `not_empty`. A missing path returns
`{"removed": false}` (idempotent delete). Removing the workspace root (`"."`)
is `invalid_path`. Removing a symlink removes the link, not its target.

## Error codes (v1)

| Code | Meaning |
| --- | --- |
| `not_found` | Path or parent does not exist. |
| `permission_denied` | Host permission failure, or the adapter is read-only. |
| `invalid_path` | Path policy violation, root escape, symlink loop, or over-long name. |
| `invalid_argument` | Invalid `WorkspaceOptions` at construction. |
| `tool_argument` | Unknown operation, unknown key, wrong type, missing argument, or unsupported encoding. |
| `too_large` | Exceeds `MaxBytes` or `MaxEntries`. |
| `is_directory` | A read or write target is a directory. |
| `not_directory` | A directory was required but the path is not one. |
| `exists` | `fs_mkdir` target already exists in a non-idempotent case. |
| `not_empty` | `fs_remove` target is a non-empty directory. |
| `io_error` | Other host I/O failure, non-regular file, closed adapter, or cancellation. |
| `invalid_utf8` | Text is not valid UTF-8. |
| `invalid_base64` | Base64 is not canonical standard padded base64. |

Cancellation and deadline expiry surface as `io_error` with a message
beginning `operation cancelled` or `operation deadline exceeded`. The code set
is closed: the core never invents a code outside this table. Language facades
map these codes to their own exception classes.

## Path policy

Paths are POSIX relative paths inside the root.

- Empty paths are rejected (`invalid_path`), except that `fs_listdir` may omit
  `path` entirely, which means `"."`.
- Absolute paths (`/...`), UNC paths (`\\...`), backslashes, and colons are
  rejected. Rejecting `:` also rejects Windows drive letters and alternate
  data streams.
- NUL is rejected.
- `..` components that escape the root are rejected. Non-escaping `.` and
  `..` components are cleaned normally, so `a/../b` means `b`.
- A trailing slash is accepted and cleaned, so `sub/` means `sub`.
- `~` is a literal filename; there is no home-directory expansion.
- The host chooses the root. Operations never accept an absolute path and
  never expose the root path in an operation error.

## Limits

- `MaxBytes` applies to `fs_read_text`, `fs_read_bytes`, `fs_write_text`, and
  `fs_write_bytes`.
- Reads stat the file for an early rejection, then read through
  `io.LimitReader(f, MaxBytes+1)`. The `LimitReader` is authoritative even if
  the file grows after the stat. A read never allocates more than
  `MaxBytes+1` bytes.
- Writes check the byte length before creating a temporary file. Base64 writes
  compute the exact decoded length before allocating.
- `fs_listdir` reads at most `MaxEntries+1` entries and returns `too_large`
  rather than truncating.

## Read-only mode

`ReadOnly: true` denies `fs_write_text`, `fs_write_bytes`, `fs_mkdir`, and
`fs_remove` with `permission_denied` before any filesystem mutation. No
temporary file is created. Reads, `fs_exists`, and `fs_listdir` still work.

## Atomic writes

Writes use a unique temporary file in the target directory, then `rename` over
the target:

1. create `.capability-tmp-<random>` with `O_CREATE|O_EXCL`, mode `0600`;
2. write the bytes and close;
3. check the context;
4. rename over the target.

A failed or cancelled write removes the temporary file. There is no `fsync`,
so atomicity is guaranteed for concurrent readers but durability across a
host crash is not; a host crash may leave a `.capability-tmp-*` file behind.
Writes replace the directory entry: writing to a final-component symlink or
hardlink replaces the link, it does not modify the link target. Parent
directories are not created. Temporary files may be visible to a concurrent
`fs_listdir`.

## Security model

- Containment is enforced by `os.Root` (Go 1.24+), which binds a directory
  handle and refuses any path or symlink that escapes it. The package does not
  rely on a check-then-use path validation for security; `os.Root` guards the
  actual open, create, rename, and remove.
- Symlinks are followed only when they resolve inside the root. Absolute
  symlinks and relative symlinks that climb out are denied with
  `invalid_path`. Symlink loops are `invalid_path`.
- `fs_listdir` and `fs_exists` use `lstat` semantics and never follow a
  symlink for metadata.
- Operation errors contain only the caller-supplied relative path and static
  operation/errno context. The host root path is never included.
- The package has no environment, network, process, or native FFI capability.

### Error sanitization

Every error returned by `Handle` is an `*Error`. Its `Message` is built from
static text and caller-relative paths only. Raw host error text is never
copied, including root-qualified `*os.PathError` values that `os.Root` may
return from post-open `Read`, `Write`, `Stat`, or `ReadDir` calls.

- Classification uses `errors.Is`/`errors.As` before sanitization, so
  `not_found`, `permission_denied`, and the other codes keep their meaning.
  The `io_error` fallback includes only a whitelisted operation name and
  static syscall errno text, never the raw error string.
- `os.Root`'s unexported escape sentinel is recognized by its stable
  "escapes from parent" text as a classification aid. The security boundary
  is `os.Root` itself, not the string check.
- The `Handle` boundary is a last-resort filter: a non-`*Error` becomes a
  generic `io_error`, and the host root is redacted from any message that
  accidentally contains it. Trivial roots (`"."` and the filesystem
  separator) are not redacted because they are not secret and redacting them
  would mangle messages.
- The `Error` type has no `Unwrap`, so the underlying cause is not exposed
  through `Error()` or bridge serialization. Hosts that need the original
  cause for diagnostics should capture it at the call site, not from the
  public error.
- `NewWorkspace` constructor errors are host-facing diagnostics and may name
  the root path. They are outside the operation contract. A host that does
  not want the root name in its own logs should wrap or replace that
  constructor error before logging it.

### Known limitations

- **Hardlinks.** `os.Root` cannot distinguish a hardlink inside the root from
  a file created outside it. A hardlink to an outside inode is readable
  through the capability. Atomic writes replace the directory entry, so they
  do not modify the outside inode, but reads can still expose outside
  content. Hosts must not let untrusted code create hardlinks into a
  capability root.
- **Special files.** Non-regular files are rejected after open with
  `io_error`. Opening a FIFO may block on some platforms before that check.
  Do not place FIFOs, devices, or sockets in a capability root.
- **Filesystem boundaries.** `os.Root` does not prohibit bind mounts, `/proc`
  special files, or device files. The host must choose a root that does not
  contain them.
- **Concurrent mutation.** `os.Root` prevents escape, not logical races.
  Concurrent mutation of the same path is not transactional; for example, a
  directory can become non-empty between the emptiness check and `remove`.
- **No `fsync`.** Writes are atomic for readers but not crash-durable.
- **No recursive remove, append, copy, rename, or mode control.** Writes
  create files with mode `0600`; directories use `0755`.
- **Windows.** The path policy rejects backslashes and colons. Some host errno
  distinctions may surface as `io_error` rather than a more specific code.

## Concurrency and context

- `Adapter` is safe for concurrent use. Configuration is immutable after
  construction and `os.Root` is safe for concurrent use.
- `Close` waits for in-flight operations to finish. `Handle` after `Close`
  returns `io_error`.
- The context is checked at operation boundaries and immediately before an
  atomic write is committed. Cancellation removes the temporary file.
- The package starts no background goroutines.

## Versioning

v1 fixes the operation names, argument keys, result keys, error codes, path
policy, and default limits above. Additive changes that do not alter existing
behavior may land in v1; anything that changes behavior requires a contract
version bump. Bridges should pin the version they implement.

## Backend needs for peer bridges

A Zig, Python, or JavaScript bridge needs to:

1. map its native values to the JSON-like set above (`int64` for integers,
   `[]any`/`map[string]any` for arrays and objects);
2. call `Handle` with the operation name and argument map;
3. map the returned `*Error.Code` to its language's exception or error class;
4. obtain the root and read/write flags from host policy, never from the
   guest.

There is no network protocol today. A future wire protocol must add framing,
version negotiation, and transport while preserving this contract.
