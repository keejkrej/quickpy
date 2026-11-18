# Agent target: a compiled Prime-inspired scripting agent

**Status:** proposed north star, after M1. This is not M1 scope, not a
commitment, and not a compatibility claim. QuickPy remains a generic execution
runtime; the agent application described here is a separate future consumer.

## 1. Primary-source inspection

Inspected `PrimeIntellect-ai/prime-agent` at `main` tree
`033eea1705c5ae953fe7cd7a5e7e7c45317e0dfe`, workspace version **0.10.5**.

It is a **Rust desktop/daemon plus CPython sidecar persistent RLM**, not a
simple Python application:

- The root `Cargo.toml` is a Rust workspace with `pa-daemon`, `pa-tui`,
  `pa-cli`, `pa-agent`, `pa-ai`, `pa-core`, `pa-types`, `pa-models`, and
  `pa-perf`.
- `prime-agent-runtime/` is a Python package (`requires-python >= 3.11`) that
  provides the kernel-side runtime shim under `src/rlm`.
- The README describes a persistent Python REPL, daemon-backed sessions,
  recursive subagents, and a continual harness.

Reference links:

- <https://github.com/PrimeIntellect-ai/prime-agent/tree/033eea1705c5ae953fe7cd7a5e7e7c45317e0dfe>
- <https://github.com/PrimeIntellect-ai/prime-agent/blob/033eea1705c5ae953fe7cd7a5e7e7c45317e0dfe/Cargo.toml>
- <https://github.com/PrimeIntellect-ai/prime-agent/blob/033eea1705c5ae953fe7cd7a5e7e7c45317e0dfe/prime-agent-runtime/pyproject.toml>
- <https://github.com/PrimeIntellect-ai/prime-agent/tree/033eea1705c5ae953fe7cd7a5e7e7c45317e0dfe/prime-agent-runtime/src/rlm>
- <https://github.com/PrimeIntellect-ai/prime-agent/blob/033eea1705c5ae953fe7cd7a5e7e7c45317e0dfe/README.md>

No full Prime compatibility is claimed. Persistent CPython variables and
imports, `rlm`, the daemon, and the continual harness are **non-goals** for the
M1 compiled-agent MVP.

## 2. Recommended shape

Build a **new lightweight Go application** that composes:

- `pi-go/agent` for the agent loop and tool scheduling,
- selected `pi-go/ai` provider code (not the full provider matrix),
- the QuickPy SDK for Python execution.

`pi-rs` and `pi-ts` are viable cores, but each would need a new SDK, runtime
lifecycle, and security test surface. They are not drop-in replacements for the
Go composition.

Reference implementations: <https://github.com/keejkrej/pi-go>,
<https://github.com/keejkrej/pi-rs>, <https://github.com/keejkrej/pi-ts>.

QuickPy stays generic. The application owns the `python_repl` tool, its prompt,
session policy, and workspace policy. A fresh namespace per call is acceptable
for the MVP; the model is closer to `python -c` than to a persistent kernel.

Do not claim arbitrary pip or CPython support. The supported module facade is
dictated by the acceptance corpus unless that corpus proves a broader surface.

## 3. Future execution profiles

Keep two profiles explicit and separate. Both are policy wrappers around
QuickPy, not a CPython backend.

| Profile | Default posture | Capabilities |
| --- | --- | --- |
| **CodeMode** | Default deny, fresh state per call | No ambient filesystem, process, environment, or network |
| **AgentScript** | Opt-in | Explicit cwd, filesystem, process, and environment grants |

No ambient root mounts in either profile. The host enforces path and security
policy; the guest does not decide it.

## 4. Guest capability bridge (proposed)

A shared guest-side adapter (Zig target) could expose a `node:fs` subset and a
Python `os` subset **inside the guest**, linked into the guest's capability
bridge rather than added as a native host addon.

- Calls bridge to the Go/WASI host; the final binary stays `CGO_ENABLED=0`.
- Python `os` blocking operations use synchronous imports.
- Async tools keep the existing host scheduling model.
- The host enforces path and security policy.
- No ambient root mounts.

This is a design direction, not implemented behavior. It needs protocol,
lifecycle, and security tests before it can be claimed.

## 5. Packaging and measurement

- `ArtifactSource.Bytes` currently writes a temporary file for compilation.
  `go:embed` is supported. A later internal bytes cache should avoid requiring
  a writable temp directory.
- CI should pin the exact guest ABI and artifact hash.
- Record, without guessed numbers: executable total installed footprint, cold
  `PyReady`, cold and warm RSS, and the same figures for Prime including its
  CPython sidecars.

## 6. Initial acceptance corpus

The compiled-agent MVP should be accepted against a corpus containing:

- JSON, text, CSV, and `re` workloads.
- Path/open wrapper policy cases.
- Temporary fixtures.
- Process arguments and timeouts.
- Actual sanitized user `python -c` snippets.
- Deny cases for escapes and network access.

## 7. Sequencing after M1 acceptance

Start with only:

1. the acceptance corpus,
2. packaging,
3. one agent loop with a deterministic provider.

Do not start the daemon, TUI, plugin system, or full provider matrix first.

## 8. Licensing caveat

- Original QuickPy code has no chosen project license.
- `pi-go` had no root license file found at inspection time; confirm before
  external distribution.
- `pi-rs` and `pi-ts` are MIT licensed.
- `prime-agent` is MIT licensed.

Re-check all licenses and notices before distributing a composed application.
