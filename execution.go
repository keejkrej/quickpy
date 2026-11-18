package quickpy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

var errExecutionCancelled = errors.New("quickpy: execution cancelled")

// Execution is one in-flight or finished execution.
type Execution struct {
	id      string
	runtime *Runtime
	ctx     context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}

	sink           func(Event)
	maxOutputBytes int
	maxStateBytes  int

	obsMu       sync.Mutex
	outputs     []Event
	outputBytes int
	latched     *Error
	obsDone     bool

	mu       sync.Mutex
	result   Result
	err      error
	finished bool
}

func newExecution(r *Runtime, id string, ctx context.Context, cancel context.CancelCauseFunc, sink func(Event), maxOutputBytes, maxStateBytes int) *Execution {
	return &Execution{
		id:             id,
		runtime:        r,
		ctx:            ctx,
		cancel:         cancel,
		done:           make(chan struct{}),
		sink:           sink,
		maxOutputBytes: maxOutputBytes,
		maxStateBytes:  maxStateBytes,
	}
}

// ID returns the stable execution identifier.
func (e *Execution) ID() string { return e.id }

// Cancel requests cancellation. It is idempotent and a no-op after completion.
func (e *Execution) Cancel() {
	select {
	case <-e.done:
		return
	default:
	}
	e.cancel(errExecutionCancelled)
}

// Wait waits for the execution to finish. The context bounds only the wait; it
// does not cancel the execution. If the context expires first, Wait returns a
// zero Result and ErrWaitTimeout wrapping the context error. A later Wait can
// still obtain the terminal result.
func (e *Execution) Wait(ctx context.Context) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-e.done:
		return e.terminal()
	default:
	}
	select {
	case <-e.done:
		return e.terminal()
	case <-ctx.Done():
		// Prefer a terminal result that raced with the wait deadline.
		select {
		case <-e.done:
			return e.terminal()
		default:
		}
		return Result{}, fmt.Errorf("%w: %w", ErrWaitTimeout, ctx.Err())
	}
}

func (e *Execution) terminal() (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := e.result
	res.Outputs = append([]Event(nil), e.result.Outputs...)
	return res, e.err
}

func (e *Execution) watchParent(parent context.Context) {
	select {
	case <-parent.Done():
		e.cancel(context.Cause(parent))
	case <-e.done:
	}
}

func (e *Execution) observe(obs observation) {
	e.obsMu.Lock()
	defer e.obsMu.Unlock()
	if e.obsDone {
		return
	}
	switch obs.Kind {
	case observationText:
		e.appendText(obs.Text, "")
	case observationConsole:
		e.appendText(obs.Text, obs.Level)
	case observationImage:
		e.appendImage(obs.MIME, obs.Data)
	case observationTool:
		e.deliver(ToolEvent{
			Phase:  obs.Phase,
			Name:   obs.ToolName,
			CallID: strconv.FormatInt(obs.CallID, 10),
			Error:  obs.Error,
		})
	}
}

func (e *Execution) appendText(text, level string) {
	if text == "" {
		return
	}
	if e.maxOutputBytes > 0 {
		remaining := e.maxOutputBytes - e.outputBytes
		if remaining <= 0 {
			e.latchLimit("output limit exceeded")
			return
		}
		if len(text) > remaining {
			text = text[:remaining]
			e.outputBytes = e.maxOutputBytes
			e.latchLimit("output limit exceeded")
		} else {
			e.outputBytes += len(text)
		}
	}
	var ev Event
	if level == "" {
		ev = TextEvent{Text: text}
	} else {
		ev = ConsoleEvent{Level: level, Text: text}
	}
	e.outputs = append(e.outputs, ev)
	e.deliver(ev)
}

func (e *Execution) appendImage(mime string, data []byte) {
	if e.maxOutputBytes > 0 {
		remaining := e.maxOutputBytes - e.outputBytes
		if remaining <= 0 {
			e.latchLimit("output limit exceeded")
			return
		}
		if len(data) > remaining {
			data = data[:remaining]
			e.outputBytes = e.maxOutputBytes
			e.latchLimit("output limit exceeded")
		} else {
			e.outputBytes += len(data)
		}
	}
	ev := ImageEvent{MIME: mime, Data: append([]byte(nil), data...)}
	e.outputs = append(e.outputs, ev)
	e.deliver(ev)
}

func (e *Execution) latchLimit(msg string) {
	if e.latched == nil {
		e.latched = &Error{Kind: KindLimitExceeded, Status: StatusLimitExceeded, ExecutionID: e.id, Op: "execute", Message: msg}
	}
	e.cancel(errExecutionCancelled)
}

func (e *Execution) deliver(ev Event) {
	if e.sink == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			if e.latched == nil {
				e.latched = &Error{Kind: KindHostFault, Status: StatusFailed, ExecutionID: e.id, Op: "execute", Message: fmt.Sprintf("event sink panic: %v", p)}
			}
			e.cancel(errExecutionCancelled)
		}
	}()
	e.sink(ev)
}

func (e *Execution) finish(res engineResult, err error) {
	e.obsMu.Lock()
	e.obsDone = true
	outputs := append([]Event(nil), e.outputs...)
	latched := e.latched
	e.obsMu.Unlock()

	if latched != nil {
		err = latched
		res.Status = latched.Status
	}

	if err == nil && res.StateWritten {
		size, serr := stateSize(res.State)
		if serr != nil {
			err = &Error{Kind: KindProtocolError, Status: StatusFailed, ExecutionID: e.id, Op: "execute", Message: "invalid state value: " + serr.Error()}
			res.State = nil
			res.StateWritten = false
		} else if e.maxStateBytes > 0 && size > e.maxStateBytes {
			err = &Error{Kind: KindLimitExceeded, Status: StatusLimitExceeded, ExecutionID: e.id, Op: "execute", Message: fmt.Sprintf("state exceeds %d bytes", e.maxStateBytes)}
			res.State = nil
			res.StateWritten = false
		}
	}

	status := res.Status
	if err != nil {
		var qe *Error
		if errors.As(err, &qe) && qe.ExecutionID == "" {
			qe.ExecutionID = e.id
		}
		res.State = nil
		res.StateWritten = false
		status = statusForError(err)
	}
	if status == "" {
		status = StatusCompleted
	}

	result := Result{
		ExecutionID:  e.id,
		Status:       status,
		Outputs:      outputs,
		State:        res.State,
		StateWritten: res.StateWritten,
		ToolCalls:    res.ToolCalls,
		Duration:     res.Duration,
		Guest:        res.Guest,
	}

	e.mu.Lock()
	if e.finished {
		e.mu.Unlock()
		return
	}
	e.finished = true
	e.result = result
	e.err = err
	// Capacity invariant: remove the execution from the runtime's in-flight
	// set before done becomes visible. A caller that observes done closed
	// (via Wait) must also observe the execution as no longer counted against
	// MaxExecutions, so an immediate Execute cannot spuriously return
	// CapacityExceeded. Holding e.mu across the removal also means terminal()
	// cannot return a result before the removal is complete.
	e.runtime.removeActive(e)
	e.mu.Unlock()
	close(e.done)
}
