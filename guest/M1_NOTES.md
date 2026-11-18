# QuickPy M1 guest state slice

**Status:** guest-side explicit persistence is implemented and verified against
the real artifact. This note covers only `guest/**`; the Go host still has to
consume the protocol below. It does not change the M0 status history in
`guest/M0_NOTES.md`.

## What changed

- `quickpy.get_state()` returns the current JSON-like state as a fresh deep
  copy. Mutating the returned value does not change stored state.
- `quickpy.set_state(value)` validates and takes a deep snapshot of a JSON-like
  value. Later mutation of the caller's Python object does not change stored
  state. The last successful call before completion wins.
- `quickpy.StateError` is a catchable structured exception with `code` and
  `message`. A failed `set_state` never partially overwrites the previous valid
  state.
- There is no `quickpy.state` attribute and no live VM snapshot.
- The guest keeps the pending value across suspensions and forced GC. It is a
  module global rooted through the existing `MP_REGISTER_ROOT_POINTER` for the
  `quickpy` module.
- Floats are now IEEE 754 binary64 (`MICROPY_FLOAT_IMPL_DOUBLE`), so state and
  tool values preserve float64 precision. `1.0` stays `1.0` on the wire.
- `qp_abi_version()` is now packed **0.2 = integer 2**. M1 state is an additive
  capability; old 0.1 artifacts silently ignore `initial_state`, so this host
  must require ABI2 and reject 0.1 rather than risk silent data loss.
- The guest heap was raised from 1 MiB to 4 MiB and `MICROPY_LONGINT_IMPL` was
  set to `LONGLONG` so int64 state values and a 1 MiB state snapshot fit. No
  WASI imports were added.

## Transient protocol extension

### ABI gate

- `qp_abi_version()` returns **2** (packed major/minor 0.2).
- The JSON protocol envelope stays `v:0`; the ABI integer is the capability
  gate for M1 state.
- The host must require exactly `2` at instance start. A 0.1 artifact would
  accept the start message but silently ignore `initial_state`, so it must be
  rejected with a protocol/ABI error instead of running with lost state.

### Start request

Existing starts remain valid. The optional field is:

```json
{"v":0,"type":"start","source":"...","filename":"...","initial_state":<JSON-like>}
```

- `initial_state` absent or `null` means the initial Python value is `None`.
- The guest parses the raw JSON value independently, validates it with the same
  rules as `set_state`, and copies it before any source runs. Host pointers and
  host objects are never passed in.
- An invalid `initial_state` fails the start with a `failed` response whose
  `error.type` is `StateError`; the source does not run.
- Loading initial state is **not** a state write. `state_written` stays false
  unless the script calls `set_state`.

### Completed response

A successful completed response always carries `state_written`:

```json
{"v":0,"type":"completed","output":"...","state_written":false}
```

```json
{"v":0,"type":"completed","output":"...","state_written":true,"state":<JSON-like>}
```

```json
{"v":0,"type":"completed","output":"...","state_written":true,"state":null}
```

- `state_written:false` means no successful `set_state` call. The `state` field
  is absent and the host must ignore it.
- `state_written:true` means at least one successful `set_state` call. `state`
  is the final current value. `set_state(None)` is `state_written:true` with
  `state:null`, which is distinct from no write.
- `state` is emitted only when the execution is genuinely completed. A source
  return with `quickpy.run` main still pending is a `suspended` response, not a
  completed one.
- The value is the last successful `set_state` before successful source and
  async main completion.
- A source that never calls `quickpy.run` is treated as completed (`idle`), so
  a top-level `set_state` is committed normally.

### Failed, suspended, stalled, cancelled

These responses do **not** include `state_written` or `state`. The host must
not commit state for them. If a future implementation adds the fields to other
response types, the host must still ignore them unless `type` is `completed`.

## State value rules

Accepted and normalized:

