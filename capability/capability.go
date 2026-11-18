// Package capability implements a runtime-neutral filesystem capability core
// for polyglot agent execution.
//
// The package exposes a small, stable, JSON-like operation surface over a
// single host-chosen workspace root. It has no dependency on QuickPy, on a
// Python object model, or on any guest runtime. A peer bridge (Zig, Python,
// JavaScript, ...) can forward an operation name plus map[string]any arguments
// and receive a map[string]any result or a structured *Error.
//
// See README.md for the versioned v1 contract, path policy, limits, and
// security model.
package capability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"
)

// Operation names. These strings are part of the v1 wire contract and must not
// change without a contract version bump.
const (
	OpReadText   = "fs_read_text"
	OpWriteText  = "fs_write_text"
	OpReadBytes  = "fs_read_bytes"
	OpWriteBytes = "fs_write_bytes"
	OpExists     = "fs_exists"
	OpListDir    = "fs_listdir"
	OpMkdir      = "fs_mkdir"
	OpRemove     = "fs_remove"
)

// Error codes. These strings are part of the v1 wire contract. Language
// facades map them to their own exception or error classes; the core never
// invents a code outside this set.
const (
	CodeNotFound         = "not_found"
	CodePermissionDenied = "permission_denied"
	CodeInvalidPath      = "invalid_path"
	CodeInvalidArgument  = "invalid_argument"
	CodeToolArgument     = "tool_argument"
	CodeTooLarge         = "too_large"
	CodeIsDirectory      = "is_directory"
	CodeNotDirectory     = "not_directory"
	CodeExists           = "exists"
	CodeNotEmpty         = "not_empty"
	CodeIOError          = "io_error"
	CodeInvalidUTF8      = "invalid_utf8"
	CodeInvalidBase64    = "invalid_base64"
)

// Error is the structured error returned by Adapter.Handle. It carries a
// stable machine-readable Code and a human-readable Message. Messages contain
// only the caller-supplied relative path, never the host workspace root.
type Error struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "capability: " + e.Code + ": " + e.Message
}

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func toolArgument(format string, args ...any) *Error {
	return newError(CodeToolArgument, format, args...)
}

func invalidPath(format string, args ...any) *Error {
	return newError(CodeInvalidPath, format, args...)
}

func tooLarge(format string, args ...any) *Error {
	return newError(CodeTooLarge, format, args...)
}

func permissionDenied(format string, args ...any) *Error {
	return newError(CodePermissionDenied, format, args...)
}

func contextError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return newError(CodeIOError, "operation deadline exceeded")
	}
	return newError(CodeIOError, "operation cancelled")
}

// Adapter is a concurrency-safe filesystem capability bound to one workspace
// root. Construct it with NewWorkspace. The zero value is not usable.
type Adapter struct {
	root       *os.Root
	readOnly   bool
	maxBytes   int64
	maxEntries int

	mu     sync.RWMutex
	closed bool
}

// Operations returns the supported operation names in deterministic sorted
// order. The returned slice is a fresh copy and may be modified by the caller.
func (a *Adapter) Operations() []string {
	return []string{
		OpExists,
		OpListDir,
		OpMkdir,
		OpReadBytes,
		OpReadText,
		OpRemove,
		OpWriteBytes,
		OpWriteText,
	}
}

// Handle dispatches one operation. args may be nil. The result is a
// map[string]any with the keys documented in README.md. On failure the error
// is always an *Error.
//
// Handle is safe for concurrent use. A nil ctx is treated as
// context.Background(). Cancellation is checked at operation boundaries and
// before an atomic write is committed.
//
// Every error leaving Handle is normalized at this boundary. Operation code
// builds messages from static text and caller-relative paths; the boundary
// drops any non-*Error and redacts the host root as a last-resort defense.
func (a *Adapter) Handle(ctx context.Context, operation string, args map[string]any) (result any, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return nil, newError(CodeIOError, "adapter is closed")
	}
	if a.root == nil {
		return nil, newError(CodeIOError, "adapter is not initialized")
	}

	defer func() {
		if err != nil {
			err = a.publicError(err)
		}
	}()

	switch operation {
	case OpReadText:
		return a.readText(ctx, args)
	case OpWriteText:
		return a.writeText(ctx, args)
	case OpReadBytes:
		return a.readBytes(ctx, args)
	case OpWriteBytes:
		return a.writeBytes(ctx, args)
	case OpExists:
		return a.exists(ctx, args)
	case OpListDir:
		return a.listDir(ctx, args)
	case OpMkdir:
		return a.mkdir(ctx, args)
	case OpRemove:
		return a.remove(ctx, args)
	default:
		return nil, toolArgument("unknown operation %q", operation)
	}
}

// publicError normalizes an error leaving Handle. It guarantees the result is
// an *Error whose message contains only static text and caller-relative paths.
// It never copies raw host error text, which may contain a root-qualified path.
func (a *Adapter) publicError(err error) error {
	if err == nil {
		return nil
	}
	var ce *Error
	if errors.As(err, &ce) {
		if ce.Message == "" {
			return ce
		}
		return &Error{Code: ce.Code, Message: a.redactRoot(ce.Message)}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return contextError(err)
	}
	return newError(CodeIOError, "io error")
}

// Close releases the workspace root handle. It is idempotent. After Close,
// Handle returns an io_error and Operations still reports the static surface.
func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	if a.root == nil {
		return nil
	}
	return a.root.Close()
}

// checkKeys rejects argument keys outside the operation's contract. Unknown
// keys are reported deterministically (sorted) so bridges see stable messages.
func checkKeys(args map[string]any, allowed ...string) error {
	var unknown []string
	for key := range args {
		if !slices.Contains(allowed, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return toolArgument("unsupported argument %q", unknown[0])
}

func requireString(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok {
		return "", toolArgument("missing required argument %q", key)
	}
	s, ok := value.(string)
	if !ok {
		return "", toolArgument("argument %q must be a string", key)
	}
	return s, nil
}

func optionalString(args map[string]any, key, def string) (string, error) {
	value, ok := args[key]
	if !ok {
		return def, nil
	}
	s, ok := value.(string)
	if !ok {
		return "", toolArgument("argument %q must be a string", key)
	}
	return s, nil
}

func optionalBool(args map[string]any, key string, def bool) (bool, error) {
	value, ok := args[key]
	if !ok {
		return def, nil
	}
	b, ok := value.(bool)
	if !ok {
		return false, toolArgument("argument %q must be a boolean", key)
	}
	return b, nil
}
