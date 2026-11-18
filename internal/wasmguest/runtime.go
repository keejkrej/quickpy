package wasmguest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ABI 0.4 is the M1 conformance gate. The integer is packed major/minor.
// ABI4 adds the structured outputs array (text, console, image) while keeping
// the legacy text-only output string for older consumers. The JSON protocol
// envelope remains v0; ProtocolVersion reports that envelope version, while
// ABIVersion reports the capability gate.
const (
	abiVersion      = 4
	abiVersionText  = "0.4"
	protocolVersion = "0"
)

// Options configures the spike host. All limits are per instance except where
// noted. The artifact is compiled once and instantiated fresh per execution.
//
// Set exactly one of ArtifactPath or ArtifactBytes. ArtifactPath is for CLI and
// local-development callers that already have a file. ArtifactBytes is
// compiled directly from memory and never touches the filesystem or a
// temporary directory. NewRuntime does not retain or mutate ArtifactBytes; the
// caller must not mutate the slice concurrently with NewRuntime.
type Options struct {
	ArtifactPath    string
	ArtifactBytes   []byte
	MaxMemoryBytes  int64
	MaxMessageBytes int
	MaxOutputBytes  int
	// MaxStateBytes bounds the encoded initial and final state values. Zero
	// inherits MaxMessageBytes. The public wrapper resolves the strict
	// per-execution value; this is the runtime default.
	MaxStateBytes  int
	DefaultTimeout time.Duration
}

// Import is one artifact import discovered at compile time.
type Import struct {
	Kind   string `json:"kind"`
	Module string `json:"module"`
	Name   string `json:"name"`
}