| Python | State value |
|---|---|
| `None` | `null` |
| `bool` | `true` / `false` |
| `int` in int64 range | integer |
| finite IEEE float64 | number (decimal/exponent; `1.0` stays `1.0`) |
| `str` (valid UTF-8, no lone surrogates) | string |
| `list` / `tuple` | list (tuples normalize to lists) |
| `dict` with string keys | object |

Rejected with `StateError`:

| Code | Cause |
|---|---|
| `unsupported_type` | bytes, bytearray, custom objects, sets, etc. |
| `cycle` | a list/dict contains itself directly or indirectly |
| `non_string_key` | dict key is not a `str` |
| `non_finite_float` | NaN or Infinity |
| `int_out_of_range` | integer outside int64 |
| `depth_exceeded` | nesting deeper than 64 |
| `size_exceeded` | estimated JSON size over 1 MiB, or guest memory exhaustion |
| `invalid_string` | lone UTF-16 surrogate in a string |
| `invalid_initial_state` | `initial_state` is not valid JSON |
| `serialization_failed` | final `json.dumps` failed |

`set_state` validates first and copies second. If either step fails, the
previous `_state_value` and `_state_written` are unchanged. `get_state` returns
a deep copy, so callers cannot mutate stored state without `set_state`. Shared
references are copied independently; the stored value has no aliasing.

### Float semantics

- The guest uses IEEE 754 binary64 (`MICROPY_FLOAT_IMPL_DOUBLE`). State floats
  and tool argument/result floats are float64, not float32.
- The JSON printer emits a shortest round-trip representation. Observed
  examples: `0.1` → `0.1`, `1.0` → `1.0`,
  `1.0000000000000002` → `1.0000000000000002`,
  `123456789.123456789` → `123456789.12345679` (which parses back to the same
  float64). The host must compare decoded float64 values, not raw strings.
- The guest JSON parser maps integer syntax to `int` and fractional/exponent
  syntax to `float`. The host must preserve the int64/float64 distinction in
  both directions.
- **Host encoding requirement:** Go's `encoding/json` marshals `float64(1.0)`
  as `1`, which the guest parses as an integer. When sending `initial_state` or
  tool results, the host must encode float64 `1.0` as `1.0` (for example with a
  custom encoder, `json.Number`, or `strconv.FormatFloat` plus a decimal point).
  The guest's own state output already preserves `1.0`.
- Tool arguments and results use the same float64 JSON channel as state. The
  host must preserve the int64/float64 distinction for tool payloads too, not
  just for state.

## Artifact evidence

Built with the pinned toolchain from `guest/build.sh` (Zig 0.17.1, wasm-opt
133, MicroPython v1.29.0 commit
`0fd6c573ea815774668bbb16b8e197c8822368b2`).

- size: **220,134 bytes**
- sha256: `be1b8e48b6884e3e9e2a59c758a2abceac564adb5162b691e5a6b6cd3eac9c4d`
- ABI: **2** (packed 0.2)
- imports: **0** (`grep -c '(import' guest/build/quickpy.wat` → `0`)
- tag sections: **1** (`grep -c '(tag' guest/build/quickpy.wat` → `1`)
- memory: initial **64 pages / 4 MiB**, maximum **1024 pages / 64 MiB**
  (`(memory $0 64 1024)`); MicroPython heap **4 MiB**
- exports: `memory`, `_initialize`, `qp_abi_version`, `qp_alloc`, `qp_free`,
  `qp_invoke`
- reproducibility: a clean rebuild (`rm -rf guest/build && ./guest/build.sh`)
  produced the same size and sha256.

## Commands and tests

```sh
./guest/build.sh
cd guest/smoke
go run . ../build/quickpy.wasm
```

Result: **66 checks pass** (41 M0 checks plus 25 M1 checks). The existing ABI
check now expects 2. The seven new checks are:

