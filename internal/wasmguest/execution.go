package wasmguest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ToolHandler is a host tool. It must not call into the guest. It may run
// concurrently with other handlers of the same execution.
type ToolHandler func(ctx context.Context, call ToolCall) (any, error)

// ToolCall is one guest tool request.
type ToolCall struct {
	Name        string
	Args        map[string]any
	ExecutionID string
	CallID      int64
	// Deadline is the execution deadline, or the zero time when the execution
	// has no deadline.
	Deadline time.Time
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

// CallState is the host ledger state for one guest call.
type CallState string

const (
	CallReceived      CallState = "received"
	CallDispatched    CallState = "dispatched"
	CallResponseReady CallState = "response_ready"
	CallDelivered     CallState = "delivered"
	CallCompleted     CallState = "completed"
)

// CallLedgerEntry is inspectable evidence for one call.
type CallLedgerEntry struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	State      CallState `json:"state"`
	Dispatched bool      `json:"dispatched"`
	Completed  bool      `json:"completed"`
	Delivered  bool      `json:"delivered"`
	Error      string    `json:"error,omitempty"`
}

type callState struct {
	id         int64
	name       string
	args       map[string]any
	state      CallState
	dispatched bool
	completed  bool
	delivered  bool
	errorCode  string
	response   *toolResponse
	ctx        context.Context
	cancel     context.CancelFunc
}

type toolResult struct {
	id       int64
	value    any
	err      error
	panicked bool
}

type toolResponse struct {
	ID    int64           `json:"id"`
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error *toolErrorBody  `json:"error,omitempty"`
}

type toolErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type resumeMessage struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

type toolResponsesMessage struct {
	V         int            `json:"v"`
	Type      string         `json:"type"`
	Responses []toolResponse `json:"responses"`
}

type executionOwner struct {
	rt     *Runtime
	req    Request
	result *Result

	inst     api.Module
	mem      api.Memory
	allocFn  api.Function
	freeFn   api.Function
	invokeFn api.Function
	callCtx  context.Context
	stdout   *boundedBuffer
	stderr   *boundedBuffer

	tools              map[string]ToolHandler
	maxConcurrent      int
	maxToolCalls       int
	maxPendingCalls    int
	maxResumes         int
	maxToolResultBytes int
	maxOutputBytes     int
	maxStateBytes      int
	initialState       json.RawMessage

	calls           map[int64]*callState
	tombstones      map[int64]bool
	awaiting        map[int64]bool
	receivedOrder   []int64
	dispatchOrder   []int64
	completionOrder []int64
	deliveredOrder  []int64
	requests        []CallRequest
	resumeCount     int
	toolCalls       int
	active          int
	totalOutput     int
	outputLimitHit  bool
	observerFailed  bool
	resultCh        chan toolResult
	trace           []string
}

// Execute runs one synchronous execution with no host tools. It is kept as the
// existing synchronous entry point; ExecuteWithTools is the async-capable one.
func (r *Runtime) Execute(ctx context.Context, req Request) (*Result, error) {
	req.Tools = nil
	res, werr := r.execute(ctx, req)
	if werr != nil {
		return res, werr
	}
	return res, nil
}

// ExecuteWithTools runs one execution and dispatches guest tool requests to the
// supplied handlers. The same instance is reused for resumes; the source is
// entered exactly once.
func (r *Runtime) ExecuteWithTools(ctx context.Context, req Request) (*Result, error) {
	res, werr := r.execute(ctx, req)
	if werr != nil {
		return res, werr
	}
	return res, nil
}

