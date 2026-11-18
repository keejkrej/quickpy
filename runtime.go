package quickpy

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var toolNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Runtime owns the compiled artifact, tool registry, and execution admission.
// It is safe for concurrent use.
type Runtime struct {
	opts   RuntimeOptions
	engine engine

	toolMu sync.RWMutex
	tools  map[string]ToolHandler

	mu     sync.Mutex
	closed bool
	// activeMu guards active. It is separate from mu so the terminal finish
	// path can remove an execution from the in-flight set without taking the
	// admission lock. Lock order is mu -> activeMu; finish takes only activeMu.
	activeMu sync.Mutex
	active   map[*Execution]struct{}
	wg       sync.WaitGroup

	nextID atomic.Uint64

	closeOnce sync.Once
	closeDone chan struct{}
	closeMu   sync.Mutex
	closeErr  error
}

func newRuntime(ctx context.Context, opts RuntimeOptions, factory engineFactory) (*Runtime, error) {
	resolved, err := validateRuntimeOptions(opts)
	if err != nil {
		return nil, err
	}
	eng, err := factory(ctx, engineOptions{
		Artifact:        resolved.Artifact,
		MaxMemoryBytes:  resolved.MaxMemoryBytes,
		MaxMessageBytes: resolved.MaxMessageBytes,
		MaxOutputBytes:  resolved.MaxOutputBytes,
		MaxStateBytes:   resolved.MaxStateBytes,
		DefaultTimeout:  resolved.DefaultTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &Runtime{
		opts:      resolved,
		engine:    eng,
		tools:     make(map[string]ToolHandler),
		active:    make(map[*Execution]struct{}),
		closeDone: make(chan struct{}),
	}, nil
}

func validateRuntimeOptions(opts RuntimeOptions) (RuntimeOptions, error) {
	if opts.Artifact.Path == "" && len(opts.Artifact.Bytes) == 0 {
		return opts, invalidRequest("new", "artifact is required")
	}
	if opts.Artifact.Path != "" && len(opts.Artifact.Bytes) > 0 {
		return opts, invalidRequest("new", "artifact must be either a path or bytes, not both")
	}
	if len(opts.Artifact.Bytes) > 0 {
		// Take ownership of the artifact bytes so a caller that reuses or
		// mutates its slice after New cannot race the compiled runtime or
		// change what was compiled.
		opts.Artifact.Bytes = append([]byte(nil), opts.Artifact.Bytes...)
	}
	if opts.MaxMemoryBytes <= 0 {
		return opts, invalidRequest("new", "MaxMemoryBytes must be > 0")
	}
	if opts.MaxExecutions <= 0 {
		return opts, invalidRequest("new", "MaxExecutions must be > 0")
	}
	if opts.DefaultTimeout < 0 {
		return opts, invalidRequest("new", "DefaultTimeout must be >= 0")
	}
	if opts.MaxSourceBytes < 0 {
		return opts, invalidRequest("new", "MaxSourceBytes must be >= 0")
	}
	if opts.MaxSourceBytes == 0 {
		opts.MaxSourceBytes = 256 << 10
	}
	if opts.MaxMessageBytes < 0 {
		return opts, invalidRequest("new", "MaxMessageBytes must be >= 0")
	}
	if opts.MaxMessageBytes == 0 {
		opts.MaxMessageBytes = 1 << 20
	}
	if opts.MaxOutputBytes < 0 {
		return opts, invalidRequest("new", "MaxOutputBytes must be >= 0")
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = 4 << 20
	}
	if opts.MaxToolResultBytes < 0 {
		return opts, invalidRequest("new", "MaxToolResultBytes must be >= 0")
	}
	if opts.MaxToolResultBytes == 0 {
		opts.MaxToolResultBytes = 1 << 20
	}
	if opts.MaxStateBytes < 0 {
		return opts, invalidRequest("new", "MaxStateBytes must be >= 0")
	}
	if opts.MaxStateBytes == 0 {
		opts.MaxStateBytes = 1 << 20
	}
	if opts.MaxToolCalls < 0 {
		return opts, invalidRequest("new", "MaxToolCalls must be >= 0")
	}
	if opts.MaxConcurrentToolCalls < 0 {
		return opts, invalidRequest("new", "MaxConcurrentToolCalls must be >= 0")
	}
	if opts.MaxConcurrentToolCalls == 0 {
		opts.MaxConcurrentToolCalls = 8
	}
	if opts.MaxPendingCalls < 0 {
		return opts, invalidRequest("new", "MaxPendingCalls must be >= 0")
	}
	if opts.MaxPendingCalls == 0 {
		opts.MaxPendingCalls = 64
	}
	if opts.MaxResumes < 0 {
		return opts, invalidRequest("new", "MaxResumes must be >= 0")
	}
	if opts.MaxResumes == 0 {
		opts.MaxResumes = 10000
	}
	return opts, nil
}

// RegisterTool adds a host capability. Names match
// ^[A-Za-z_][A-Za-z0-9_]{0,63}$ and are unique per runtime. A duplicate
// registration returns an error and leaves the previous handler in place.
func (r *Runtime) RegisterTool(name string, h ToolHandler) error {
	if !toolNameRE.MatchString(name) {
		return invalidRequest("register_tool", fmt.Sprintf("invalid tool name %q", name))
	}
	if h == nil {
		return invalidRequest("register_tool", "tool handler is nil")
	}
	r.toolMu.Lock()
	defer r.toolMu.Unlock()
	if _, ok := r.tools[name]; ok {
		return invalidRequest("register_tool", fmt.Sprintf("tool %q is already registered", name))
	}
	r.tools[name] = h
	return nil
}

// UnregisterTool removes a host capability. It affects only later executions.
func (r *Runtime) UnregisterTool(name string) error {
	r.toolMu.Lock()
	defer r.toolMu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return invalidRequest("unregister_tool", fmt.Sprintf("tool %q is not registered", name))
	}
	delete(r.tools, name)
	return nil
}

// ToolNames returns the registered tool names in sorted order.
func (r *Runtime) ToolNames() []string {
	r.toolMu.RLock()
	defer r.toolMu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Execute validates the request, snapshots the tool registry, admits the
// execution atomically, and returns a handle. Validation and capacity failures
// return an immediate error and no handle.
func (r *Runtime) Execute(ctx context.Context, req ExecuteRequest) (*Execution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resolved, err := r.resolveRequest(req)
	if err != nil {
		return nil, err
	}
	tools := r.snapshotTools()
	sink := r.opts.EventSink
	if req.EventSink != nil {
		sink = req.EventSink
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, &Error{Kind: KindRuntimeClosed, Op: "execute", Message: "runtime is closed"}
	}
	r.activeMu.Lock()
	if len(r.active) >= r.opts.MaxExecutions {
		r.activeMu.Unlock()
		r.mu.Unlock()
		return nil, &Error{Kind: KindCapacityExceeded, Op: "execute", Message: fmt.Sprintf("max executions %d reached", r.opts.MaxExecutions)}
	}
	id := fmt.Sprintf("exec-%d", r.nextID.Add(1))
	execCtx, cancel := context.WithCancelCause(ctx)
	e := newExecution(r, id, execCtx, cancel, sink, resolved.MaxOutputBytes, r.opts.MaxStateBytes)
	r.active[e] = struct{}{}
	r.activeMu.Unlock()
	r.wg.Add(1)
	r.mu.Unlock()

	go e.watchParent(ctx)
	// The public contract treats MaxToolCalls == 0 as "no cap". The low-level
	// engine uses -1 for unlimited and reserves 0 for its own M0 default, so
	// map the public zero explicitly instead of forwarding math.MaxInt as a
	// guest-visible cap.
	engineMaxToolCalls := resolved.MaxToolCalls
	if engineMaxToolCalls == 0 {
		engineMaxToolCalls = -1
	}
	go r.runExecution(e, engineRequest{
		Source:                 req.Source,
		Filename:               req.Filename,
		InitialState:           req.InitialState,
		Timeout:                resolved.Timeout,
		MaxMemoryBytes:         resolved.MaxMemoryBytes,
		MaxMessageBytes:        r.opts.MaxMessageBytes,
		MaxOutputBytes:         resolved.MaxOutputBytes,
		MaxToolResultBytes:     r.opts.MaxToolResultBytes,
		MaxStateBytes:          r.opts.MaxStateBytes,
		MaxToolCalls:           engineMaxToolCalls,
		MaxConcurrentToolCalls: r.opts.MaxConcurrentToolCalls,
		MaxPendingCalls:        r.opts.MaxPendingCalls,
		MaxResumes:             r.opts.MaxResumes,
		Tools:                  tools,
		Observer:               e.observe,
	})
	return e, nil
}

type resolvedRequest struct {
	Timeout        time.Duration
	MaxMemoryBytes int64
	MaxOutputBytes int
	MaxToolCalls   int
}

func (r *Runtime) resolveRequest(req ExecuteRequest) (resolvedRequest, error) {
	if req.Filename == "" {
		return resolvedRequest{}, invalidRequest("execute", "filename is required")
	}
	if len(req.Source) > r.opts.MaxSourceBytes {
		return resolvedRequest{}, invalidRequest("execute", fmt.Sprintf("source exceeds %d bytes", r.opts.MaxSourceBytes))
	}
	if len(req.Source) > r.opts.MaxMessageBytes {
		// The source is part of the start message. Reject an obviously
		// oversized source before admission; the low-level engine still
		// enforces the exact encoded message cap.
		return resolvedRequest{}, invalidRequest("execute", fmt.Sprintf("source exceeds %d bytes", r.opts.MaxMessageBytes))
	}
	if req.Timeout < 0 {
		return resolvedRequest{}, invalidRequest("execute", "Timeout must be >= 0")
	}
	timeout := r.opts.DefaultTimeout
	if req.Timeout > 0 {
		if r.opts.DefaultTimeout > 0 && req.Timeout > r.opts.DefaultTimeout {
			return resolvedRequest{}, invalidRequest("execute", "Timeout may only tighten the runtime default")
		}
		timeout = req.Timeout
	}
	if req.MaxMemoryBytes < 0 {
		return resolvedRequest{}, invalidRequest("execute", "MaxMemoryBytes must be >= 0")
	}
	maxMemory := r.opts.MaxMemoryBytes
	if req.MaxMemoryBytes > 0 && req.MaxMemoryBytes != r.opts.MaxMemoryBytes {
		return resolvedRequest{}, invalidRequest("execute", "per-execution MaxMemoryBytes tightening is not supported by this engine; use 0 to inherit the runtime cap")
	}
	if req.MaxOutputBytes < 0 {
		return resolvedRequest{}, invalidRequest("execute", "MaxOutputBytes must be >= 0")
	}
	maxOutput := r.opts.MaxOutputBytes
	if req.MaxOutputBytes > 0 {
		if req.MaxOutputBytes > r.opts.MaxOutputBytes {
			return resolvedRequest{}, invalidRequest("execute", "MaxOutputBytes may only lower the runtime ceiling")
		}
		maxOutput = req.MaxOutputBytes
	}
	if req.MaxToolCalls < 0 {
		return resolvedRequest{}, invalidRequest("execute", "MaxToolCalls must be >= 0")
	}
	maxToolCalls := r.opts.MaxToolCalls
	if req.MaxToolCalls > 0 {
		if r.opts.MaxToolCalls > 0 && req.MaxToolCalls > r.opts.MaxToolCalls {
			return resolvedRequest{}, invalidRequest("execute", "MaxToolCalls may only lower the runtime ceiling")
		}
		maxToolCalls = req.MaxToolCalls
	}
	if err := validateValue(req.InitialState, 0); err != nil {
		return resolvedRequest{}, invalidRequest("execute", "InitialState: "+err.Error())
	}
	return resolvedRequest{
		Timeout:        timeout,
		MaxMemoryBytes: maxMemory,
		MaxOutputBytes: maxOutput,
		MaxToolCalls:   maxToolCalls,
	}, nil
}

func (r *Runtime) snapshotTools() map[string]ToolHandler {
	r.toolMu.RLock()
	defer r.toolMu.RUnlock()
	if len(r.tools) == 0 {
		return nil
	}
	out := make(map[string]ToolHandler, len(r.tools))
	for name, h := range r.tools {
		out[name] = h
	}
	return out
}

func (r *Runtime) runExecution(e *Execution, req engineRequest) {
	defer r.wg.Done()
	var res engineResult
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = &Error{Kind: KindHostFault, Status: StatusFailed, ExecutionID: e.id, Op: "execute", Message: fmt.Sprintf("host panic: %v", p)}
			}
		}()
		res, err = r.engine.Execute(e.ctx, req)
	}()
	// finish is the single terminal path: it removes the execution from the
	// in-flight set and only then closes done. Do not remove here.
	e.finish(res, err)
}

