# QuickPy handoff

This document is for a new agent picking up QuickPy on a new machine. It
describes what exists, how to verify it, what is still open, and how the
repository is being handed over.

## 1. Start here

1. Read [`../README.md`](../README.md) for the short orientation.
2. Read [`SPEC.md`](SPEC.md) for the intended contract and milestone criteria.
3. Read [`../guest/M0_NOTES.md`](../guest/M0_NOTES.md) for guest build details,
   pins, the transient ABI, and evidence.
4. Read [`../cmd/quickpy-spike/README.md`](../cmd/quickpy-spike/README.md) for
   the host CLI and the exact evidence commands.
5. Check the current git state and the handoff PR (see §9).

QuickPy is an M0-accepted feasibility baseline plus a proposed specification.
It is not a stable public SDK, not CPython, and not pi-go integration.

## 2. What exists

- **Guest:** MicroPython v1.29.0 plus a small C adapter, compiled to a
  reactor-style WASI module. The adapter is C, compiled with Zig as the C
  toolchain. A Zig-language adapter is a possible later path, not current work.
- **Host:** a Go/wazero adapter in `internal/wasmguest` and a CLI in
  `cmd/quickpy-spike`. The host owns instantiation, tool dispatch, scheduling,
  cancellation, limits, and structured results.
- **Spec:** `docs/SPEC.md` defines the proposed public Go contract, guest
  protocol, error taxonomy, limits, and milestones.
- **Tests:** synthetic host-adapter fixtures under
  `internal/wasmguest/testdata/` and a separate real-guest smoke module under
  `guest/smoke/`.

The guest artifact is **not committed**. `guest/build/quickpy.wasm` is generated
and gitignored. A clean clone must run `./guest/build.sh` before using the real
guest.

## 3. Prerequisites (exact)

| Tool | Version / note |
|---|---|
| Go | **1.25.0 or newer** (current `go.mod` says `go 1.25.0`) |
| Zig | **0.17.1** (C toolchain and wasi-libc) |
| binaryen / `wasm-opt` | **133** |
| git, make, python3 | Required by `guest/build.sh` |
| Network | Required on first build to clone MicroPython v1.29.0 |
| Go module | `github.com/tetratelabs/wazero v1.12.0` |
| Go indirect | `golang.org/x/sys v0.44.0` |

The current `go.mod` module path is `github.com/keejkrej/quickpy`, matching the
remote. If that changes, update imports and this document together.

`guest/build.sh` warns (but does not fail) if Zig or `wasm-opt` differ from the
tested versions. Treat the pins as required for reproducible evidence.

## 4. Clean-clone build and verification

```sh
git clone https://github.com/keejkrej/quickpy.git
cd quickpy

# 1. Build the guest. Fetches the pinned MicroPython checkout into
#    guest/.cache/micropython and writes guest/build/quickpy.wasm.
./guest/build.sh

# 2. Root Go module.
go build ./...
go vet ./...
go test -count=1 ./...

# 3. Separate real-guest smoke module.
cd guest/smoke
go run . ../build/quickpy.wasm
cd ../..

# 4. Real-guest CLI evidence.
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode all -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode async -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode async-cancel -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode stall -json
go run ./cmd/quickpy-spike -artifact guest/build/quickpy.wasm -mode sleep0 -json
```

Expected artifact identity for the current build:

- size: **195,053 bytes**
- sha256: `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`

If a rebuild produces a different hash, record the new hash and treat
reproducibility as an open M0 item rather than silently updating the number.

## 5. Current evidence status (M0 accepted)

Independent verification returned **GO on all 11 M0 exit criteria**. M0 is an
accepted feasibility baseline, not a production SDK.

Verified on the development machine:

- MicroPython v1.29.0 builds to a reactor WASI module with Zig 0.17.1 and
  wasm-opt 133.
- The artifact imports **zero** WASI functions and exports `memory`,
  `_initialize`, `qp_abi_version`, `qp_alloc`, `qp_free`, `qp_invoke`.
- Artifact: **195,053 bytes**, sha256
  `4af0bedc33b11101e88cfc4e42a6e4ae187b0db6f079afa3d6e9baa90c738aca`.
- **41 guest smoke checks** pass, covering real `print`, structured Python
  errors, `try/except`, fresh-instance isolation, async `gather` with
  out-of-order completion and partial resume, GC-pressure preservation of
  locals/`finally`, terminal `stalled`, nonterminal `awaiting_calls`,
  `sleep(0)` interleaving, positive-timer rejection, catchable unknown tool,
  memory/output limits, and no stdout leak.
- Root `go build`, `go vet`, and `go test` pass; the real CLI modes `all`,
  `async`, `async-cancel`, `stall`, and `sleep0` pass.
- Explicit cancellation with an unaffected overlapping execution: **3/3**.
- Host WASI import allowlist is **empty**; the guest imports zero WASI
  functions.
- Warm startup: **~0.193 ms mean** in the independent run.
- Suspended guest linear memory: **100 pages = 6,553,600 bytes per instance**
  (exact for the measured instances).
- Process RSS: approximately **11.5–11.8 MiB per instance** in the independent
  run. This is a process-level, approximate measurement; the spike CLI's own
  run reported ~13.1–13.6 MiB per instance.

`docs/SPEC.md` and `guest/M0_NOTES.md` are owned by separate status-doc workers
and may lag this handoff until their final updates land. Re-read them before
quoting status, but do not treat a stale “pending” line as a new blocker
without re-running the evidence.

## 6. Immediate next steps for a new agent (start at M1)