func (r *Runtime) execute(ctx context.Context, req Request) (result *Result, werr *Error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, newError(KindRuntimeClosed, "", "execute", "", "runtime is closed", nil)
	}
	if req.Filename == "" {
		return nil, newError(KindInvalidRequest, "", "execute", "", "filename is required", nil)
	}
	if len(req.Source) > r.opts.MaxMessageBytes {
		return nil, newError(KindInvalidRequest, "", "execute", "", fmt.Sprintf("source exceeds %d bytes", r.opts.MaxMessageBytes), nil)
	}

	maxOutputBytes := req.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = r.opts.MaxOutputBytes
	}
	maxStateBytes := req.MaxStateBytes
	if maxStateBytes <= 0 {
		maxStateBytes = r.opts.MaxStateBytes
	}
	if maxStateBytes <= 0 {
		maxStateBytes = r.opts.MaxMessageBytes
	}
	maxConcurrent := req.MaxConcurrentToolCalls
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	maxToolCalls := req.MaxToolCalls
	if maxToolCalls == 0 {
		// The low-level API keeps the M0 default finite cap for callers that
		// do not set a value (notably the CLI). The public SDK resolves its
		// own zero ("no cap") to -1 before reaching this layer.
		maxToolCalls = 1000
	} else if maxToolCalls < 0 {
		maxToolCalls = -1
	}
	maxPendingCalls := req.MaxPendingCalls
	if maxPendingCalls <= 0 {
		maxPendingCalls = 64
	}
	maxResumes := req.MaxResumes
	if maxResumes <= 0 {
		maxResumes = 10000
	}
	maxToolResultBytes := req.MaxToolResultBytes
	if maxToolResultBytes <= 0 {
		maxToolResultBytes = 1 << 20
	}

	execID := fmt.Sprintf("exec-%d", r.nextExec.Add(1))
	instID := fmt.Sprintf("inst-%d", r.nextInst.Add(1))
	start := time.Now()
	stdout := newBoundedBuffer(maxOutputBytes)
	stderr := newBoundedBuffer(maxOutputBytes)

	result = &Result{
		ExecutionID: execID,
		InstanceID:  instID,
		Guest:       GuestInfo{Imports: r.Imports()},
	}
	var owner *executionOwner
	defer func() {
		result.CapturedStdout = stdout.String()
		result.CapturedStderr = stderr.String()
		result.StdoutTruncated = stdout.Truncated()
		result.StderrTruncated = stderr.Truncated()
		result.Duration = time.Since(start)
		result.DurationMS = float64(result.Duration.Microseconds()) / 1000.0
		if owner != nil {
			result.Requests = owner.requests
			result.DispatchOrder = owner.dispatchOrder
			result.CompletionOrder = owner.completionOrder
			result.DeliveredOrder = owner.deliveredOrder
			result.ResumeCount = owner.resumeCount
			result.ToolCalls = owner.toolCalls
			result.CallLedger = owner.ledger()
			result.Trace = owner.trace
		}
		if p := recover(); p != nil {
			result.Status = StatusGuestFault
			werr = newError(KindHostFault, StatusGuestFault, "execute", execID, fmt.Sprintf("host panic: %v", p), nil)
		}
		if werr != nil && result.Status == "" {
			result.Status = werr.Status
		}
	}()

	initialState, ierr := encodeInitialState(req.InitialState, maxStateBytes, execID)
	if ierr != nil {
		return result, ierr
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = r.opts.DefaultTimeout
	}
	execCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	callCtx := execCtx
	if timeout > 0 {
		var cancelTimeout context.CancelFunc
		callCtx, cancelTimeout = context.WithTimeoutCause(execCtx, timeout, context.DeadlineExceeded)
		defer cancelTimeout()
	}

	modCfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions().
		WithStdout(stdout).
		WithStderr(stderr).
		WithStdin(strings.NewReader(""))

	inst, instErr := r.rt.InstantiateModule(callCtx, r.compiled, modCfg)
	if instErr != nil {
		if cause := context.Cause(callCtx); cause != nil {
			return result, classifyCause(cause, execID)
		}
		if isMemoryLimitError(instErr) {
			return result, newError(KindLimitExceeded, StatusLimitExceeded, "instantiate", execID, "instantiate failed: "+instErr.Error(), instErr)
		}
		return result, newError(KindGuestFault, StatusGuestFault, "instantiate", execID, "instantiate failed: "+instErr.Error(), instErr)
	}
	defer func() { _ = inst.Close(context.Background()) }()

	if initFn := inst.ExportedFunction("_initialize"); initFn != nil {
		result.ExportCalls.Initialize++
		if _, ierr := initFn.Call(callCtx); ierr != nil {
			if cause := context.Cause(callCtx); cause != nil {
				return result, classifyCause(cause, execID)
			}
			return result, newError(KindGuestFault, StatusGuestFault, "initialize", execID, "_initialize trapped: "+ierr.Error(), ierr)
		}
	}

	abiFn := inst.ExportedFunction("qp_abi_version")
	if abiFn == nil {
		return result, protocolErr(execID, "missing qp_abi_version export")
	}
	result.ExportCalls.ABI++
	abiRes, aerr := abiFn.Call(callCtx)
	if aerr != nil {
		if cause := context.Cause(callCtx); cause != nil {
			return result, classifyCause(cause, execID)
		}
		return result, newError(KindGuestFault, StatusGuestFault, "abi", execID, "qp_abi_version trapped: "+aerr.Error(), aerr)
	}
	version := uint32(abiRes[0])
	result.Guest.ABIVersion = version
	if version != abiVersion {
		return result, protocolErr(execID, fmt.Sprintf("unsupported ABI version %d (want %d = %s)", version, abiVersion, abiVersionText))
	}
	result.Guest.ProtocolVersion = protocolVersion

	mem := inst.Memory()
	if mem == nil {
		return result, protocolErr(execID, "missing exported memory")
	}
	result.MemoryPages = mem.Size() / 65536

	allocFn := inst.ExportedFunction("qp_alloc")
	freeFn := inst.ExportedFunction("qp_free")
	invokeFn := inst.ExportedFunction("qp_invoke")
	if allocFn == nil || freeFn == nil || invokeFn == nil {
		return result, protocolErr(execID, "missing qp_alloc, qp_free, or qp_invoke export")
	}

	tools := make(map[string]ToolHandler, len(req.Tools))
	for name, handler := range req.Tools {
		tools[name] = handler
	}

	owner = &executionOwner{
		rt:                 r,
		req:                req,
		result:             result,
		inst:               inst,
		mem:                mem,
		allocFn:            allocFn,
		freeFn:             freeFn,
		invokeFn:           invokeFn,
		callCtx:            callCtx,
		stdout:             stdout,
		stderr:             stderr,
		tools:              tools,
		maxConcurrent:      maxConcurrent,
		maxToolCalls:       maxToolCalls,
		maxPendingCalls:    maxPendingCalls,
		maxResumes:         maxResumes,
		maxToolResultBytes: maxToolResultBytes,
		maxOutputBytes:     maxOutputBytes,
		maxStateBytes:      maxStateBytes,
		initialState:       initialState,
		calls:              map[int64]*callState{},
		tombstones:         map[int64]bool{},
		awaiting:           map[int64]bool{},
		resultCh:           make(chan toolResult, maxConcurrent),
	}

	resp, oerr := owner.invoke(startMessage{
		V:            0,
		Type:         "start",
		Source:       req.Source,
		Filename:     req.Filename,
		InitialState: initialState,
		MaxToolCalls: maxToolCalls,
	})
	if oerr != nil {
		owner.cancelAll()
		return result, oerr
	}

	for {
		if ierr := owner.accumulateOutput(resp); ierr != nil {
			owner.cancelAll()
			return result, ierr
		}

		switch resp.Type {
		case "completed":
			owner.finishAll()
			if ierr := owner.checkOutputLimits(); ierr != nil {
				return result, ierr
			}
			if ierr := owner.commitState(resp); ierr != nil {
				return result, ierr
			}
			result.Status = StatusCompleted
			return result, nil

		case "failed":
			owner.finishAll()
			result.Status = StatusFailed
			result.Exception = resp.Exception
			var toolCause *ToolCause
			if resp.Exception != nil {
				toolCause = resp.Exception.ToolCause
			}
			return result, &Error{
				Kind:          KindScriptError,
				Status:        StatusFailed,
				ExecutionID:   execID,
				Op:            "execute",
				Message:       "guest reported failure",
				Exception:     resp.Exception,
				ToolCause:     toolCause,
				UnknownFields: resp.UnknownFields,
			}

		case "suspended":
			if ierr := owner.validateSuspension(resp); ierr != nil {
				owner.cancelAll()
				return result, ierr
			}
			if resp.Reason == "stalled" {
				owner.finishAll()
				result.Status = StatusStalled
				return result, &Error{
					Kind:          KindStalled,
					Status:        StatusStalled,
					ExecutionID:   execID,
					Op:            "execute",
					Message:       "guest reported a terminal stalled run",
					UnknownFields: resp.UnknownFields,
				}
			}
			if derr := owner.dispatchQueued(); derr != nil {
				owner.cancelAll()
				return result, derr
			}

			if resp.Reason == "runnable" {
				owner.resumeCount++
				if owner.resumeCount > owner.maxResumes {
					owner.cancelAll()
					return result, newError(KindLimitExceeded, StatusLimitExceeded, "resume", execID, fmt.Sprintf("resume count exceeds %d", owner.maxResumes), nil)
				}
				resp, oerr = owner.invoke(resumeMessage{V: 0, Type: "resume"})
				if oerr != nil {
					owner.cancelAll()
					return result, oerr
				}
				continue
			}

			for !owner.hasReady() {
				select {
				case tr := <-owner.resultCh:
					if ierr := owner.processResult(tr); ierr != nil {
						owner.cancelAll()
						return result, ierr
					}
				case <-owner.callCtx.Done():
					owner.cancelAll()
					return result, classifyCause(context.Cause(owner.callCtx), execID)
				}
			}

			responses := owner.takeReadyResponses()
			if len(responses) == 0 {
				owner.cancelAll()
				return result, protocolErr(execID, "no ready tool responses to deliver")
			}
			owner.resumeCount++
			if owner.resumeCount > owner.maxResumes {
				owner.cancelAll()
				return result, newError(KindLimitExceeded, StatusLimitExceeded, "resume", execID, fmt.Sprintf("resume count exceeds %d", owner.maxResumes), nil)
			}
			resp, oerr = owner.invoke(toolResponsesMessage{V: 0, Type: "tool_responses", Responses: responses})
			if oerr != nil {
				owner.cancelAll()
				return result, oerr
			}
			continue

		default:
			owner.cancelAll()
			return result, protocolErr(execID, fmt.Sprintf("unexpected response type %q", resp.Type))
		}
	}
}

