package quickpy

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/keejkrej/quickpy/internal/wasmguest"
)

// New validates options, loads and compiles the artifact once, and returns a
// Runtime. The artifact is compiled once and instantiated fresh per execution.
func New(opts RuntimeOptions) (*Runtime, error) {
	return newRuntime(context.Background(), opts, newWasmEngine)
}

type wasmEngine struct {
	rt *wasmguest.Runtime
}

func newWasmEngine(ctx context.Context, opts engineOptions) (engine, error) {
	path := opts.Artifact.Path
	var cleanup func()
	if len(opts.Artifact.Bytes) > 0 {
		f, err := os.CreateTemp("", "quickpy-artifact-*.wasm")
		if err != nil {
			return nil, &Error{Kind: KindInvalidRequest, Op: "new", Message: "write artifact: " + err.Error(), Cause: err}
		}
		name := f.Name()
		if _, err := f.Write(opts.Artifact.Bytes); err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return nil, &Error{Kind: KindInvalidRequest, Op: "new", Message: "write artifact: " + err.Error(), Cause: err}
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(name)
			return nil, &Error{Kind: KindInvalidRequest, Op: "new", Message: "write artifact: " + err.Error(), Cause: err}
		}
		path = name
		cleanup = func() { _ = os.Remove(name) }
	}
	rt, err := wasmguest.NewRuntime(ctx, wasmguest.Options{
		ArtifactPath:    path,
		MaxMemoryBytes:  opts.MaxMemoryBytes,
		MaxMessageBytes: opts.MaxMessageBytes,
		MaxOutputBytes:  opts.MaxOutputBytes,
		MaxStateBytes:   opts.MaxStateBytes,
		DefaultTimeout:  opts.DefaultTimeout,
	})
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		return nil, mapWasmError(err, "")
	}
	return &wasmEngine{rt: rt}, nil
}

func (e *wasmEngine) Execute(ctx context.Context, req engineRequest) (engineResult, error) {
	wreq := wasmguest.Request{
		Source:                 req.Source,
		Filename:               req.Filename,
		Timeout:                req.Timeout,
		Tools:                  convertTools(req.Tools),
		MaxConcurrentToolCalls: req.MaxConcurrentToolCalls,
		MaxToolCalls:           req.MaxToolCalls,
		MaxPendingCalls:        req.MaxPendingCalls,
		MaxResumes:             req.MaxResumes,
		MaxToolResultBytes:     req.MaxToolResultBytes,
		InitialState:           req.InitialState,
		Observer: wasmguest.Observer(func(obs wasmguest.Observation) {
			if req.Observer != nil {
				req.Observer(convertObservation(obs))
			}
		}),
		MaxMemoryBytes: req.MaxMemoryBytes,
		MaxOutputBytes: req.MaxOutputBytes,
		MaxStateBytes:  req.MaxStateBytes,
	}
	res, err := e.rt.ExecuteWithTools(ctx, wreq)
	out := engineResult{}
	if res != nil {
		out = engineResult{
			Status:       mapStatus(res.Status),
			State:        res.State,
			StateWritten: res.StateWritten,
			ToolCalls:    res.ToolCalls,
			Duration:     res.Duration,
			Guest:        mapGuest(res.Guest),
		}
	}
	if err != nil {
		return out, mapWasmError(err, "")
	}
	return out, nil
}

func (e *wasmEngine) Close(ctx context.Context) error {
	return e.rt.Close(ctx)
}

func convertTools(tools map[string]ToolHandler) map[string]wasmguest.ToolHandler {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]wasmguest.ToolHandler, len(tools))
	for name, h := range tools {
		h := h
		out[name] = func(ctx context.Context, call wasmguest.ToolCall) (any, error) {
			v, err := h(ctx, ToolCall{
				Name:        call.Name,
				Args:        call.Args,
				ExecutionID: call.ExecutionID,
				Deadline:    call.Deadline,
			})
			if err != nil {
				var te *ToolError
				if errors.As(err, &te) {
					return nil, &wasmguest.ToolError{Code: te.Code, Message: te.Message}
				}
			}
			return v, err
		}
	}
	return out
}

