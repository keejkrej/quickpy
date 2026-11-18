package wasmguest

import "fmt"

// Kind classifies a host-side failure. The set is intentionally small for the
// M0 spike; it mirrors the terminal kinds in docs/SPEC.md closely enough to
// adapt later without exposing a public SDK.
type Kind string

const (
	KindInvalidRequest Kind = "invalid_request"
	KindProtocolError  Kind = "protocol_error"
	KindScriptError    Kind = "script_error"
	KindGuestFault     Kind = "guest_fault"
	KindLimitExceeded  Kind = "limit_exceeded"
	KindCancelled      Kind = "cancelled"
	KindTimedOut       Kind = "timed_out"
	KindStalled        Kind = "stalled"
	KindHostFault      Kind = "host_fault"
	KindRuntimeClosed  Kind = "runtime_closed"
)

// Status is the terminal status reported for an execution.
type Status string

const (
	StatusCompleted     Status = "completed"
	StatusFailed        Status = "failed"
	StatusCancelled     Status = "cancelled"
	StatusTimedOut      Status = "timed_out"
	StatusStalled       Status = "stalled"
	StatusLimitExceeded Status = "limit_exceeded"
	StatusGuestFault    Status = "guest_fault"
	StatusProtocolError Status = "protocol_error"
)

// Error is the spike's structured host error. It is deliberately not the
// public QuickPy *Error from the specification.
type Error struct {
	Kind          Kind
	Status        Status
	ExecutionID   string
	Op            string
	Message       string
	Cause         error
	Exception     *Exception
	ToolCause     *ToolCause
	UnknownFields []string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Message
	if msg == "" && e.Cause != nil {
		msg = e.Cause.Error()
	}
	if e.ExecutionID != "" {
		return fmt.Sprintf("%s (%s, execution %s): %s", e.Kind, e.Op, e.ExecutionID, msg)
	}
	return fmt.Sprintf("%s (%s): %s", e.Kind, e.Op, msg)
}

func (e *Error) Unwrap() error { return e.Cause }

func newError(kind Kind, status Status, op, execID, msg string, cause error) *Error {
	return &Error{
		Kind:        kind,
		Status:      status,
		Op:          op,
		ExecutionID: execID,
		Message:     msg,
		Cause:       cause,
	}
}
