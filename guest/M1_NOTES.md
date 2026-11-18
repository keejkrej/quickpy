# QuickPy M1 guest state slice

**Status:** guest-side explicit persistence, ABI3 tool conformance, engine
identity, and ABI4 structured output (console/image) are implemented and
verified against the real artifact. This note covers only `guest/**`; the Go
host still has to consume the protocol below. It does not change the M0 status
history in `guest/M0_NOTES.md`.

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
- `qp_abi_version()` is now packed **0.4 = integer 4**. ABI4 adds the ordered
  structured `outputs` array (text/console/image) and the dedicated
  `quickpy.console` / `quickpy.image` guest APIs. The envelope stays `v:0` and
  every ABI3 field is unchanged. An ABI3 host would ignore `outputs` and lose
  console/image output, so this host must require ABI4 and reject older
  versions.
- ABI3 (still present) added the mandatory `max_tool_calls` start field,
  distinct catchable tool error classes, structured uncaught-tool causes, and
  engine identity. An ABI2 guest would silently ignore `max_tool_calls` and run
  without the cap, so the host must reject it too.
- `quickpy.ToolError` is the base class for `ToolNotFoundError`,
  `ToolArgumentError`, and `ToolLimitError`. `except quickpy.ToolError` catches
  all of them.
- The guest enforces the total tool-call cap before allocating or registering a
  host call, so a blocked call never reaches the host.
- An uncaught `ToolError` adds `error.tool_cause` to the failed response so the
  host can populate `Error.Tool` without parsing traceback text.
- Every completed, failed, and suspended response carries
  `engine_name:"micropython"` and `engine_version:"1.29.0"` (derived from the
  pinned MicroPython `MICROPY_VERSION_STRING` macro).
- `quickpy.console.debug/info/warn/error(*args)` emits one ordered console
  event per call. `quickpy.image(data, mime="image/png")` emits one ordered
  image event and returns `None`. Both are recorded in the C-side event list
  and serialized as the ABI4 `outputs` array; the legacy `output` string keeps
  text only.
- `bytearray` is enabled so `quickpy.image` can accept it. Image bytes are
  copied at call time and base64-encoded without any text decoding.
- The guest heap was raised from 1 MiB to 4 MiB and `MICROPY_LONGINT_IMPL` was
  set to `LONGLONG` so int64 state values and a 1 MiB state snapshot fit. No
  WASI imports were added.

## Transient protocol extension

### ABI gate

- `qp_abi_version()` returns **4** (packed major/minor 0.4).
- The JSON protocol envelope stays `v:0`; the ABI integer is the capability
  gate for M1 state, ABI3 tool conformance, and ABI4 structured output.
- The host must require exactly `4` at instance start. A 0.3 artifact would
  accept the start message but silently drop `outputs` (console/image), and a
  0.2 artifact would silently ignore `max_tool_calls`; both must be rejected
  with a protocol/ABI error instead of running with degraded semantics.

### Start request

Existing starts remain valid. The ABI3 fields are:

```json
{"v":0,"type":"start","source":"...","filename":"...","initial_state":<JSON-like>,"max_tool_calls":<int>}
```

- `initial_state` absent or `null` means the initial Python value is `None`.
- `max_tool_calls` is **required** on every ABI3 start. `-1` means unlimited;
  `>= 0` is an inclusive cap on the total number of host calls the execution may
  request (`0` forbids all calls). The host resolves the public zero-unlimited
  value to `-1` before sending.
- A start that is missing `max_tool_calls`, has a non-integer value, or has a
  value below `-1` fails with `error.type:"ProtocolError"` and the source does
  not run. The guest never silently defaults the cap.
- The guest parses the raw JSON value independently, validates it with the same
  rules as `set_state`, and copies it before any source runs. Host pointers and
  host objects are never passed in.
- An invalid `initial_state` fails the start with a `failed` response whose
  `error.type` is `StateError`; the source does not run.
- Loading initial state is **not** a state write. `state_written` stays false
  unless the script calls `set_state`.

### Completed response

A successful completed response always carries `state_written` and the engine
identity:

```json
{"v":0,"type":"completed","engine_name":"micropython","engine_version":"1.29.0","output":"...","outputs":[...],"state_written":false}
```

```json
{"v":0,"type":"completed","engine_name":"micropython","engine_version":"1.29.0","output":"...","outputs":[...],"state_written":true,"state":<JSON-like>}
```

