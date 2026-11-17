package wasmguest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

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
	id    int64
	value any
	err   error
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

	execID := fmt.Sprintf("exec-%d", r.nextExec.Add(1))
	instID := fmt.Sprintf("inst-%d", r.nextInst.Add(1))
	start := time.Now()
	stdout := newBoundedBuffer(r.opts.MaxOutputBytes)
	stderr := newBoundedBuffer(r.opts.MaxOutputBytes)

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

	inst, ierr := r.rt.InstantiateModule(callCtx, r.compiled, modCfg)
	if ierr != nil {
		if cause := context.Cause(callCtx); cause != nil {
			return result, classifyCause(cause, execID)
		}
		return result, newError(KindGuestFault, StatusGuestFault, "instantiate", execID, "instantiate failed: "+ierr.Error(), ierr)
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
	if version != 1 {
		return result, protocolErr(execID, fmt.Sprintf("unsupported ABI version %d (want 1 = 0.1)", version))
	}

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

	maxConcurrent := req.MaxConcurrentToolCalls
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	maxToolCalls := req.MaxToolCalls
	if maxToolCalls <= 0 {
		maxToolCalls = 1000
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
		calls:              map[int64]*callState{},
		tombstones:         map[int64]bool{},
		awaiting:           map[int64]bool{},
		resultCh:           make(chan toolResult, maxConcurrent),
	}

	resp, oerr := owner.invoke(startMessage{
		V:        0,
		Type:     "start",
		Source:   req.Source,
		Filename: req.Filename,
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
			result.Status = StatusCompleted
			if ierr := owner.checkOutputLimits(); ierr != nil {
				return result, ierr
			}
			return result, nil

		case "failed":
			owner.finishAll()
			result.Status = StatusFailed
			result.Exception = resp.Exception
			return result, &Error{
				Kind:          KindScriptError,
				Status:        StatusFailed,
				ExecutionID:   execID,
				Op:            "execute",
				Message:       "guest reported failure",
				Exception:     resp.Exception,
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
			owner.dispatchQueued()

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

	resp, derr := decodeResponse(respCopy, o.rt.opts.MaxOutputBytes)
	if derr != nil {
		derr.ExecutionID = o.result.ExecutionID
		return nil, derr
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

func (o *executionOwner) accumulateOutput(resp *Response) *Error {
	for _, out := range resp.Outputs {
		if out.Type == "text" && out.Text == "" {
			continue
		}
		o.result.Outputs = append(o.result.Outputs, out)
		o.totalOutput += len(out.Text)
	}
	o.result.UnknownFields = append(o.result.UnknownFields, resp.UnknownFields...)
	if resp.OutputTruncated {
		o.result.OutputTruncated = true
	}
	if resp.Stalled {
		o.result.Stalled = true
	}
	if o.totalOutput > o.rt.opts.MaxOutputBytes {
		return newError(KindLimitExceeded, StatusLimitExceeded, "output", o.result.ExecutionID, fmt.Sprintf("total output exceeds %d bytes", o.rt.opts.MaxOutputBytes), nil)
	}
	return nil
}

func (o *executionOwner) checkOutputLimits() *Error {
	if o.result.OutputTruncated {
		return newError(KindLimitExceeded, StatusLimitExceeded, "output", o.result.ExecutionID, "guest reported truncated output", nil)
	}
	if o.stdout.Truncated() || o.stderr.Truncated() {
		return newError(KindLimitExceeded, StatusLimitExceeded, "output", o.result.ExecutionID, fmt.Sprintf("captured output exceeded %d bytes", o.rt.opts.MaxOutputBytes), nil)
	}
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
		if o.maxToolCalls > 0 && o.toolCalls >= o.maxToolCalls {
			return newError(KindLimitExceeded, StatusLimitExceeded, "tool", o.result.ExecutionID, fmt.Sprintf("tool call count exceeds %d", o.maxToolCalls), nil)
		}
		if len(o.calls) >= o.maxPendingCalls {
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

func (o *executionOwner) dispatchQueued() {
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
			continue
		}
		c.state = CallDispatched
		c.dispatched = true
		c.ctx, c.cancel = context.WithCancel(o.callCtx)
		o.dispatchOrder = append(o.dispatchOrder, c.id)
		o.active++
		o.startHandler(c, handler)
	}
}

func (o *executionOwner) startHandler(c *callState, handler ToolHandler) {
	go func() {
		var res toolResult
		defer func() {
			if p := recover(); p != nil {
				res = toolResult{id: c.id, err: &ToolError{Code: "handler_panic", Message: fmt.Sprintf("tool handler panic: %v", p)}}
			}
			o.resultCh <- res
		}()
		value, err := handler(c.ctx, ToolCall{
			Name:        c.name,
			Args:        c.args,
			ExecutionID: o.result.ExecutionID,
			CallID:      c.id,
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

	if tr.err != nil {
		code, msg := toolErrorCode(tr.err)
		c.errorCode = code
		c.response = &toolResponse{ID: c.id, OK: false, Error: &toolErrorBody{Code: code, Message: msg}}
		return nil
	}

	raw, err := json.Marshal(tr.value)
	if err != nil {
		c.errorCode = "invalid_result"
		c.response = &toolResponse{ID: c.id, OK: false, Error: &toolErrorBody{Code: "invalid_result", Message: err.Error()}}
		return nil
	}
	if len(raw) > o.maxToolResultBytes {
		c.errorCode = "result_too_large"
		c.response = &toolResponse{ID: c.id, OK: false, Error: &toolErrorBody{Code: "result_too_large", Message: fmt.Sprintf("tool result exceeds %d bytes", o.maxToolResultBytes)}}
		return nil
	}
	c.response = &toolResponse{ID: c.id, OK: true, Value: json.RawMessage(raw)}
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

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
