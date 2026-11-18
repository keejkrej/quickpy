// Package quickpy provides an embeddable Python execution runtime backed by a
// WASM guest. It is the public Go SDK described in docs/SPEC.md.
package quickpy

import (
	"context"
	"time"
)

// Value is a JSON-like value: nil, bool, int64, float64, string, []Value, or
// map[string]Value. It is not a generic escape hatch for arbitrary Go values.
type Value any

// ArtifactSource identifies the compiled guest artifact. Set exactly one of
// Path or Bytes.
type ArtifactSource struct {
	Path  string
	Bytes []byte
}

// RuntimeOptions configures a Runtime.
//
// MaxMemoryBytes and MaxExecutions are required and must be positive. Zero
// numeric limits select the documented defaults. MaxToolCalls == 0 explicitly
// means no tool-call cap; set it to a positive value to enable the cap.
//
// MaxMemoryBytes is the only enforced memory cap. The current engine cannot
// apply a tighter per-execution memory limit, so ExecuteRequest.MaxMemoryBytes
// must be zero (inherit) or exactly equal to this value.
type RuntimeOptions struct {
	Artifact ArtifactSource

	MaxMemoryBytes int64
	MaxExecutions  int

	DefaultTimeout time.Duration

	MaxSourceBytes         int
	MaxMessageBytes        int
	MaxOutputBytes         int
	MaxToolResultBytes     int
	MaxStateBytes          int
	MaxToolCalls           int
	MaxConcurrentToolCalls int
	MaxPendingCalls        int
	MaxResumes             int

	EventSink func(Event)
}

// ExecuteRequest is one execution request.
//
// A zero per-execution limit inherits the runtime value. Positive values may
// only tighten the runtime ceiling. MaxMemoryBytes is the exception: the
// current engine enforces only the runtime-level cap, so a nonzero
// MaxMemoryBytes must equal RuntimeOptions.MaxMemoryBytes. Any other value is
// rejected with InvalidRequest before admission.
type ExecuteRequest struct {
	Source       string
	Filename     string
	InitialState Value
	Timeout      time.Duration

	MaxMemoryBytes int64
	MaxOutputBytes int
	MaxToolCalls   int

	EventSink func(Event)
}

// ToolHandler is a host capability callable from the guest.
type ToolHandler func(ctx context.Context, call ToolCall) (Value, error)

// ToolCall is one guest tool request.
type ToolCall struct {
	Name        string
	Args        Value
	ExecutionID string
	Deadline    time.Time
}

// ToolError is a structured, guest-safe tool failure.
type ToolError struct {
	Code    string
	Message string
}

func (e *ToolError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

// Status is the terminal status of an execution.
type Status string

const (
	StatusCompleted     Status = "completed"
	StatusFailed        Status = "failed"
	StatusCancelled     Status = "cancelled"
	StatusTimedOut      Status = "timed_out"
	StatusLimitExceeded Status = "limit_exceeded"
	StatusStalled       Status = "stalled"
)

// Result is the terminal outcome of one execution.
type Result struct {
	ExecutionID  string
	Status       Status
	Outputs      []Event
	State        Value
	StateWritten bool
	ToolCalls    int
	Duration     time.Duration
	Guest        GuestInfo
}

// HasState reports whether the execution committed a state write.
func (r Result) HasState() bool { return r.StateWritten }

// GuestInfo identifies the guest engine for one execution.
//
// Engine and Version come from the ABI 0.4 guest identity metadata. If an
// artifact omits the version, Version is "unknown" rather than blank.
type GuestInfo struct {
	Engine          string
	Version         string
	ProtocolVersion string
	ABIVersion      uint32
	Imports         []Import
}

// Import is one artifact import discovered at compile time.
type Import struct {
	Kind   string
	Module string
	Name   string
}