- legacy 0.1 ABI is rejected
- float64 neighboring doubles (`1.0` vs `1.0000000000000002`) stay distinct
- float64 non-trivial round-trip (`0.1`, `123456789.123456789`) matches host
  float64 equality and is not the float32 value
- float64 `1.0` stays `1.0` on the state wire
- initial-state integer/float syntax stays distinct (`1` vs `1.0`)
- tool float64 argument fidelity (guest→host)
- tool float64 result fidelity (host→guest)

The earlier M1 checks cover:

- no state write (`state_written:false`, no `state`)
- initial state read and deep-copy behavior
- explicit `set_state(None)` vs no write
- last-write-wins
- failed run not committable
- state across partial async resumes with forced `gc` on each resume
- suspended responses never carrying state fields
- deep-copy/mutation semantics for `set_state` and `get_state`
- tuple normalization
- int64 min/max round-trip
- finite float round-trip
- unicode string round-trip
- unsupported, cyclic, depth, size, non-string-key, NaN and Infinity rejection
  with the previous state preserved
- fresh instance has no previous state
- explicit `initial_state:null`
- invalid out-of-range `initial_state` rejected before source runs

## Host TODO

1. **Require ABI 2.** Check `qp_abi_version() == 2` at instance start and
   reject 0.1 with a protocol/ABI error. Do not run M1 state against a 0.1
   artifact; it would silently ignore `initial_state`.
2. Add `initial_state` to the start message from `ExecuteRequest.InitialState`.
   Omit it or send `null` for no initial state.
3. On a `completed` response, read `state_written`. If true, commit `state`
   (including `null`); if false, set `State=nil, StateWritten=false`.
4. On every non-completed terminal result, ignore any state fields and set
   `State=nil, StateWritten=false`.
5. Enforce the host's `MaxStateBytes` on the committed value. The guest cap is
   1 MiB; a stricter host cap is fine. A state that exceeds the host cap should
   be `LimitExceeded` and must not be committed. Decode state numbers so int64
   stays int64 and does not silently become float64 (for example
   `json.Decoder.UseNumber` plus an explicit int64/float64 split).
6. Preserve float64 identity in the host→guest direction. Encode float64 `1.0`
   as `1.0`, not `1`; Go's `encoding/json` collapses it to `1`, which the guest
   parses as an integer. Use a custom encoder, `json.Number`, or
   `strconv.FormatFloat` with a decimal point.
7. Ensure `MaxMessageBytes` can carry the state plus the response envelope, or
   set `MaxStateBytes` low enough that it can. A 1 MiB state plus envelope can
   exceed a 1 MiB message cap.
8. Do not treat initial state as a write. Do not expose a mutable
   `quickpy.state` attribute.

## Limitations and risks

- The guest state cap is a compile-time 1 MiB and depth 64. The host cannot
  configure these without a guest rebuild; it can enforce stricter limits.
- The guest heap is 4 MiB. Linear memory is initial 64 pages / 4 MiB, maximum
  1024 pages / 64 MiB. Per-instance memory and RSS are higher than the M0
  measurements; the host's `MaxMemoryBytes` must allow it. Re-measure before
  setting production defaults.
- `set_state` is O(n) validation plus a deep copy. Large states cost CPU and
  memory; `get_state` also copies.
- Floats are IEEE binary64. The guest JSON printer uses a shortest-round-trip
  representation; the host must compare decoded float64 values, not raw
  strings. The host must encode float64 `1.0` as `1.0` to preserve type
  identity.
- ABI 0.2 is the capability gate. The protocol envelope remains `v:0`; a 0.1
  artifact would accept the start message but ignore `initial_state`, so the
  host must reject it.
- The guest size check is an estimate during validation and a `len(text)` check
  at serialization. The host must enforce the exact byte limit.
- `state` is raw JSON embedded in the completed response. The host must parse
  it as a JSON value, not as a string.
- The guest does not implement cooperative abort; the M0 host-led forced
  cancellation path is unchanged. A cancelled execution never emits state.