1. Confirm the handoff PR is merged and `main` contains the expected source
   commit (see §9). Do not assume the branch is still current.
2. Run the clean-clone build and verification commands in §4 to confirm the
   accepted M0 baseline on the new machine.
3. Start **M1 — SDK MVP**:
   - implement the public Go contract (`Runtime`, `RegisterTool`, `Execute`,
     `Execution.Wait/Cancel`, `Close`, events, results, errors),
   - value and protocol conformance suites,
   - execution semantics, async tools, lifecycle, limits, and stall detection,
   - state commit-only-on-success and output ordering/truncation,
   - security assertions (default-deny imports, no host FS/network, no
     stdout/stderr leak),
   - a normative supported `asyncio` subset and failure matrix,
   - call-ledger conformance and the 6.6.1 resource/trap matrix.
4. Keep the M0 evidence reproducible; do not regress the accepted baseline.
5. Only after M1/M2 should pi-go integration begin.

## 7. Known hard requirements and limitations

- **WASM exception handling:** wazero must enable
  `experimental.CoreFeaturesExceptionHandling`. The artifact contains one
  required EH tag for MicroPython `nlr`. Without it, compilation fails.
- **Zero WASI imports (current build):** the guest imports no WASI functions.
  The host is default-deny. If a future build adds imports, each must be
  justified and allowlisted explicitly.
- **Forced cancellation while running:** while a guest entry is active,
  cancellation is host-led via wazero `CloseOnContextDone`. The host must not
  call another guest export or free buffers after the forced close, and must
  wait for the active entry to return before releasing the instance.
- **`quickpy.run` semantics:** submit-and-drive-to-quiescence. It must be the
  final top-level statement. There is no top-level `await` and no top-level
  `return`. Statements after `quickpy.run(...)` run before pending tools finish.
- **Serialized guest entry:** concurrent async tools are dispatched by the
  host, but only the owner re-enters the guest. No concurrent export entry.
- **No host capabilities:** no filesystem, network, environment, or process
  access. No OS process isolation; WASM is a memory/capability boundary.
- **Trusted tools:** tool implementations are trusted. A malicious trusted tool
  can do anything the host permits.
- **Narrow frozen modules:** `math`, `uos`, and `gc` are not available in the
  current guest build. `json`, `time` (for loud timer rejection), `sys`, and
  `io.StringIO` are available.
- **Memory cap scope:** `MaxMemoryBytes` is applied at the wazero runtime level,
  not per instance (wazero API limitation).
- **Timers:** positive `asyncio.sleep` and `time.sleep` raise a catchable
  `UnsupportedError`. Only `asyncio.sleep(0)` is a supported yield.
- **Unstable protocol/API:** the guest protocol and Go API are pre-1.0 and may
  change.

## 8. Repository gotchas

- `guest/smoke` is a **separate Go module**. Root `go test ./...` does not run
  it. Use `cd guest/smoke && go run . ../build/quickpy.wasm`.
- `guest/build/` and `guest/.cache/` are gitignored. The MicroPython checkout is
  fetched at the pinned commit, not vendored.
- `internal/wasmguest/testdata/*.wasm` are **intentional synthetic fixtures**,
  not the real guest. Do not delete them or add a blanket `*.wasm` ignore.
- The real guest artifact is `guest/build/quickpy.wasm` (post-`wasm-opt
  --translate-to-exnref`), not the intermediate `guest/build/obj/quickpy.wasm`.
- The host must not instantiate `wasi_snapshot_preview1` for the current
  artifact.
- `guest/build.sh` requires network access on first run and builds a host
  `mpy-cross` binary as a manifest prerequisite.

## 9. Git workflow and handoff status

At the start of the handoff, the local repository was **completely unborn**:
no commits, no refs, and all project files untracked. The remote
`https://github.com/keejkrej/quickpy.git` was public and empty.

Bootstrap performed:

- `main` contains exactly one commit, `chore: bootstrap repository ignore
  rules`, adding only `.gitignore` and `.gitattributes`. It contains **no
  QuickPy source**.
- The handoff branch is `handoff/m0-baseline`, created from that bootstrap
  commit.
- Final packaging is prepared but **not yet committed** at the time this
  document was written. The source handoff PR is **not yet opened**; the
  orchestrator will commit, open, and merge it after the status-doc workers
  finish.

Fill in after the PR exists:

- PR: _pending_
- Base: `main`
- Head: `handoff/m0-baseline`
- Merge commit: _pending_

Staging must use explicit paths, never `git add -A`. Exclude `guest/build/`,
`guest/.cache/`, logs, temp files, and local binaries. The synthetic
`internal/wasmguest/testdata/*.wasm` fixtures are intentional and must be
included.

## 10. Third-party and licensing status

No project license has been chosen. Original QuickPy code is not licensed by
the third-party notices file. Third-party components, provenance, and required
notices are recorded in [`../THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

Key points:

- MicroPython v1.29.0 is MIT licensed and is fetched at build time, not
  vendored.
- `guest/port/setjmp_rt.c` is adapted from wasi-libc's
  `libc-top-half/musl/src/setjmp/wasm32/rt.c`; wasi-libc is multi-licensed and
  the musl-derived portion is MIT. The musl `COPYRIGHT` notice is reproduced in
  `THIRD_PARTY_NOTICES.md` §2.
- The frozen `asyncio` modules are copied/adapted from MicroPython v1.29.0
  (MIT).
- Go dependencies: wazero (Apache-2.0) and `golang.org/x/sys` (BSD-3-Clause).

Before publishing a release, choose a project license and confirm the notices
file is complete.