// removeActive drops e from the in-flight set. It is called by Execution.finish
// while e.mu is held, before done is closed, so a caller that observes done
// closed also observes the execution as no longer counted against
// MaxExecutions.
func (r *Runtime) removeActive(e *Execution) {
	r.activeMu.Lock()
	delete(r.active, e)
	r.activeMu.Unlock()
}

// Close stops admission, cancels in-flight executions, waits for execution
// owner goroutines, and then closes the lower runtime. It is idempotent and
// safe for concurrent callers. A short context may return early; shutdown
// continues in the background.
func (r *Runtime) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.activeMu.Lock()
		execs := make([]*Execution, 0, len(r.active))
		for e := range r.active {
			execs = append(execs, e)
		}
		r.activeMu.Unlock()
		r.mu.Unlock()
		for _, e := range execs {
			e.Cancel()
		}
		go func() {
			r.wg.Wait()
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						err = &Error{Kind: KindHostFault, Op: "close", Message: fmt.Sprintf("engine close panic: %v", p)}
					}
				}()
				err = r.engine.Close(context.Background())
			}()
			r.closeMu.Lock()
			r.closeErr = err
			r.closeMu.Unlock()
			close(r.closeDone)
		}()
	})
	select {
	case <-r.closeDone:
		r.closeMu.Lock()
		defer r.closeMu.Unlock()
		return r.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