// encodeInitialState validates and encodes the initial state strictly. Nil is
// encoded as JSON null (Python None). The returned error is InvalidRequest for
// an unsupported value and LimitExceeded for an oversized encoding.
func encodeInitialState(initial any, maxStateBytes int, execID string) (json.RawMessage, *Error) {
	if initial == nil {
		return json.RawMessage("null"), nil
	}
	b, err := encodeValue(initial)
	if err != nil {
		return nil, newError(KindInvalidRequest, "", "execute", execID, "invalid initial state: "+err.Error(), err)
	}
	if len(b) > maxStateBytes {
		return nil, newError(KindLimitExceeded, StatusLimitExceeded, "execute", execID, fmt.Sprintf("initial state exceeds %d bytes", maxStateBytes), nil)
	}
	return json.RawMessage(b), nil
}

func (o *executionOwner) invoke(msg any) (*Response, *Error) {
	input, err := json.Marshal(msg)
	if err != nil {
		return nil, newError(KindHostFault, StatusGuestFault, "encode", o.result.ExecutionID, "encode request: "+err.Error(), err)
	}
	if len(input) > o.rt.opts.MaxMessageBytes {
		return nil, newError(KindLimitExceeded, StatusLimitExceeded, "encode", o.result.ExecutionID, fmt.Sprintf("request exceeds %d bytes", o.rt.opts.MaxMessageBytes), nil)
	}
	o.trace = append(o.trace, "request: "+truncateString(string(input), 4096))

	o.result.ExportCalls.Alloc++
	allocRes, aerr := o.allocFn.Call(o.callCtx, uint64(len(input)))
	if aerr != nil {
		if cause := context.Cause(o.callCtx); cause != nil {
			return nil, classifyCause(cause, o.result.ExecutionID)
		}
		return nil, newError(KindGuestFault, StatusGuestFault, "alloc", o.result.ExecutionID, "qp_alloc trapped: "+aerr.Error(), aerr)
	}
	inputPtr := uint32(allocRes[0])
	if inputPtr == 0 && len(input) > 0 {
		return nil, newError(KindGuestFault, StatusGuestFault, "alloc", o.result.ExecutionID, "qp_alloc returned null", nil)
	}
	if !o.mem.Write(inputPtr, input) {
		return nil, newError(KindGuestFault, StatusGuestFault, "alloc", o.result.ExecutionID, "qp_alloc returned an out-of-bounds pointer", nil)
	}

	o.result.ExportCalls.Invoke++
	invokeRes, ierr := o.invokeFn.Call(o.callCtx, uint64(inputPtr), uint64(len(input)))
	o.result.InvokeReturned = true

	if cause := context.Cause(o.callCtx); cause != nil {
		o.result.SkippedFreeOnCancel = true
		return nil, classifyCause(cause, o.result.ExecutionID)
	}
	if ierr != nil {
		// A trap is not attributable to a configured limit from the host
		// side. wazero's memory.grow returns -1 to the guest instead of
		// trapping, so a configured runtime memory cap hit is guest-visible
		// (usually MemoryError) and is ScriptError if uncaught. Arbitrary
		// out-of-bounds traps stay GuestFault; do not classify them as
		// LimitExceeded.
		return nil, newError(KindGuestFault, StatusGuestFault, "invoke", o.result.ExecutionID, "qp_invoke trapped: "+ierr.Error(), ierr)
	}

	raw := invokeRes[0]
	if raw == math.MaxUint64 {
		if ferr := o.freeGuest(inputPtr, uint32(len(input))); ferr != nil {
			return nil, newError(KindGuestFault, StatusGuestFault, "free", o.result.ExecutionID, "qp_free(input) failed: "+ferr.Error(), ferr)
		}
		return nil, newError(KindGuestFault, StatusGuestFault, "invoke", o.result.ExecutionID, "qp_invoke returned failure sentinel (-1)", nil)
	}

	respPtr := uint32(raw >> 32)
	respLen := uint32(raw & 0xffffffff)
	if respLen > uint32(o.rt.opts.MaxMessageBytes) {
		_ = o.freeGuest(inputPtr, uint32(len(input)))
		return nil, newError(KindLimitExceeded, StatusLimitExceeded, "invoke", o.result.ExecutionID, fmt.Sprintf("guest response length %d exceeds %d bytes", respLen, o.rt.opts.MaxMessageBytes), nil)
	}

	var respCopy []byte
	if respLen > 0 {
		respBytes, ok := o.mem.Read(respPtr, respLen)
		if !ok {
			_ = o.freeGuest(inputPtr, uint32(len(input)))
			return nil, newError(KindProtocolError, StatusProtocolError, "invoke", o.result.ExecutionID, "guest response pointer/length is out of bounds", nil)
		}
		respCopy = append([]byte(nil), respBytes...)
	}

	if ferr := o.freeGuest(respPtr, respLen); ferr != nil {
		return nil, newError(KindGuestFault, StatusGuestFault, "free", o.result.ExecutionID, "qp_free(response) failed: "+ferr.Error(), ferr)
	}
	if ferr := o.freeGuest(inputPtr, uint32(len(input))); ferr != nil {
		return nil, newError(KindGuestFault, StatusGuestFault, "free", o.result.ExecutionID, "qp_free(input) failed: "+ferr.Error(), ferr)
	}

	resp, derr := decodeResponseWithMessageLimit(respCopy, o.rt.opts.MaxMessageBytes)
	if derr != nil {
		derr.ExecutionID = o.result.ExecutionID
		return nil, derr
	}
	if resp.EngineName != "" {
		o.result.Guest.Engine = resp.EngineName
	}
	if resp.EngineVersion != "" {
		o.result.Guest.Version = resp.EngineVersion
	}
	o.trace = append(o.trace, "response: "+resp.Type)
	if resp.Exception != nil {
		o.trace = append(o.trace, "response_error: "+resp.Exception.Type+": "+resp.Exception.Message)
	}
	o.result.MemoryPages = o.mem.Size() / 65536
	return resp, nil
}