```json
{"v":0,"type":"completed","engine_name":"micropython","engine_version":"1.29.0","output":"...","outputs":[...],"state_written":true,"state":null}
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

### Failed response and uncaught tool cause

A failed response carries the engine identity and the usual `error` object. If
the uncaught exception is a `quickpy.ToolError` (or subclass), the error object
also carries a structured cause:

```json
{"v":0,"type":"failed","engine_name":"micropython","engine_version":"1.29.0","output":"...","outputs":[...],
 "error":{"type":"ToolNotFoundError","message":"...","traceback":"...",
          "tool_cause":{"code":"tool_not_found","message":"unknown tool \"x\"","tool":"x","call_id":1}}}
```

- `error.tool_cause.code` and `error.tool_cause.message` are always present.
- `error.tool_cause.tool` is present when the failure is tied to a tool name.
- `error.tool_cause.call_id` is present when the failure is tied to a
  dispatched call. It is absent for failures raised before dispatch (for
  example positional-argument validation).
- A non-tool exception (for example `ValueError`) never carries `tool_cause`.
- A caught tool failure completes normally and never carries a public cause.
- The cause is read from the exception's own `code`, `message`, `tool`, and
  `call_id` attributes; the guest never parses the traceback string.

### Suspended response

Suspended responses carry the engine identity and never carry state fields:

```json
{"v":0,"type":"suspended","reason":"awaiting_calls","engine_name":"micropython","engine_version":"1.29.0","calls":[...],"awaiting":[...],"output":"...","outputs":[...]}
```

### Failed, suspended, stalled, cancelled

These responses do **not** include `state_written` or `state`. The host must
not commit state for them. If a future implementation adds the fields to other
response types, the host must still ignore them unless `type` is `completed`.

## Structured output (ABI4)

Every `completed`, `failed`, and `suspended` response carries an ordered
`outputs` array. The array is always present; an execution with no output emits
`"outputs":[]`. A missing array and `[]` are equivalent, but this guest always
emits the field.

```json
"outputs":[
  {"type":"text","text":"hello\n"},
  {"type":"console","level":"warn","text":"careful"},
  {"type":"image","mime":"image/png","data_base64":"iVBORw0KGgo="}
]
```

- `text` entries are guest `print`/stdout. They are never parsed for control
  sentinels: a `print()` whose text looks like a console or image message stays
  a text entry.
- `console` entries come only from `quickpy.console.*`. `level` is exactly one
  of `debug`, `info`, `warn`, `error`.
- `image` entries come only from `quickpy.image()`. `mime` is the validated
  MIME type and `data_base64` is standard RFC 4648 base64 (with padding) of the
  raw bytes. NUL bytes and invalid UTF-8 survive exactly; the bytes are never
  decoded as a Python `str`.
- Ordering is the guest's emission order. Adjacent text chunks may coalesce
  into one `text` entry. A console or image entry always starts a new record,
  so pending text is flushed before the next kind. The host must not reorder or
  merge entries by timestamp.
- Output is a **per-response delta**. A later `resume`/`tool_responses`
  invocation never replays output from an earlier response. The host appends
  each response's `outputs` to the execution's ordered output list exactly
  once.

### Legacy `output` field

The response also keeps the legacy `output` string for old CLI trace
compatibility. It is the concatenation of `text` entries only; console and
image entries are never folded into it.

- The host **must prefer `outputs` when present** and must not concatenate the
  legacy `output` string again. Doing both would double-count text.
- When `outputs` is absent (an older guest), the host may fall back to
  `output` as a single text entry.
- `output_truncated:true` means the guest dropped or clipped text/console
  output at its own cap. The host must still enforce its exact
  `MaxOutputBytes`/`MaxMessageBytes`.

### Python API

```python
quickpy.console.debug(*args)   # one console event, level "debug"
quickpy.console.info(*args)    # one console event, level "info"
quickpy.console.warn(*args)    # one console event, level "warn"
quickpy.console.error(*args)   # one console event, level "error"

