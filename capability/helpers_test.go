package capability

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestAdapter constructs an Adapter over a fresh temp directory (unless
// opts.Root is set) and registers cleanup.
func newTestAdapter(t *testing.T, opts WorkspaceOptions) (*Adapter, string) {
	t.Helper()
	if opts.Root == "" {
		opts.Root = t.TempDir()
	}
	a, err := NewWorkspace(opts)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, opts.Root
}

func mustHandle(t *testing.T, a *Adapter, op string, args map[string]any) map[string]any {
	t.Helper()
	res, err := a.Handle(context.Background(), op, args)
	if err != nil {
		t.Fatalf("%s(%v): unexpected error: %v", op, args, err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("%s: result type %T, want map[string]any", op, res)
	}
	return m
}

func wantCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", code)
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error type %T, want *Error: %v", err, err)
	}
	if ce.Code != code {
		t.Fatalf("error code %q, want %q (message: %s)", ce.Code, code, ce.Message)
	}
	return ce
}

func writeHostFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}
}

func readHostFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	return data
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink and permission semantics differ on Windows")
	}
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission checks")
	}
}

// assertNoRootLeak checks that err is an *Error with wantCode, that its
// message contains wantRelative when non-empty, and that it contains neither
// the host root nor any of the forbidden strings.
func assertNoRootLeak(t *testing.T, err error, wantCode, wantRelative string, forbidden ...string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", wantCode)
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error type %T, want *Error: %v", err, err)
	}
	if ce.Code != wantCode {
		t.Fatalf("error code %q, want %q (message: %s)", ce.Code, wantCode, ce.Message)
	}
	if wantRelative != "" && !strings.Contains(ce.Message, wantRelative) {
		t.Fatalf("message %q does not contain relative path %q", ce.Message, wantRelative)
	}
	for _, s := range forbidden {
		if s != "" && strings.Contains(ce.Message, s) {
			t.Fatalf("message leaks %q: %s", s, ce.Message)
		}
	}
	return ce
}