func (o *executionOwner) freeGuest(ptr, length uint32) error {
	if context.Cause(o.callCtx) != nil {
		o.result.SkippedFreeOnCancel = true
		return nil
	}
	o.result.ExportCalls.Free++
	_, err := o.freeFn.Call(o.callCtx, uint64(ptr), uint64(length))
	return err
}

// accumulateOutput stores and observes output deltas. It clips each delta to
// the remaining cumulative cap before storing or observing it, so the retained
// result prefix and the observer stream never exceed the resolved cap. Text and
// console deltas are counted by their UTF-8 text bytes; image deltas are
// counted by their raw decoded bytes. The first clipped delta makes the
// execution fail deterministically with LimitExceeded; the retained prefix
// stays available on the result.
//
// A single image larger than the total cap is clipped to a raw binary prefix
// exactly like an image that no longer fits after earlier output. The decoder
// only enforces a message-derived allocation bound; the public output cap is
// applied here, before the observer or result sees the bytes.
func (o *executionOwner) accumulateOutput(resp *Response) *Error {
	for _, out := range resp.Outputs {
		switch out.Type {
		case "text":
			if out.Text == "" {
				continue
			}
			text := out.Text
			if o.totalOutput >= o.maxOutputBytes {
				o.markOutputLimit()
				continue
			}
			remaining := o.maxOutputBytes - o.totalOutput
			if len(text) > remaining {
				text = clipUTF8(text, remaining)
				o.markOutputLimit()
			}
			if text == "" && out.Text != "" {
				continue
			}
			stored := out
			stored.Text = text
			o.result.Outputs = append(o.result.Outputs, stored)
			o.totalOutput += len(text)
			if err := o.emitOutput(stored); err != nil {
				return err
			}
		case "console":
			text := out.Text
			if o.totalOutput >= o.maxOutputBytes {
				o.markOutputLimit()
				continue
			}
			remaining := o.maxOutputBytes - o.totalOutput
			if len(text) > remaining {
				text = clipUTF8(text, remaining)
				o.markOutputLimit()
			}
			stored := out
			stored.Text = text
			o.result.Outputs = append(o.result.Outputs, stored)
			o.totalOutput += len(text)
			if err := o.emitOutput(stored); err != nil {
				return err
			}
		case "image":
			data := out.Data
			if o.totalOutput >= o.maxOutputBytes {
				o.markOutputLimit()
				continue
			}
			remaining := o.maxOutputBytes - o.totalOutput
			if len(data) > remaining {
				data = data[:remaining]
				o.markOutputLimit()
			}
			stored := out
			stored.Data = append([]byte(nil), data...)
			o.result.Outputs = append(o.result.Outputs, stored)
			o.totalOutput += len(data)
			if err := o.emitOutput(stored); err != nil {
				return err
			}
		}
	}
	o.result.UnknownFields = append(o.result.UnknownFields, resp.UnknownFields...)
	if resp.OutputTruncated {
		o.result.OutputTruncated = true
	}
	if resp.Stalled {
		o.result.Stalled = true
	}
	if o.outputLimitHit {
		return newError(KindLimitExceeded, StatusLimitExceeded, "output", o.result.ExecutionID, fmt.Sprintf("total output exceeds %d bytes", o.maxOutputBytes), nil)
	}
	return nil
}

