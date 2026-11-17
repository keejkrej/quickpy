// Package wasmguest is the experimental Go/wazero host for the QuickPy M0
// feasibility spike.
//
// It compiles a WASI reactor artifact once, instantiates a fresh module per
// execution, performs the transient ABI handshake, sends one length-delimited
// JSON start message, and decodes one completed/failed response. It is
// deliberately not the public QuickPy SDK: there is no tool dispatch, value
// codec, persistence, event sink, or conformance framework here.
//
// Cancellation while qp_invoke is active is host-led. The host cancels the
// wazero call context (CloseOnContextDone), waits for the active entry to
// return, and does not call qp_free or any other guest export afterwards.
package wasmguest