quickpy.image(data, mime="image/png")  # one image event, returns None
```

- Console arguments are stringified with `str()` and joined with single
  spaces, matching `print()` (without the trailing newline). One call emits
  exactly one console event. `quickpy.console.info()` emits an empty-text
  event.
- `quickpy.image` accepts `bytes` or `bytearray` only. The bytes are copied at
  call time, so mutating a `bytearray` afterwards cannot change the emitted
  image. It returns `None`.
- `mime` must be a non-empty `token/token` value: printable ASCII token
  characters plus exactly one `/`, no whitespace, control characters,
  parameters, or non-ASCII. The default is `image/png`.
- Errors are raised before any event is committed:
  - `TypeError` for non-`bytes`/`bytearray` data or a non-`str` MIME.
  - `ValueError` for an invalid MIME, an image over the per-image cap, an image
    that would exceed the total image cap, or the output-event cap.
- A caught error leaves the output list unchanged and the execution can
  continue. An uncaught error fails the run normally; state is not committed.

### Guest caps

These are compile-time guest safety limits. The host must enforce its exact
limits independently; a stricter host limit is fine.

| Cap | Value | Behaviour |
|---|---|---|
| text + console text | 1 MiB | clipped, `output_truncated:true` |
| per image raw bytes | 1 MiB | `ValueError`, nothing committed |
| total image raw bytes per response | 1 MiB | `ValueError`, nothing committed |
| output events per response | 1024 | text/console dropped with `output_truncated:true`; image raises `ValueError` |

A maximal response is roughly 1 MiB of text plus 1.33 MiB of base64 image data
plus the envelope. The host's `MaxMessageBytes` must be large enough to carry
that (or the host must set a stricter `MaxOutputBytes` and expect
`LimitExceeded`). The default 1 MiB `MaxMessageBytes` is **not** enough for a
maximal ABI4 response.

## Tool error classes and the call cap

`quickpy.ToolError` is the base class. The subclasses are:

| Class | Code | Cause |
|---|---|---|
| `ToolNotFoundError` | `tool_not_found` | host reported an unregistered tool |
| `ToolArgumentError` | `tool_argument` | positional args, unsupported/cyclic/oversized/deep values, or a host argument rejection |
| `ToolLimitError` | `tool_limit` | the total tool-call cap would be exceeded |

- `except quickpy.ToolError` catches every subclass.
- `ToolError` exposes `code`, `message`, `tool`, and `call_id`. `tool` and
  `call_id` are `None` when the failure is not tied to a dispatched call.
- The cap is checked in `_make_call` before `json.dumps`, before the call id is
  allocated, and before `_quickpy.register_call`. A blocked call produces no
  host request and no handler invocation.
- Argument validation runs before dispatch. Unsupported types, cycles, depth
  over 64, non-string keys, non-finite floats, int64 overflow, and a 1 MiB
  estimated size raise `ToolArgumentError` and never produce a partial host
  call.
- Unknown host error codes remain the base `ToolError`; the host's other limits
  remain hard backstops for malicious guests.

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

- size: **236,119 bytes**
- sha256: `171502b0e072d03402672f75ea7c35e651341518d0f452c533995c7106372184`
- ABI: **4** (packed 0.4)
- engine: `micropython` / `1.29.0`
- imports: **0** (`grep -c '(import' guest/build/quickpy.wat` → `0`)
- tag sections: **1** (`grep -c '(tag' guest/build/quickpy.wat` → `1`)
- memory: initial **64 pages / 4 MiB**, maximum **1024 pages / 64 MiB**
  (`(memory $0 64 1024)`); MicroPython heap **4 MiB**
- exports: `memory`, `_initialize`, `qp_abi_version`, `qp_alloc`, `qp_free`,
  `qp_invoke`
- reproducibility: a clean rebuild (`rm -rf guest/build && ./guest/build.sh`)
  produced the same size and sha256.
- `bytearray` is now enabled (`MICROPY_PY_BUILTINS_BYTEARRAY`) so
  `quickpy.image` can accept it. No `array` or `memoryview` surface is added.

## Commands and tests

```sh
./guest/build.sh
cd guest/smoke
go run . ../build/quickpy.wasm
```

Result: **118 checks pass** (41 M0 checks plus 77 M1 checks). The ABI check now
expects 4 and rejects 0.3, 0.2, and 0.1. The ABI4 structured-output checks are:

- console `debug`/`info`/`warn`/`error` levels and exact text
- console argument stringification, including an exception argument and an
  empty argument list
- ordering `print` → console → binary image → `print`
- legacy `output` contains text only and equals the concatenated text entries
- adjacent text coalesces into one text entry
- text flushes into a new entry across a console entry
- binary image bytes (NUL, 0xFF, quotes, backslash) round-trip exactly through
  base64
- all 256 byte values round-trip exactly
- `bytearray` is copied at call time; later mutation does not change the image
- default and custom MIME types
- empty image bytes and the 128/129-character MIME length boundary
- invalid data types and invalid MIME values (empty, space, newline, non-ASCII,
  no slash, extra slash) raise before emitting
- per-image oversize and total-image-cap rejection
- a `print` that looks like a console/image sentinel stays a text entry
- async output deltas across two suspended tool calls: no replay, no
  duplication, and the final completed response carries only its own delta
- failed responses carry their partial ordered outputs
- an execution with no output emits an empty `outputs` array
- console/image output does not affect state commit

The ABI3 conformance checks are:

- ABI3 start missing `max_tool_calls` is rejected
- ABI3 start with a non-integer or out-of-range `max_tool_calls` is rejected
- engine identity (`micropython` / `1.29.0`) on completed, failed, and
  suspended responses
- `ToolNotFoundError`, `ToolArgumentError`, and `ToolLimitError` are subclasses
  of `ToolError`, which is an `Exception`
- host `tool_not_found` maps to `ToolNotFoundError` and is catchable as the base
  `ToolError`
- positional arguments raise `ToolArgumentError` with no host call
- unsupported argument values raise `ToolArgumentError` with no host call
- over-deep arguments raise `ToolArgumentError` with no host call
- cap `0` blocks every call with no host call
- cap `1` dispatches the first call and blocks the second with no host call
- `-1` is unlimited and a two-call gather completes
- an uncaught tool failure carries `error.tool_cause` with code, message, tool,
  and call id
- an uncaught `ToolNotFoundError` keeps its subclass type and cause code
- an uncaught argument error carries the tool name but no call id
- a `ToolError` raised in ordinary source carries a cause without tool/call id
- a non-tool `ValueError` carries no tool cause
- a caught tool failure completes with no public cause

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

### Harness ownership and RSS measurement

The smoke harness is a test driver, not the SDK. It now closes each scenario's
guest module before instantiating the next one, and closes the last module
before the shared wazero runtime. A final `harness closes every instance` check
asserts `created == closed` (currently 60/60). This is a harness-only change; the
guest artifact and ABI are unchanged.

Peak RSS measured with `/usr/bin/time -l` on macOS (arm64, Go 1.27.4, wazero
1.12.0), running the built smoke binary against the same artifact:

| Harness | Peak RSS |
|---|---|
| before per-scenario close (leaked 60 modules) | ~1.32 GiB (1,414,791,168 B) |
| after per-scenario close | ~69-72 MiB (72,138,752-75,137,024 B) |

The absolute numbers depend on the OS, Go runtime, wazero version, and machine;
treat them as a relative baseline, not a product guarantee. The guest's own
per-instance linear memory is 4 MiB initial / 64 MiB maximum with a 4 MiB
MicroPython heap. No guest-side or runtime leak was observed beyond the harness
retaining modules; the drop is entirely explained by closing modules that the
harness previously kept alive.

## Host TODO

1. **Require ABI 4.** Check `qp_abi_version() == 4` at instance start and
   reject 0.3, 0.2, and 0.1 with a protocol/ABI error. Do not run ABI4 against
   an older artifact; a 0.3 guest would silently drop `outputs`, and a 0.2
   guest would silently ignore `max_tool_calls`.
2. Add `initial_state` to the start message from `ExecuteRequest.InitialState`.
   Omit it or send `null` for no initial state.
3. Add `max_tool_calls` to **every** start. Resolve the public zero-unlimited
   value to `-1`; send `>= 0` as the inclusive cap. The guest rejects a start
   that is missing the field or sends a non-integer or a value below `-1`.
4. On a `completed` response, read `state_written`. If true, commit `state`
   (including `null`); if false, set `State=nil, StateWritten=false`.
5. On every non-completed terminal result, ignore any state fields and set
   `State=nil, StateWritten=false`.
6. Enforce the host's `MaxStateBytes` on the committed value. The guest cap is
   1 MiB; a stricter host cap is fine. A state that exceeds the host cap should
   be `LimitExceeded` and must not be committed. Decode state numbers so int64
   stays int64 and does not silently become float64 (for example
   `json.Decoder.UseNumber` plus an explicit int64/float64 split).
7. Preserve float64 identity in the host→guest direction. Encode float64 `1.0`
   as `1.0`, not `1`; Go's `encoding/json` collapses it to `1`, which the guest
   parses as an integer. Use a custom encoder, `json.Number`, or
   `strconv.FormatFloat` with a decimal point.
8. Ensure `MaxMessageBytes` can carry the state plus the response envelope, or
   set `MaxStateBytes` low enough that it can. A 1 MiB state plus envelope can
   exceed a 1 MiB message cap.
9. Do not treat initial state as a write. Do not expose a mutable
   `quickpy.state` attribute.
10. On a `failed` response, decode `error.tool_cause` into `Error.Tool`:
    `code` and `message` are always present; `tool` and `call_id` are optional.
    Do not parse the traceback string. A non-tool exception has no
    `tool_cause`.
11. Read `engine_name` and `engine_version` from every response and populate
    `GuestInfo.Engine` and `GuestInfo.Version`. The guest reports
    `micropython` / `1.29.0`; the version is derived from the pinned
    MicroPython `MICROPY_VERSION_STRING` macro.
12. Keep the host's own `MaxToolCalls` enforcement as a hard backstop. The
    guest cap is cooperative; a malicious guest could bypass it.
13. Decode the `outputs` array on every `completed`, `failed`, and `suspended`
    response. Map `text` → `TextEvent`, `console` → `ConsoleEvent` (level is
    one of `debug`/`info`/`warn`/`error`), and `image` → `ImageEvent` (decode
    `data_base64` to raw bytes; do not treat it as text). Preserve array order.
14. **Prefer `outputs` when present.** Do not also append the legacy `output`
    string; that would double-count text. Fall back to `output` only when
    `outputs` is absent (older guest).
15. Treat a missing `outputs` array and `[]` as equivalent. This guest always
    emits the field, including on failed and suspended responses.
16. Append each response's `outputs` to the execution's ordered output list
    exactly once. A later resume's `outputs` is a delta, not a replay.
17. Enforce the host's exact `MaxOutputBytes` across text+console+image and
    reject a response that exceeds `MaxMessageBytes`. The guest caps are
    cooperative; a maximal ABI4 response is roughly 2.4 MiB, so the default
    1 MiB `MaxMessageBytes` is not enough. Either raise `MaxMessageBytes` or
    set a stricter `MaxOutputBytes` and expect `LimitExceeded`.
18. Reject a malformed `outputs` entry (unknown `type`, missing `text`/`level`/
    `mime`/`data_base64`, invalid base64, or a non-string field) as a
    `ProtocolError`. Do not silently drop it.

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
- ABI 0.4 is the capability gate. The protocol envelope remains `v:0`; a 0.3
  artifact would accept the start message but drop `outputs`, and a 0.2
  artifact would ignore `max_tool_calls`, so the host must reject both.
- The guest output caps are compile-time (1 MiB text+console, 1 MiB per image,
  1 MiB total image, 1024 events). The host cannot configure them without a
  guest rebuild; it can enforce stricter limits.
- A maximal ABI4 response is roughly 2.4 MiB (1 MiB text + 1.33 MiB base64
  image + envelope). The host's `MaxMessageBytes` must allow it, or the host
  must set a stricter `MaxOutputBytes` and expect `LimitExceeded`.
- `quickpy.image` accepts only `bytes`/`bytearray`. `memoryview`, `array`, and
  other buffer objects are rejected with `TypeError`; arbitrary binary tool
  payloads remain unsupported.
- MIME types are validated as `token/token` only. Parameters such as
  `image/png; charset=binary` are rejected; the guest does not parse or
  normalize MIME parameters.
- Console text and image bytes are copied into the guest's C-side event list
  (outside the MicroPython GC heap). A 1 MiB image therefore costs roughly
  1 MiB of linear memory in addition to the Python object.
- The guest tool-call cap is cooperative. The host must keep its own
  `MaxToolCalls` enforcement as a hard backstop for a malicious guest.
- The guest argument size cap is a compile-time 1 MiB estimate. The host must
  still enforce its exact `MaxMessageBytes` and tool-result limits.
- `error.tool_cause` is only emitted for an uncaught `quickpy.ToolError`. A
  non-tool exception has no cause, and a caught tool failure completes normally.
- The guest size check is an estimate during validation and a `len(text)` check
  at serialization. The host must enforce the exact byte limit.
- `state` is raw JSON embedded in the completed response. The host must parse
  it as a JSON value, not as a string.
- The guest does not implement cooperative abort; the M0 host-led forced
  cancellation path is unchanged. A cancelled execution never emits state.