func (o *executionOwner) markOutputLimit() {
	o.result.OutputTruncated = true
	o.outputLimitHit = true
}

func (o *executionOwner) checkOutputLimits() *Error {
	if o.result.OutputTruncated {
		return newError(KindLimitExceeded, StatusLimitExceeded, "output", o.result.ExecutionID, "guest reported truncated output", nil)
	}
	if o.stdout.Truncated() || o.stderr.Truncated() {
		return newError(KindLimitExceeded, StatusLimitExceeded, "output", o.result.ExecutionID, fmt.Sprintf("captured output exceeded %d bytes", o.maxOutputBytes), nil)
	}
	return nil
}

// commitState decodes and commits the final state of a completed response. It
// is only called on the completed path; failed, suspended, stalled, cancelled,
// and limited executions never commit state.
func (o *executionOwner) commitState(resp *Response) *Error {
	if !resp.StateWritten {
		return nil
	}
	if !resp.StatePresent {
		return protocolErr(o.result.ExecutionID, "completed response with state_written:true is missing state")
	}
	if len(resp.StateRaw) > o.maxStateBytes {
		return newError(KindLimitExceeded, StatusLimitExceeded, "state", o.result.ExecutionID, fmt.Sprintf("state exceeds %d bytes", o.maxStateBytes), nil)
	}
	val, err := decodeValue(resp.StateRaw)
	if err != nil {
		return newError(KindProtocolError, StatusProtocolError, "state", o.result.ExecutionID, "invalid final state: "+err.Error(), err)
	}
	o.result.State = val
	o.result.StateWritten = true
	return nil
}

