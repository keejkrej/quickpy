# QuickPy M2 status: scripting foundation

**Status: accepted M2 scripting foundation (independent verification,
2026-11-19).** This revision is a reusable QuickPy + runtime-neutral filesystem
foundation consumed by the `script-agent` test field. It is **not** full M2, a
polyglot agent, a general OS sandbox, or a JavaScript facade inside QuickPy.
M1 remains the accepted execution milestone; see [`M1_STATUS.md`](M1_STATUS.md).

The original confidentiality NO-GO was resolved, and the focused independent
gates all pass. The remaining items below are documented coverage limits and
future work, not current defects.

## What this revision adds

- **Scripting stdlib baseline in the guest:** `json`, `re`, `math`,
  `io.StringIO`, and `binascii`. These are pure computation and add no WASI
  import. The exact surface and CPython differences are in
  [`../guest/SCRIPTING_NOTES.md`](../guest/SCRIPTING_NOTES.md).
- **Runtime-neutral filesystem capability core:** [`../capability/`](../capability/)
  exposes eight versioned operations over one host-chosen `os.Root`, using only
  the Go standard library. The v1 operation, argument, result, error-code, path,
  and limit contract is in [`../capability/README.md`](../capability/README.md).
  CAPv1 is an in-process data ABI, not a network protocol.
- **Explicit QuickPy consumption:** `capability/quickpybridge.Install`
  registers the eight `fs_*` tools on a runtime, using the same runtime tool
  snapshot as any other tool. There is no automatic I/O.
- **Opt-in guest facade:** `quickpy.fs` is present in the artifact but inert
  until the host registers the tools. With no registration, every method is a
  catchable `ToolNotFoundError`.
- **Native embedding reference:** [`../examples/embed/`](../examples/embed/)
  passes the artifact as `ArtifactSource.Bytes` via `go:embed`, so the binary
  needs no artifact file and no writable `TMPDIR`.

## Independent verification

- Artifact: **256,486 bytes**, SHA-256
  `5fdd351a70e0f65a4b812ad21c34a0885f87e839aee7721164768907ad61f50a`, ABI
  **4**, MicroPython **1.29.0**, **zero** WASI imports, **1** EH tag, initial
  memory **64 pages / 4 MiB**, maximum **1024 pages / 64 MiB**, guest heap
  **4 MiB**.
- Guest smoke: **135/135** checks pass, **94/94** instances closed.
- `go vet`, `go build`, `CGO_ENABLED=0 go test`, and the default test suite
  pass. `gofmt` diff check is clean.
- `go test -race ./capability/...` and the sanitizer race pass.
- The real-artifact integration suite passes all tests except the opt-in
  `QUICKPY_SOAK` test, which is intentionally skipped by default.
- Post-sanitizer targeted workspace tests: **3/3** pass in **0.465 s**.
- Race representatives: **12 tests** all pass in **29.3 s**. A previous
  complete prototype full race run was about **165 s**.
- Filesystem unit tests cover the earlier concurrency, symlink, and path-limit
  behavior exactly.
- Native embedded example: direct `ArtifactSource.Bytes` with no `TMPDIR`
  requirement and `CGO_ENABLED=0` was validated against the current artifact
  via an overlay. A linux/amd64 tagged cross-build was built but not executed.
  The read-only independent review did not run `examples/build.sh`; the example
  worker's prior full script produced a functional native binary. A stripped
  darwin/arm64 build measured **6,507,490 bytes** when the generated artifact
  was 251,590 bytes earlier on this branch. That is a historical, run-specific
  observation, not a current size guarantee.
- No actual device `EIO` fault was simulated. That is recorded as a coverage
  limitation, not a current defect.

## Review finding: operation error path secrecy

An independent review found that raw post-open `os.PathError` text could leak
the host workspace root through `capability.Adapter.Handle` errors, notably
the read and fallback `io_error` paths. This was resolved on the branch and
independently re-verified:

