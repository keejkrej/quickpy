# Real-artifact integration tests

These tests exercise the public QuickPy Go SDK (`github.com/keejkrej/quickpy`)
against the real MicroPython guest artifact. They are intentionally gated so a
plain `go test ./...` on a clean checkout never needs `guest/build/quickpy.wasm`
or the Zig/MicroPython build toolchain.

## Gate

`QUICKPY_TEST_ARTIFACT` selects the artifact:

- **unset** — every test calls `t.Skip`; the package is a no-op for the default
  test run.
- **set** — the value is a path to a real ABI 4 MicroPython artifact. Relative
  paths are resolved from the repository root, so `guest/build/quickpy.wasm`
  works from any working directory.

The current M2 scripting-foundation artifact is ABI 4: **256,486 bytes** with
SHA-256
`5fdd351a70e0f65a4b812ad21c34a0885f87e839aee7721164768907ad61f50a`. It adds
the scripting stdlib baseline and the inert `quickpy.fs` facade; see
[`../guest/SCRIPTING_NOTES.md`](../guest/SCRIPTING_NOTES.md). The accepted M1
reference artifact was 236,119 bytes with SHA-256
`171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`. The earlier
scripting-baseline artifact (JSON/re/math/io.StringIO/binascii, still ABI 4 and
zero imports) was 251,590 bytes with SHA-256
`fde6f80ae8775997360f09ca61e0a994f224f4f8f99e4a79811983b8e216af82`. The
previously recorded ABI 3 artifact was 227,717 bytes with SHA-256
`a20ca657e77364104902dff759c8be42c964e2cd9fe8f21c97102a606c11a4fc`. The tests
log the observed size and hash and require ABI 4 with zero WASI imports; they do
not hard-code the hash so a rebuilt artifact can still be tested. The artifact
identity test asserts ABI 4 directly. The ABI 4-only suites also gate on the
observed ABI, so a pending ABI 3 artifact reports a clear skip instead of a
misleading failure. The ABI 3 tool-conformance suites accept ABI 3 or newer, so
they still run against the final ABI 4 artifact.

`QUICKPY_SOAK` is a second, independent opt-in gate for the resource/soak test.
It is off by default and still requires `QUICKPY_TEST_ARTIFACT`.

## Commands

```sh
# Default run: integration tests skip cleanly.
go test ./...

# Real artifact, verbose.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -v ./integration

# Real artifact under the race detector.
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -race ./integration

# Opt-in soak/resource evidence (about 33 s on the reference machine).
QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm QUICKPY_SOAK=1 \
  go test -count=1 -run TestSoak -v ./integration
```

## What is covered

### State, events, lifecycle, registry

- Explicit state: initial int/float identity, get/set, deep copy, last write,
  `set_state(None)` vs no write, nested JSON, and no state commit on script
  error, stall, cancel, timeout, or final oversize.
- Ordered `TextEvent`/`ConsoleEvent`/`ImageEvent`/`ToolEvent` delivery through
  both the request sink and `Result.Outputs`, including controlled out-of-order
  `asyncio.gather` completion, opaque call IDs, unknown tools, handler
  arguments, and deadlines.
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
  imports, unavailable `os`/`uos`/`gc` modules, and no guest stdout leak to the
  host process. The negative module list is about ambient OS/process access,
  not pure computation: language-only modules such as `math` are allowed and
  are covered by the scripting corpus.

### Structured console and image output (ABI 4)

- All four console levels (`debug`, `info`, `warn`, `error`) and the
  print-like stringification of multiple arguments (`str()` joined by single
  spaces, with no trailing newline). The console surface is strict: there is no
  generic `log`/`trace` entry point that could carry an arbitrary level token.
- Precise ordered `print`/`console`/`print` delivery through the sink and
  `Result.Outputs`, with no duplicate legacy text.
- Exact raw image round-trip, including invalid UTF-8 and NUL bytes, the
  documented default MIME, and `bytearray` input. The guest captures a copy at
  emit time, so mutating the caller's buffer afterwards does not change the
  emitted image.
- Invalid image data and MIME forms are clear catchable `TypeError`/`ValueError`
  script errors, never `HostFault` or `ProtocolError`, and never commit state.
- Ordered `print`/`console`/`image`/tool start/response cycles across async
  suspensions, with sink and `Result.Outputs` agreeing and no duplicate deltas.
- The cumulative `MaxOutputBytes` cap counts text bytes, console text bytes, and
  decoded image bytes, never raw JSON/base64 characters. An image that no
  longer fits after earlier output, and a valid single image larger than the
  total cap, are both retained as a legitimate raw byte prefix and return
  `LimitExceeded`. The tests do not assert that a truncated prefix is a formed
  image. Invalid image structure, base64, or MIME remains a `ProtocolError`.