func (o *executionOwner) validateSuspension(resp *Response) *Error {
	if resp.Reason == "stalled" {
		if !resp.Stalled {
			return protocolErr(o.result.ExecutionID, "stalled suspension requires stalled:true")
		}
		if len(resp.Calls) != 0 {
			return protocolErr(o.result.ExecutionID, "stalled suspension must not include new calls")
		}
		if len(resp.Awaiting) != 0 {
			return protocolErr(o.result.ExecutionID, "stalled suspension must have empty awaiting")
		}
		for id, c := range o.calls {
			if c.state != CallDelivered && c.state != CallCompleted {
				return protocolErr(o.result.ExecutionID, fmt.Sprintf("stalled suspension with pending call %d", id))
			}
		}
		return nil
	}

	if resp.Reason == "runnable" {
		if len(resp.Calls) != 0 {
			return protocolErr(o.result.ExecutionID, "runnable suspension must not include new calls")
		}
		if len(resp.Awaiting) != 0 {
			return protocolErr(o.result.ExecutionID, "runnable suspension must have empty awaiting")
		}
		for id, c := range o.calls {
			if c.state != CallDelivered && c.state != CallCompleted {
				return protocolErr(o.result.ExecutionID, fmt.Sprintf("runnable suspension with pending call %d", id))
			}
		}
		return nil
	}

	newAwaiting := map[int64]bool{}
	for _, id := range resp.Awaiting {
		newAwaiting[id] = true
	}

	for _, call := range resp.Calls {
		if o.calls[call.ID] != nil || o.tombstones[call.ID] {
			return protocolErr(o.result.ExecutionID, fmt.Sprintf("duplicate guest call id %d", call.ID))
		}
		if !newAwaiting[call.ID] {
			return protocolErr(o.result.ExecutionID, fmt.Sprintf("new call %d is not listed in awaiting", call.ID))
		}
		if o.maxToolCalls >= 0 && o.toolCalls >= o.maxToolCalls {
			return newError(KindLimitExceeded, StatusLimitExceeded, "tool", o.result.ExecutionID, fmt.Sprintf("tool call count exceeds %d", o.maxToolCalls), nil)
		}
		if o.pendingCount() >= o.maxPendingCalls {
			return newError(KindLimitExceeded, StatusLimitExceeded, "tool", o.result.ExecutionID, fmt.Sprintf("pending call count exceeds %d", o.maxPendingCalls), nil)
		}
		c := &callState{id: call.ID, name: call.Name, args: call.Args, state: CallReceived}
		o.calls[call.ID] = c
		o.receivedOrder = append(o.receivedOrder, call.ID)
		o.requests = append(o.requests, call)
		o.toolCalls++
	}

	for _, id := range resp.Awaiting {
		if o.tombstones[id] {
			return protocolErr(o.result.ExecutionID, fmt.Sprintf("awaiting call %d was already delivered or completed", id))
		}
		c := o.calls[id]
		if c == nil {
			return protocolErr(o.result.ExecutionID, fmt.Sprintf("awaiting call %d is unknown", id))
		}
		if c.state == CallDelivered || c.state == CallCompleted {
			return protocolErr(o.result.ExecutionID, fmt.Sprintf("awaiting call %d is already %s", id, c.state))
		}
	}

	for id, c := range o.calls {
		if c.state == CallDelivered || c.state == CallCompleted {
			continue
		}
		if !newAwaiting[id] {
			return protocolErr(o.result.ExecutionID, fmt.Sprintf("guest dropped awaiting call %d", id))
		}
	}

	for id, c := range o.calls {
		if c.state == CallDelivered && !newAwaiting[id] {
			c.state = CallCompleted
			c.completed = true
		}
	}

	o.awaiting = newAwaiting
	return nil
}