- `Handle` normalizes every error at its boundary. Messages contain only
  static text and caller-relative paths; raw host error text is never copied.
- `io_error` fallback messages use only a whitelisted operation name and
  static syscall errno text.
- Classification still uses `errors.Is`/`errors.As` before sanitization, so
  the capability error codes and guest bridge mapping are unchanged.
- `NewWorkspace` constructor errors remain host-facing diagnostics and may
  name the root; they are outside the operation contract.
- Regression coverage includes hostile root-qualified `*os.PathError` values
  for read, write, stat, and readdir, a synthetic post-open read fault, all
  eight operations, and the central boundary. `go test -race ./capability/...`
  passes.

This is a path-secrecy fix, not a new confidentiality or isolation claim.

## Filesystem behavior and limits

The capability is explicit and workspace-scoped. A writable workspace is
explicit; a read-only workspace is also explicit (`ReadOnly: true`). The guest
never chooses the root, resolves `..`, or grants permissions.

- Text is UTF-8 only. Reads and writes are whole-file; there is no streaming,
  partial read, seek, or append mode.
- Binary data uses standard padded base64 with strict whitespace rejection.
- Installed `fs_*` tools persist across executions; guest globals do not.
- Error mapping in the facade: `not_found` -> `OSError(2)`,
  `permission_denied` -> `OSError(13)`, `invalid_path` ->
  `ValueError("invalid_path: ...")`. Other `ToolError` codes propagate
  unchanged. This ROM does not define CPython's `FileNotFoundError` or
  `PermissionError` subclasses.
- Side effects are immediate and are not rolled back if a later Python
  statement raises (`TestWorkspaceSideEffectsNotRolledBack`). There is no
  multi-operation transaction.

Known limitations, documented rather than hidden:

- Hardlinks, bind mounts, devices, and FIFOs are host responsibilities.
  `os.Root` prevents path escape, not every special-file or hardlink case.
- There is no `fsync`; writes are atomic for readers but not crash-durable.
  A host crash may leave a `.capability-tmp-*` file behind.
- No glob or rich pattern matching, no recursive remove/copy, and no append
  modes.
- No full `open`/`pathlib`/`tempfile`/`subprocess` API. `quickpy.fs` is a
  limited async facade, not a general OS layer.
- Path sanitization is a capability-boundary concern. Any future JavaScript or
  node facade must reuse the same sanitized operation/error contract rather
  than exposing raw host paths.
- This is not a claim of full secure OS isolation.

## Open work

- **Language frontends:** no JavaScript `node:fs` facade, Bash facade, Zig
  facade, or general CPython `os` support exists **inside QuickPy**. The
  user-created `script-agent` test field is the next consumer for new facades.
  Future consumers should target the same runtime-neutral, versioned
  operation/error contract. Nothing here equates QuickPy with the compiled-agent
  target.
- **Process, working-directory, and environment policy:** the current
  capability covers filesystem operations only. A module interface beyond
  filesystem (process, cwd, env) is not designed or implemented.
- **Test coverage:** the workspace tests are real-artifact evidence, but they
  are heuristic for special files and platform-specific limits. They do not
  prove full isolation or crash durability.
- **Polyglot direction:** the eventual target is more than a single Python
  agent. The user-created `script-agent` test field is the next consumer; no
  application repository is part of this QuickPy revision, and no wire protocol
  exists yet. The v1 core is an in-process data ABI.
- **pi-go:** the local pi-go prototype is a reference only. This revision does
  not include or upstream any local pi-go changes, and no pi-go repository is
  edited by this packaging job.
- **ADR:** add one only if a real architectural decision is made that the
  existing docs do not already record. The current v1 contract and limits are
  documented in [`../capability/README.md`](../capability/README.md); no new
  ADR is required for this revision.

## Next step

Start the `script-agent` test field and polyglot composition against this
accepted foundation. A new Zig adapter is optional; do not rewrite the existing
engine or add unbounded wrappers. Keep the runtime-neutral operation/error
contract as the anchor.