- Failed, stalled, and cancelled executions retain their partial ordered
  outputs and commit no state. A panicking event sink is recovered as
  `HostFault` and does not escape `Wait`; the runtime remains usable with a
  non-panicking per-execution sink.

### Request caps (ABI 4)

- `MaxSourceBytes`: exact cap allowed, one byte over is `InvalidRequest` before
  admission, and the rejected request does not consume `MaxExecutions`.
- `MaxToolCalls`: guest-side inclusive cap. With `MaxToolCalls=1` the first call
  is dispatched and the second raises a catchable `ToolLimitError`; uncaught it
  is `ScriptError` with `Error.Tool`. Public `MaxToolCalls=0` maps to the ABI 3
  `-1` unlimited sentinel and is exercised with 1001 sequential calls, so it
  does not fall back to the old 1000-call default.
- `MaxPendingCalls`: two valid concurrent calls with a cap of 1 fail hard with
  `LimitExceeded` before any handler is dispatched.
- `MaxResumes`: a valid two-call flow with a cap of 1 fails hard with
  `LimitExceeded`.
- `MaxConcurrentToolCalls`: cap 1 serializes real handlers; cap 2 allows
  observed overlap.
- `MaxToolResultBytes`: exact JSON-encoded boundary allowed, one byte over is
  `LimitExceeded`, and an invalid Go result shape is `ProtocolError`.
- `MaxMessageBytes`: request, initial-state, and response caps. The response
  boundary is found by binary search so the test asserts the exact allowed
  message size and the one-byte-over rejection. An oversized source is
  `InvalidRequest` before the guest starts.
- Every error case asserts that no state is committed.

### Structured tool errors and guest identity (ABI 4)

- Caught `ToolNotFoundError`, `ToolArgumentError`, `ToolLimitError`, and base
  `ToolError` print their class names; caught failures are ordinary script
  success and may commit state.
- Uncaught tool failures produce `ScriptErrorInfo` plus `Error.Tool` with
  code/message/tool and, when a call was registered, the opaque `CallID`.
  `ToolArgumentError` and `ToolLimitError` are raised before a call is
  registered, so they have no call id.
- A regular `ValueError` has `Error.Tool == nil`.
- `Guest.Engine == "micropython"`, `Guest.Version == "1.29.0"`,
  `Guest.ABIVersion == 4`, and zero imports.
- Handler panic is `HostFault`; an invalid Go result shape is `ProtocolError`;
  an oversized result is `LimitExceeded`. Each failure leaves the runtime
  healthy for the next execution.

### Scripting corpus (compiled-agent target)

[`../guest/SCRIPTING_NOTES.md`](../guest/SCRIPTING_NOTES.md) records the
ordinary Python-C-style scripting corpus and its observed support matrix. The
tests are top-level scripts, not agent-API examples, and they do not claim a
persistent REPL. They cover:

- `json` loads/dumps with Unicode, nesting, `None`/`true`/`false`, and float64
  numeric output. Dict order is MicroPython hash order, so assertions are
  semantic rather than CPython byte-exact.
- `re` groups/search/sub and a clean `ValueError("error in regex")` pattern
  error; compiled-pattern `split`/`sub`; module-level `split`/`findall` are
  recorded as clean unsupported when absent.
- `math` sqrt/floor/trig accuracy and a clean domain error.
- `io.StringIO` text funnel; `csv` and `textwrap` are recorded as clean
  unsupported when the frozen modules are absent.
- `binascii` hex/base64 round trips that preserve NUL and `0xff` bytes, with
  bytes and text inputs.
- Default-deny: `subprocess`/`pathlib`/`os`/`uos`/`gc`/`socket`/`shutil`/
  `tempfile`/`threading`/`multiprocessing` are unimportable, `open()` is a
  clean `OSError`, `quickpy.fs` is present but inert (every method is a
  catchable `ToolNotFoundError` when no host `fs_*` tools are registered), and
  unregistered tool calls are catchable `ToolNotFoundError` values that create
  no files, change no host environment, and leak no host data.
- Honest identity and freshness: `sys.version` identifies MicroPython,
  `sys.implementation` reports the wasm32 guest, guest imports are empty,
  globals and imports are fresh per call, and state is explicit
  (`InitialState`/`set_state`) rather than an implicit persistent REPL.
- Filesystem read/write through ambient `open()` is cleanly unsupported; the
  practical future `fs_*` wrapper names are denied rather than silently
  succeeding. The opt-in `quickpy.fs` facade is covered separately below. This
  is evidence for the wrapper layer, not a claim that ambient filesystem
  support exists.

### Opt-in workspace filesystem (M2 scripting foundation, accepted)

[`workspace_test.go`](workspace_test.go) exercises the full stack: the
runtime-neutral [`capability`](../capability/README.md) core, the
`quickpybridge.Install` adapter, the guest `quickpy.fs` facade, and the real
artifact. The core contract is documented in
[`../capability/README.md`](../capability/README.md).