// pendingCount counts calls that have not been delivered to the guest. It
// excludes delivered and completed calls so the pending-call cap measures
// actual outstanding work rather than the lifetime call count. Duplicate-ID
// guards and tombstones remain in force.
func (o *executionOwner) pendingCount() int {
	n := 0
	for _, c := range o.calls {
		if c.state != CallDelivered && c.state != CallCompleted {
			n++
		}
	}
	return n
}

func (o *executionOwner) dispatchQueued() *Error {
	for _, id := range o.receivedOrder {
		c := o.calls[id]
		if c == nil || c.state != CallReceived {
			continue
		}
		if o.active >= o.maxConcurrent {
			break
		}
		handler, ok := o.tools[c.name]
		if !ok {
			c.state = CallResponseReady
			c.completed = true
			c.errorCode = "tool_not_found"
			c.response = &toolResponse{
				ID: c.id,
				OK: false,
				Error: &toolErrorBody{
					Code:    "tool_not_found",
					Message: fmt.Sprintf("unknown tool %q", c.name),
				},
			}
			o.completionOrder = append(o.completionOrder, c.id)
			if err := o.emitTool(c, "finished", "tool_not_found"); err != nil {
				return err
			}
			continue
		}
		c.state = CallDispatched
		c.dispatched = true
		c.ctx, c.cancel = context.WithCancel(o.callCtx)
		o.dispatchOrder = append(o.dispatchOrder, c.id)
		o.active++
		if err := o.emitTool(c, "started", ""); err != nil {
			return err
		}
		o.startHandler(c, handler)
	}
	return nil
}

func (o *executionOwner) startHandler(c *callState, handler ToolHandler) {
	deadline, _ := o.callCtx.Deadline()
	go func() {
		var res toolResult
		defer func() {
			if p := recover(); p != nil {
				res = toolResult{id: c.id, err: fmt.Errorf("tool handler panic: %v", p), panicked: true}
			}
			o.resultCh <- res
		}()
		value, err := handler(c.ctx, ToolCall{
			Name:        c.name,
			Args:        c.args,
			ExecutionID: o.result.ExecutionID,
			CallID:      c.id,
			Deadline:    deadline,
		})
		res = toolResult{id: c.id, value: value, err: err}
	}()
}

func (o *executionOwner) processResult(tr toolResult) *Error {
	c := o.calls[tr.id]
	if c == nil {
		return newError(KindHostFault, StatusGuestFault, "tool", o.result.ExecutionID, fmt.Sprintf("result for unknown call %d", tr.id), nil)
	}
	if c.state != CallDispatched {
		return newError(KindHostFault, StatusGuestFault, "tool", o.result.ExecutionID, fmt.Sprintf("duplicate result for call %d in state %s", tr.id, c.state), nil)
	}
	o.active--
	c.state = CallResponseReady
	c.completed = true
	o.completionOrder = append(o.completionOrder, c.id)

	if tr.panicked {
		c.errorCode = "handler_panic"
		if err := o.emitTool(c, "finished", "handler_panic"); err != nil {
			return err
		}
		return newError(KindHostFault, StatusGuestFault, "tool", o.result.ExecutionID, fmt.Sprintf("tool handler panic: %v", tr.err), tr.err)
	}

	if tr.err != nil {
		if errors.Is(tr.err, context.Canceled) || errors.Is(tr.err, context.DeadlineExceeded) {
			if cause := context.Cause(o.callCtx); cause != nil {
				code := "cancelled"
				if errors.Is(cause, context.DeadlineExceeded) {
					code = "deadline_exceeded"
				}
				c.errorCode = code
				if err := o.emitTool(c, "finished", code); err != nil {
					return err
				}
				return classifyToolCause(cause, o.result.ExecutionID)
			}
		}
		code, msg := toolErrorCode(tr.err)
		c.errorCode = code
		c.response = &toolResponse{ID: c.id, OK: false, Error: &toolErrorBody{Code: code, Message: msg}}
		if err := o.emitTool(c, "finished", code); err != nil {
			return err
		}
		return nil
	}

	raw, err := encodeValue(tr.value)
	if err != nil {
		c.errorCode = "invalid_result"
		if oerr := o.emitTool(c, "finished", "invalid_result"); oerr != nil {
			return oerr
		}
		return newError(KindProtocolError, StatusProtocolError, "tool", o.result.ExecutionID, "invalid tool result: "+err.Error(), err)
	}
	if len(raw) > o.maxToolResultBytes {
		c.errorCode = "result_too_large"
		if oerr := o.emitTool(c, "finished", "result_too_large"); oerr != nil {
			return oerr
		}
		return newError(KindLimitExceeded, StatusLimitExceeded, "tool", o.result.ExecutionID, fmt.Sprintf("tool result exceeds %d bytes", o.maxToolResultBytes), nil)
	}
	c.response = &toolResponse{ID: c.id, OK: true, Value: json.RawMessage(raw)}
	if oerr := o.emitTool(c, "finished", ""); oerr != nil {
		return oerr
	}
	return nil
}

