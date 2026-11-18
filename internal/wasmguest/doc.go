// Package wasmguest is the experimental Go/wazero host for the QuickPy M1
// low-level slice.
//
// It compiles a WASI reactor artifact once, instantiates a fresh module per
// execution, performs the transient ABI handshake (ABI 0.4, integer 4), sends
// one length-delimited JSON start message, dispatches host tool calls, and
// decodes completed/failed/suspended responses. It is deliberately not the
// public QuickPy SDK: the public package wraps this package and owns the
// public value, event, and error types.
//
// Values are encoded and decoded with a strict JSON-like codec shared by tool
// arguments, tool results, initial state, and final state. The codec preserves
// int64 versus float64, rejects unsupported Go types, cycles, excessive depth
// or node count, invalid UTF-8, unpaired surrogates, non-finite floats, and
// out-of-range integer tokens.
//
// Cancellation while qp_invoke is active is host-led. The host cancels the
// wazero call context (CloseOnContextDone), waits for the active entry to
// return, and does not call qp_free or any other guest export afterwards.
package wasmguest
