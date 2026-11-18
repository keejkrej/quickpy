// Package quickpybridge installs a capability.Adapter's filesystem operations
// as QuickPy host tools.
//
// The bridge is deliberately thin: it translates a QuickPy ToolCall into the
// adapter's (operation, map[string]any) contract, forwards the caller's
// context unchanged, and converts a *capability.Error into a *quickpy.ToolError
// so the guest can catch it as a normal tool failure. It does not choose a
// workspace, grant permissions, or add ambient filesystem access. The
// application must construct the adapter with its own policy and call Install
// before Execute.
package quickpybridge

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/keejkrej/quickpy"
	"github.com/keejkrej/quickpy/capability"
)

// ToolRegistrar is the subset of *quickpy.Runtime that Install needs.
// *quickpy.Runtime satisfies it.
type ToolRegistrar interface {
	RegisterTool(name string, h quickpy.ToolHandler) error
}

// toolUnregisterer is implemented by *quickpy.Runtime. Install uses it to
// roll back a partial installation when a later registration fails.
type toolUnregisterer interface {
	UnregisterTool(name string) error
}

// adapter is the capability surface Install needs. *capability.Adapter
// implements it; tests use a stub.
type adapter interface {
	Handle(ctx context.Context, operation string, args map[string]any) (any, error)
	Operations() []string
}

// Install registers every operation exposed by a as a QuickPy tool.
//
// Install must be called before Execute. Execute snapshots the tool registry,
// so a tool installed after an execution starts is not visible to that
// execution.
//
// Registration is all-or-nothing when r also implements UnregisterTool, as
// *quickpy.Runtime does. If a later registration fails, Install removes the
// tools it already registered and returns the original error. If r does not
// support unregistration, Install returns an error that names the partially
// installed tools; it never reports success for a partial install.
func Install(r ToolRegistrar, a *capability.Adapter) error {
	if a == nil {
		return errors.New("quickpybridge: adapter is nil")
	}
	return install(r, a)
}

func install(r ToolRegistrar, a adapter) error {
	if r == nil {
		return errors.New("quickpybridge: registrar is nil")
	}
	if a == nil {
		return errors.New("quickpybridge: adapter is nil")
	}

	ops := append([]string(nil), a.Operations()...)
	sort.Strings(ops)

	installed := make([]string, 0, len(ops))
	for _, op := range ops {
		if err := r.RegisterTool(op, handler(a, op)); err != nil {
			if rbErr := rollback(r, installed); rbErr != nil {
				return fmt.Errorf("quickpybridge: register %q: %w (rollback failed: %v)", op, err, rbErr)
			}
			return fmt.Errorf("quickpybridge: register %q: %w", op, err)
		}
		installed = append(installed, op)
	}
	return nil
}

func rollback(r ToolRegistrar, installed []string) error {
	if len(installed) == 0 {
		return nil
	}
	u, ok := r.(toolUnregisterer)
	if !ok {
		return fmt.Errorf("registrar does not support rollback; partially installed: %v", installed)
	}
	var first error
	for i := len(installed) - 1; i >= 0; i-- {
		if err := u.UnregisterTool(installed[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func handler(a adapter, op string) quickpy.ToolHandler {
	return func(ctx context.Context, call quickpy.ToolCall) (quickpy.Value, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		args, err := toolArgs(call.Args)
		if err != nil {
			return nil, &quickpy.ToolError{Code: capability.CodeToolArgument, Message: err.Error()}
		}
		result, err := a.Handle(ctx, op, args)
		if err != nil {
			// The adapter reports cancellation as a structured io_error. The
			// runtime classifies context.Canceled/DeadlineExceeded specially,
			// so preserve the context cause instead of turning it into an
			// ordinary tool error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, convertError(err)
		}
		return result, nil
	}
}

// toolArgs accepts the JSON object the guest sends. A nil value is treated as
// an empty argument object; any other non-object shape is an operational
// tool_argument failure, not a host fault.
func toolArgs(v quickpy.Value) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool arguments must be an object, got %T", v)
	}
	return m, nil
}

// convertError preserves the adapter's stable code and message. Unknown
// errors are returned unchanged so the runtime can classify them as ordinary
// tool failures rather than host faults.
func convertError(err error) error {
	var ce *capability.Error
	if errors.As(err, &ce) {
		return &quickpy.ToolError{Code: ce.Code, Message: ce.Message}
	}
	return err
}