// GuestInfo is the host-observed guest identity for one execution.
type GuestInfo struct {
	Engine          string   `json:"engine,omitempty"`
	Version         string   `json:"version,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	ABIVersion      uint32   `json:"abi_version"`
	Imports         []Import `json:"imports"`
}

// ExportCalls counts guest export entries for one execution. It is evidence
// that cancellation did not call qp_free or re-enter qp_invoke after the
// active entry was force-closed.
type ExportCalls struct {
	Initialize int `json:"initialize"`
	ABI        int `json:"abi"`
	Alloc      int `json:"alloc"`
	Invoke     int `json:"invoke"`
	Free       int `json:"free"`
}

// Request is one execution request.
type Request struct {
	Source   string
	Filename string
	Timeout  time.Duration

	// InitialState is the JSON-like value loaded into the guest before source
	// execution. Nil encodes as JSON null (Python None).
	InitialState any

	// Observer receives ordered text/console/tool observations serially from
	// the execution owner. It must not call back into the guest. A panic is
	// recovered and classified as HostFault.
	Observer Observer

	// Tools is the per-execution tool snapshot. It is only used by
	// ExecuteWithTools; Execute clears it.
	Tools map[string]ToolHandler

	// MaxMemoryBytes is accepted for public-wrapper compatibility. The
	// runtime's global memory cap is authoritative; per-request memory limits
	// are not supported by this host.
	MaxMemoryBytes int64

	// MaxOutputBytes tightens the runtime output cap for this execution. Zero
	// inherits Options.MaxOutputBytes.
	MaxOutputBytes int

	// MaxStateBytes tightens the runtime state cap for this execution. Zero
	// inherits Options.MaxStateBytes (which itself defaults to
	// Options.MaxMessageBytes).
	MaxStateBytes int

	MaxConcurrentToolCalls int
	// MaxToolCalls is the total tool-call cap. Zero selects the M0 default
	// (1000) for low-level callers; a negative value means unlimited; a
	// positive value is an inclusive cap. The public SDK resolves its own
	// zero ("no cap") to -1 before calling this layer.
	MaxToolCalls       int
	MaxPendingCalls    int
	MaxResumes         int
	MaxToolResultBytes int
}

// Result is the host-observed outcome of one execution. It is intentionally
// spike-shaped, not the public QuickPy Result.
type Result struct {
	ExecutionID     string     `json:"execution_id"`
	InstanceID      string     `json:"instance_id"`
	Status          Status     `json:"status"`
	Outputs         []Output   `json:"outputs,omitempty"`
	Exception       *Exception `json:"exception,omitempty"`
	CapturedStdout  string     `json:"captured_stdout,omitempty"`
	CapturedStderr  string     `json:"captured_stderr,omitempty"`
	StdoutTruncated bool       `json:"stdout_truncated,omitempty"`
	StderrTruncated bool       `json:"stderr_truncated,omitempty"`
	OutputTruncated bool       `json:"output_truncated,omitempty"`
	Stalled         bool       `json:"stalled,omitempty"`
	UnknownFields   []string   `json:"unknown_fields,omitempty"`
	// State is the committed final state. It is only set on a successful
	// completed execution. StateWritten distinguishes set_state(None)
	// (State nil, StateWritten true) from no write (State nil, false).
	State               any           `json:"state,omitempty"`
	StateWritten        bool          `json:"state_written,omitempty"`
	Duration            time.Duration `json:"-"`
	DurationMS          float64       `json:"duration_ms"`
	Guest               GuestInfo     `json:"guest"`
	ExportCalls         ExportCalls   `json:"export_calls"`
	MemoryPages         uint32        `json:"memory_pages,omitempty"`
	InvokeReturned      bool          `json:"invoke_returned"`
	SkippedFreeOnCancel bool          `json:"skipped_free_on_cancel,omitempty"`

	Requests        []CallRequest     `json:"requests,omitempty"`
	DispatchOrder   []int64           `json:"dispatch_order,omitempty"`
	CompletionOrder []int64           `json:"completion_order,omitempty"`
	DeliveredOrder  []int64           `json:"delivered_order,omitempty"`
	ResumeCount     int               `json:"resume_count"`
	ToolCalls       int               `json:"tool_calls"`
	CallLedger      []CallLedgerEntry `json:"call_ledger,omitempty"`
	Trace           []string          `json:"trace,omitempty"`
}

// Runtime owns the compiled artifact and the wazero runtime. It is safe for
// concurrent Execute calls; each call gets a fresh instance.
type Runtime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	opts     Options
	imports  []Import

	mu     sync.Mutex
	closed bool

	nextExec atomic.Uint64
	nextInst atomic.Uint64
}

// NewRuntime compiles the artifact once, installs the WASI prelude, and rejects
// imports outside the spike's capability allowlist. The artifact comes from
// exactly one of Options.ArtifactPath or Options.ArtifactBytes; the bytes form
// is compiled directly and never creates a temporary file.
func NewRuntime(ctx context.Context, opts Options) (*Runtime, error) {
	switch {
	case opts.ArtifactPath != "" && len(opts.ArtifactBytes) > 0:
		return nil, newError(KindInvalidRequest, "", "new", "", "artifact must be either a path or bytes, not both", nil)
	case opts.ArtifactPath == "" && opts.ArtifactBytes == nil:
		return nil, newError(KindInvalidRequest, "", "new", "", "artifact path or bytes is required", nil)
	case opts.ArtifactPath == "" && len(opts.ArtifactBytes) == 0:
		return nil, newError(KindInvalidRequest, "", "new", "", "artifact bytes are empty", nil)
	}
	if opts.MaxMemoryBytes <= 0 {
		return nil, newError(KindInvalidRequest, "", "new", "", "max memory bytes must be > 0", nil)
	}
	if opts.MaxMessageBytes <= 0 {
		return nil, newError(KindInvalidRequest, "", "new", "", "max message bytes must be > 0", nil)
	}
	if opts.MaxOutputBytes <= 0 {
		return nil, newError(KindInvalidRequest, "", "new", "", "max output bytes must be > 0", nil)
	}
	if opts.DefaultTimeout < 0 {
		return nil, newError(KindInvalidRequest, "", "new", "", "default timeout must be >= 0", nil)
	}

	wasmBytes := opts.ArtifactBytes
	if opts.ArtifactPath != "" {
		var err error
		wasmBytes, err = os.ReadFile(opts.ArtifactPath)
		if err != nil {
			return nil, newError(KindInvalidRequest, "", "new", "", "read artifact: "+err.Error(), err)
		}
	}

	pages := uint32((opts.MaxMemoryBytes + 65535) / 65536)
	if pages == 0 {
		pages = 1
	}
	if pages > 65536 {
		pages = 65536
	}

	// The real MicroPython guest uses one exception-handling tag for nlr
	// (setjmp/longjmp). Enable only that experimental proposal on top of the
	// default V2 feature set; do not enable threads, tail calls, extended
	// const, or typed function references.
	features := api.CoreFeaturesV2.SetEnabled(experimental.CoreFeaturesExceptionHandling, true)
	rtc := wazero.NewRuntimeConfig().
		WithCoreFeatures(features).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(pages)
	rt := wazero.NewRuntimeWithConfig(ctx, rtc)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, newError(KindHostFault, "", "new", "", "install WASI prelude: "+err.Error(), err)
	}

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, classifyCompileError(err)
	}

	imports := collectImports(compiled)
	if err := checkImports(imports); err != nil {
		_ = compiled.Close(ctx)
		_ = rt.Close(ctx)
		return nil, err
	}

	return &Runtime{
		rt:       rt,
		compiled: compiled,
		opts:     opts,
		imports:  imports,
	}, nil
}

// Imports returns a copy of the artifact's discovered imports.
func (r *Runtime) Imports() []Import {
	out := make([]Import, len(r.imports))
	copy(out, r.imports)
	return out
}

// Close releases the compiled module and runtime. It is idempotent.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	return r.rt.Close(ctx)
}

type startMessage struct {
	V            int             `json:"v"`
	Type         string          `json:"type"`
	Source       string          `json:"source"`
	Filename     string          `json:"filename"`
	InitialState json.RawMessage `json:"initial_state"`
	// MaxToolCalls is always present on an ABI 0.4 start. -1 means unlimited;
	// >= 0 is an inclusive cap (0 forbids all tool calls). The ABI3 semantics
	// are unchanged.
	MaxToolCalls int `json:"max_tool_calls"`
}

func classifyCause(cause error, execID string) *Error {
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		return newError(KindTimedOut, StatusTimedOut, "invoke", execID, "execution deadline exceeded; active guest entry was force-closed", cause)
	case errors.Is(cause, context.Canceled):
		return newError(KindCancelled, StatusCancelled, "invoke", execID, "execution cancelled; active guest entry was force-closed", cause)
	default:
		return newError(KindCancelled, StatusCancelled, "invoke", execID, "execution cancelled: "+cause.Error(), cause)
	}
}

func protocolErr(execID, msg string) *Error {
	return newError(KindProtocolError, StatusProtocolError, "execute", execID, msg, nil)
}

func classifyCompileError(err error) *Error {
	msg := err.Error()
	if isMemoryLimitError(err) {
		return newError(KindLimitExceeded, StatusLimitExceeded, "new", "", "compile artifact: "+msg, err)
	}
	return newError(KindProtocolError, "", "new", "", "compile artifact: "+msg, err)
}

// isMemoryLimitError recognizes wazero's host-configured memory-limit errors.
// It is deliberately only used for compile/instantiate failures, where the
// error is produced by wazero's memory sizing/validation rather than by an
// arbitrary guest trap. Runtime traps are never classified through this
// helper: wazero's memory.grow returns -1 to the guest instead of trapping, so
// a configured runtime cap hit is guest-visible (usually MemoryError) and an
// arbitrary out-of-bounds trap remains GuestFault.
func isMemoryLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "over limit of") || strings.Contains(msg, "memory limit")
}

func collectImports(cm wazero.CompiledModule) []Import {
	var out []Import
	for _, f := range cm.ImportedFunctions() {
		mod, name, ok := f.Import()
		if ok {
			out = append(out, Import{Kind: "function", Module: mod, Name: name})
		}
	}
	for _, m := range cm.ImportedMemories() {
		mod, name, ok := m.Import()
		if ok {
			out = append(out, Import{Kind: "memory", Module: mod, Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// allowedWASIFuncs is the M0 host's default-deny allowlist. It is
// intentionally empty: the real reactor imports zero WASI functions. The WASI
// prelude is installed for compatibility, but any artifact import is rejected
// before instantiation. Do not add filesystem, network, environment, fd, or
// timer imports without evidence and a documented capability decision.
var allowedWASIFuncs = map[string]bool{}

func checkImports(imports []Import) error {
	for _, imp := range imports {
		if imp.Kind != "function" {
			return newError(KindProtocolError, "", "new", "", fmt.Sprintf("unsupported imported %s %q.%q", imp.Kind, imp.Module, imp.Name), nil)
		}
		if imp.Module != "wasi_snapshot_preview1" {
			return newError(KindProtocolError, "", "new", "", fmt.Sprintf("unsupported import module %q (only wasi_snapshot_preview1 is allowed)", imp.Module), nil)
		}
		if !allowedWASIFuncs[imp.Name] {
			return newError(KindProtocolError, "", "new", "", fmt.Sprintf("WASI import %q is not in the spike allowlist", imp.Name), nil)
		}
	}
	return nil
}