func (o *executionOwner) takeReadyResponses() []toolResponse {
	var out []toolResponse
	for _, id := range o.receivedOrder {
		c := o.calls[id]
		if c == nil || c.state != CallResponseReady {
			continue
		}
		out = append(out, *c.response)
		c.state = CallDelivered
		c.delivered = true
		o.tombstones[id] = true
		o.deliveredOrder = append(o.deliveredOrder, id)
	}
	return out
}

func (o *executionOwner) hasReady() bool {
	for _, c := range o.calls {
		if c.state == CallResponseReady {
			return true
		}
	}
	return false
}

func (o *executionOwner) cancelAll() {
	for _, c := range o.calls {
		if c.cancel != nil {
			c.cancel()
		}
	}
}

func (o *executionOwner) finishAll() {
	o.cancelAll()
	for _, c := range o.calls {
		if c.state != CallCompleted {
			c.state = CallCompleted
			c.completed = true
		}
	}
}

func (o *executionOwner) ledger() []CallLedgerEntry {
	out := make([]CallLedgerEntry, 0, len(o.calls))
	for _, id := range o.receivedOrder {
		c := o.calls[id]
		if c == nil {
			continue
		}
		out = append(out, CallLedgerEntry{
			ID:         c.id,
			Name:       c.name,
			State:      c.state,
			Dispatched: c.dispatched,
			Completed:  c.completed,
			Delivered:  c.delivered,
			Error:      c.errorCode,
		})
	}
	return out
}

// emit delivers one observation to the observer. The execution owner is the
// only caller, so observations are serial. A panicking observer is recovered
// and classified as HostFault; the observer is not called again.
func (o *executionOwner) emit(obs Observation) *Error {
	if o.req.Observer == nil || o.observerFailed {
		return nil
	}
	var perr any
	func() {
		defer func() {
			if p := recover(); p != nil {
				perr = p
			}
		}()
		o.req.Observer(obs)
	}()
	if perr != nil {
		o.observerFailed = true
		return newError(KindHostFault, StatusGuestFault, "observer", o.result.ExecutionID, fmt.Sprintf("observer panic: %v", perr), nil)
	}
	return nil
}

func (o *executionOwner) emitOutput(out Output) *Error {
	switch out.Type {
	case "text":
		return o.emit(Observation{Kind: ObservationText, Text: out.Text})
	case "console":
		return o.emit(Observation{Kind: ObservationConsole, Text: out.Text, Level: out.Level})
	case "image":
		// The observer gets its own copy so a mutating observer cannot change
		// the retained Result.Outputs image bytes.
		return o.emit(Observation{Kind: ObservationImage, MIME: out.MIME, Data: append([]byte(nil), out.Data...)})
	}
	return nil
}

func (o *executionOwner) emitTool(c *callState, phase, errCode string) *Error {
	return o.emit(Observation{
		Kind:     ObservationTool,
		ToolName: c.name,
		CallID:   c.id,
		Phase:    phase,
		Error:    errCode,
	})
}

func toolErrorCode(err error) (string, string) {
	var te *ToolError
	if errors.As(err, &te) {
		return te.Code, truncateString(te.Message, 4096)
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", "tool call cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded", "tool call deadline exceeded"
	}
	return "tool_error", truncateString(err.Error(), 4096)
}

func classifyToolCause(cause error, execID string) *Error {
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		return newError(KindTimedOut, StatusTimedOut, "tool", execID, "execution deadline exceeded while a tool call was outstanding", cause)
	case errors.Is(cause, context.Canceled):
		return newError(KindCancelled, StatusCancelled, "tool", execID, "execution cancelled while a tool call was outstanding", cause)
	default:
		return newError(KindCancelled, StatusCancelled, "tool", execID, "execution cancelled: "+cause.Error(), cause)
	}
}

// clipUTF8 returns the longest prefix of s that is at most max bytes and ends
// on a UTF-8 rune boundary.
func clipUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// truncateString returns the longest valid UTF-8 prefix of s that is at most
// max bytes. It never splits a multi-byte rune, so trace and error text stay
// valid UTF-8 under the byte cap.
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return clipUTF8(s, max)
}