- `TestWorkspaceFilesystemOperationsPersist` covers `mkdir` (including
  `parents=True` idempotence), text and binary writes, `exists`, `listdir`
  entry shape, `remove`, and a second execution that reads the persisted host
  files. Unicode text and NUL/`0xff` binary bytes round-trip. Guest globals are
  still fresh per execution; only the host files persist.
- `TestWorkspaceReadOnlyAndContainment` covers explicit `ReadOnly: true`
  denial of every mutation, `..` traversal and absolute-path rejection, and an
  outside sentinel that is never read or written. The host root path is not
  disclosed in operation errors. Symlink containment is covered at the
  capability layer by [`../capability/security_test.go`](../capability/security_test.go),
  not by this integration test.
- `TestWorkspaceSideEffectsNotRolledBack` writes a file and then raises a
  Python exception, proving the host side effect persists and is not rolled
  back.
- `TestWorkspaceFilesystemOperationsPersist` also exercises `asyncio.gather`
  over `quickpy.fs` calls. There is no multi-operation transaction; a failed
  later statement does not roll back an earlier successful write.

These tests are real-artifact evidence for the accepted M2 scripting
foundation. They do not claim full M2, a polyglot agent, or full secure OS
isolation; see the known limitations in [`../capability/README.md`](../capability/README.md).

### Supported `asyncio` subset

The normative matrix is in [`ASYNCIO.md`](ASYNCIO.md). The executable evidence
covers `gather`, `create_task`, `sleep(0)` interleaving, `Lock` critical
sections, `Event` wakeup and unsatisfiable stall, `wait_for` with `None`/`0`,
positive-timer rejection for `asyncio.sleep` and `time.sleep`, cancellation
before host dispatch, absent `Queue`/`wait`/`Condition`/`Semaphore`, stream-I/O
`UnsupportedError`, loop entry-point rejection, the final-top-level-statement
rule, and the absence of threads/multiprocessing/sockets/TLS/select.

### Soak/resource evidence

[`SOAK.md`](SOAK.md) records the opt-in `TestSoak` command, methodology, and the
final ABI 4 reference run: 1060 executions in 32.51 s, zero errors, per-batch
RSS, goroutine, and allocation trends, and the cancellation/late-handler check.
The test fails only on a clearly monotonic, large resource increase across
comparable batches; a single RSS sample is not treated as a memory-bound proof.
Skipping the soak is reasonable for an ordinary correctness pass; run it when
resource behavior is the question.

## Known synthetic-only or future coverage

These are not claimed as real-artifact coverage by this suite:

- Malformed/truncated protocol frames, ABI mismatch, duplicate call IDs, and
  other ledger violations are covered by the low-level synthetic testdata in
  `internal/wasmguest/testdata`, not by the real artifact.
- Malformed output fields (unknown output type, missing text, bad level, bad
  MIME, invalid base64, both legacy `output` and `outputs` with conflicting
  content) are host protocol validation. They are covered by the synthetic
  protocol tests, not by the real artifact. The real guest emits only
  well-formed `outputs` entries.
- Per-execution memory tightening is rejected before admission; the runtime
  level `MaxMemoryBytes` is the only enforced memory cap.
- Positive timers, host clocks, `awaiting_timer`, top-level `await`/`return`,
  threads, multiprocessing, sockets, TLS, and arbitrary async I/O are not
  supported.
- The guest heap is 4 MiB and linear memory is capped at 64 MiB; the soak test
  records process RSS but does not claim a memory bound.
- Filesystem read/write through ambient `open()` is unsupported. The opt-in
  `quickpy.fs` facade is covered by the workspace tests above; it is not an
  ambient filesystem profile and does not claim full M2 or polyglot completion.
- A stable artifact distribution/embedding contract and pi-go integration are
  future work.

## Identity fields

`TestArtifactIdentity` requires `Guest.ABIVersion == 4`,
`Guest.Engine == "micropython"`, `Guest.Version == "1.29.0"`, and
`Guest.ProtocolVersion == "0"`. The protocol version is the JSON envelope
version (`v:0`), not the ABI integer; the ABI integer is reported separately in
`Guest.ABIVersion`. The guest engine version stays `1.29.0`; ABI 4 changes the
host/guest output protocol, not the MicroPython version.

## Test inventory

The package has 85 top-level test functions, including the opt-in `TestSoak`
(84 run by default; `TestSoak` is skipped unless `QUICKPY_SOAK` is set). 11 of
the 85 are ABI 4-gated (`requireABI4`) and skip on an older artifact; the ABI 3
tool-conformance suites accept ABI 3 or newer so they still run against the
final ABI 4 artifact. Nine of the 85 are the scripting corpus in
`scripting_corpus_test.go`; they are adaptive and record clean unsupported
results instead of skipping. Three are the opt-in workspace filesystem tests
in `workspace_test.go`.