func convertObservation(obs wasmguest.Observation) observation {
	switch obs.Kind {
	case wasmguest.ObservationText:
		return observation{Kind: observationText, Text: obs.Text}
	case wasmguest.ObservationConsole:
		return observation{Kind: observationConsole, Text: obs.Text, Level: obs.Level}
	case wasmguest.ObservationImage:
		return observation{Kind: observationImage, MIME: obs.MIME, Data: obs.Data}
	case wasmguest.ObservationTool:
		return observation{Kind: observationTool, ToolName: obs.ToolName, CallID: obs.CallID, Phase: obs.Phase, Error: obs.Error}
	default:
		return observation{}
	}
}

func mapWasmError(err error, execID string) *Error {
	var we *wasmguest.Error
	if errors.As(err, &we) {
		out := &Error{
			Kind:        mapKind(we.Kind),
			Status:      mapStatus(we.Status),
			ExecutionID: execID,
			Op:          we.Op,
			Message:     we.Message,
			Cause:       we.Cause,
		}
		if we.Exception != nil {
			out.Script = &ScriptErrorInfo{
				Type:      we.Exception.Type,
				Message:   we.Exception.Message,
				Traceback: we.Exception.Traceback,
				Filename:  we.Exception.Filename,
				Line:      we.Exception.Line,
			}
		}
		tc := we.ToolCause
		if tc == nil && we.Exception != nil {
			tc = we.Exception.ToolCause
		}
		if tc != nil {
			out.Tool = &ToolCause{
				Tool:    tc.Tool,
				Code:    tc.Code,
				Message: tc.Message,
			}
			if tc.CallID != nil {
				out.Tool.CallID = strconv.FormatInt(*tc.CallID, 10)
			}
		}
		return out
	}
	return &Error{Kind: KindHostFault, Status: StatusFailed, ExecutionID: execID, Op: "execute", Message: err.Error(), Cause: err}
}

func mapKind(k wasmguest.Kind) Kind {
	switch k {
	case wasmguest.KindInvalidRequest:
		return KindInvalidRequest
	case wasmguest.KindProtocolError:
		return KindProtocolError
	case wasmguest.KindScriptError:
		return KindScriptError
	case wasmguest.KindGuestFault:
		return KindGuestFault
	case wasmguest.KindLimitExceeded:
		return KindLimitExceeded
	case wasmguest.KindCancelled:
		return KindCancelled
	case wasmguest.KindTimedOut:
		return KindTimedOut
	case wasmguest.KindStalled:
		return KindStalled
	case wasmguest.KindHostFault:
		return KindHostFault
	case wasmguest.KindRuntimeClosed:
		return KindRuntimeClosed
	default:
		return KindHostFault
	}
}

func mapStatus(s wasmguest.Status) Status {
	switch s {
	case wasmguest.StatusCompleted:
		return StatusCompleted
	case wasmguest.StatusFailed:
		return StatusFailed
	case wasmguest.StatusCancelled:
		return StatusCancelled
	case wasmguest.StatusTimedOut:
		return StatusTimedOut
	case wasmguest.StatusStalled:
		return StatusStalled
	case wasmguest.StatusLimitExceeded:
		return StatusLimitExceeded
	case wasmguest.StatusGuestFault, wasmguest.StatusProtocolError:
		return StatusFailed
	default:
		return ""
	}
}

func mapGuest(g wasmguest.GuestInfo) GuestInfo {
	imports := make([]Import, len(g.Imports))
	for i, imp := range g.Imports {
		imports[i] = Import{Kind: imp.Kind, Module: imp.Module, Name: imp.Name}
	}
	engine := g.Engine
	if engine == "" {
		engine = "micropython"
	}
	version := g.Version
	if version == "" {
		// An ABI 0.4 guest always reports its version. If an artifact somehow
		// omits it, report that honestly instead of pretending the identity is
		// complete.
		version = "unknown"
	}
	protocol := g.ProtocolVersion
	if protocol == "" {
		// The JSON protocol envelope is v0 for every supported ABI.
		protocol = "0"
	}
	return GuestInfo{
		Engine:          engine,
		Version:         version,
		ProtocolVersion: protocol,
		ABIVersion:      g.ABIVersion,
		Imports:         imports,
	}
}
