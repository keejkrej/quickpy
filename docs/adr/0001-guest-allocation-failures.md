# ADR-0001: Guest allocation failures stay Python errors

- **Status:** accepted
- **Date:** 2026-11-18

## Decision

We will keep guest allocation failures as Python-visible errors — a catchable
`MemoryError`, or `ScriptError` if uncaught — instead of reclassifying them as
host `LimitExceeded`, because wazero's `memory.grow` returns `-1` to the guest
rather than trapping and the host cannot reliably distinguish a configured
runtime memory-cap hit from another guest allocation failure.

## Why

The shipped behavior is:

- `memory.grow` beyond the configured runtime cap returns `-1` to the guest; it
  does not produce a host trap.
- The guest can catch the resulting `MemoryError` and continue.
- An uncaught `MemoryError` ends the execution as a public `ScriptError`, not
  `LimitExceeded`.
- Arbitrary out-of-bounds accesses and other traps remain `GuestFault`.
- Host compile/instantiate resource-limit errors remain `LimitExceeded`.

The guarantee is bounded guest memory, not uniform classification. The host
cannot attribute a guest-visible allocation failure to the configured cap with
enough confidence to blame the limit; doing so would misreport guest bugs and
could hide them. The rejected alternative was to map every out-of-bounds trap or
`MemoryError` to `LimitExceeded`; that assigns incorrect blame and weakens the
semantics of both kinds.

Covered by `TestMemoryGrowFailureIsScriptError`,
`TestArbitraryMemoryTrapIsGuestFault`, and
`TestCompileMemoryLimitIsLimitExceeded` in
`internal/wasmguest/runtime_test.go`.

Memory tightening is unaffected: per-execution `MaxMemoryBytes` is still
inherit-or-equal only, and a tighter value is still rejected with
`InvalidRequest`.

## Looks like a bug when

A report says a configured memory-cap hit should be `LimitExceeded` but is
`ScriptError`/`MemoryError`, or that an out-of-bounds trap is `GuestFault`
rather than `LimitExceeded`. That is the intended contract; cite this ADR. A
genuine bug is a host compile/instantiate memory-limit error that is not
`LimitExceeded`, or a guest allocation failure that crashes the host instead of
surfacing as a Python error.
