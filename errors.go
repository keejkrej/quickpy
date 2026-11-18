package quickpy

import (
	"errors"
	"fmt"
)

// Kind classifies a QuickPy error.
type Kind string

const (
	KindInvalidRequest   Kind = "invalid_request"
	KindCapacityExceeded Kind = "capacity_exceeded"
	KindReentrant        Kind = "reentrant"
	KindScriptError      Kind = "script_error"
	KindTimedOut         Kind = "timed_out"
	KindCancelled        Kind = "cancelled"
	KindLimitExceeded    Kind = "limit_exceeded"
	KindStalled          Kind = "stalled"
	KindGuestFault       Kind = "guest_fault"
	KindHostFault        Kind = "host_fault"
	KindProtocolError    Kind = "protocol_error"
	KindRuntimeClosed    Kind = "runtime_closed"
)

// Error is the structured error returned by Execute and Wait.
type Error struct {
	Kind        Kind
	Status      Status
	ExecutionID string
	Op          string
	Message     string
	Cause       error
	Script      *ScriptErrorInfo
	Tool        *ToolCause
	Diagnostics map[string]string
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
		return fmt.Sprintf("quickpy: %s (%s, execution %s): %s", e.Kind, e.Op, e.ExecutionID, msg)
	}
	return fmt.Sprintf("quickpy: %s (%s): %s", e.Kind, e.Op, msg)
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.Cause }

// ScriptErrorInfo describes an uncaught guest exception.
type ScriptErrorInfo struct {
	Type      string
	Message   string
	Traceback string
	Filename  string
	Line      int
}

// ToolCause describes an uncaught tool failure that ended a script.
type ToolCause struct {
	Tool    string
	Code    string
	Message string
}

// ErrWaitTimeout is returned by Wait when only the wait context expires. It
// wraps the context error and does not change execution state.
var ErrWaitTimeout = errors.New("quickpy: wait timeout")

func invalidRequest(op, msg string) *Error {
	return &Error{Kind: KindInvalidRequest, Op: op, Message: msg}
}

func statusForError(err error) Status {
	var qe *Error
	if errors.As(err, &qe) {
		if qe.Status != "" {
			return qe.Status
		}
		switch qe.Kind {
		case KindScriptError, KindGuestFault, KindHostFault, KindProtocolError:
			return StatusFailed
		case KindTimedOut:
			return StatusTimedOut
		case KindCancelled:
			return StatusCancelled
		case KindLimitExceeded:
			return StatusLimitExceeded
		case KindStalled:
			return StatusStalled
		}
	}
	return StatusFailed
}
