package capability

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// hostilePathError builds a root-qualified PathError like the ones os.Root
// returns from post-open Read/Write/Stat/ReadDir calls.
func hostilePathError(root, op, rel string, err error) *fs.PathError {
	return &fs.PathError{Op: op, Path: filepath.Join(root, rel), Err: err}
}

// textError wraps an error with arbitrary text, used to prove classification
// uses errors.Is rather than string parsing.
type textError struct {
	text string
	err  error
}

func (e *textError) Error() string { return e.text }
func (e *textError) Unwrap() error { return e.err }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestSanitizePathError(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	rel := "secret.txt"
	hostPath := filepath.Join(root, rel)

	cases := []struct {
		name     string
		err      error
		code     string
		relative string
	}{
		{"not_found", hostilePathError(root, "open", rel, fs.ErrNotExist), CodeNotFound, rel},
		{"permission", hostilePathError(root, "open", rel, fs.ErrPermission), CodePermissionDenied, rel},
		{"not_directory", hostilePathError(root, "open", rel, syscall.ENOTDIR), CodeNotDirectory, rel},
		{"is_directory", hostilePathError(root, "read", rel, syscall.EISDIR), CodeIsDirectory, rel},
		{"not_empty", hostilePathError(root, "remove", rel, syscall.ENOTEMPTY), CodeNotEmpty, rel},
		{"exists", hostilePathError(root, "mkdir", rel, fs.ErrExist), CodeExists, rel},
		{"name_too_long", hostilePathError(root, "open", rel, syscall.ENAMETOOLONG), CodeInvalidPath, rel},
		{"loop", hostilePathError(root, "open", rel, syscall.ELOOP), CodeInvalidPath, rel},
		{"io_error", hostilePathError(root, "read", rel, syscall.EIO), CodeIOError, rel},
		{"write_io", hostilePathError(root, "write", rel, syscall.EIO), CodeIOError, rel},
		{"stat_io", hostilePathError(root, "stat", rel, syscall.EIO), CodeIOError, rel},
		{"readdir_io", hostilePathError(root, "readdir", rel, syscall.EIO), CodeIOError, rel},
		{"unknown", hostilePathError(root, "read", rel, errors.New("host-qualified "+hostPath)), CodeIOError, rel},
		{"canceled", hostilePathError(root, "read", rel, context.Canceled), CodeIOError, ""},
		{"deadline", hostilePathError(root, "read", rel, context.DeadlineExceeded), CodeIOError, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.mapPathError(rel, tc.err)
			ce := assertNoRootLeak(t, err, tc.code, tc.relative, root, hostPath, "host-qualified")
			if tc.name == "io_error" && !strings.Contains(ce.Message, "input/output error") {
				t.Fatalf("io_error message lacks static errno context: %s", ce.Message)
			}
			if tc.name == "unknown" && strings.Contains(ce.Message, "host-qualified") {
				t.Fatalf("io_error copied raw error text: %s", ce.Message)
			}
		})
	}
}

func TestReadAllLimitedErrorSanitized(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	hostPath := filepath.Join(root, "secret.txt")
	hostile := &fs.PathError{Op: "read", Path: hostPath, Err: syscall.EIO}

	_, err := readAllLimited(errorReader{hostile}, 10)
	if err == nil {
		t.Fatal("expected a read error")
	}
	ce := a.mapPathError("secret.txt", err)
	assertNoRootLeak(t, ce, CodeIOError, "secret.txt", root, hostPath)
	if !strings.Contains(ce.Message, "input/output error") {
		t.Fatalf("message lacks static errno context: %s", ce.Message)
	}
}

func TestPublicErrorBoundary(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	hostPath := filepath.Join(root, "secret.txt")

	// A non-*Error is replaced by a generic io_error, never copied.
	raw := errors.New("host-qualified " + hostPath)
	err := a.publicError(raw)
	ce := assertNoRootLeak(t, err, CodeIOError, "", root, hostPath, "host-qualified")
	if ce.Message != "io error" {
		t.Fatalf("message = %q, want generic io error", ce.Message)
	}

	// A wrapped *Error is unwrapped; the wrapper text is discarded.
	inner := newError(CodeNotFound, "%q does not exist", "secret.txt")
	wrapped := fmt.Errorf("host-qualified %s: %w", hostPath, inner)
	err = a.publicError(wrapped)
	assertNoRootLeak(t, err, CodeNotFound, "secret.txt", root, hostPath, "host-qualified")

	// A *Error whose message accidentally contains the root is redacted.
	leaky := newError(CodeIOError, "io error for %q: %s", "secret.txt", hostPath)
	err = a.publicError(leaky)
	ce = assertNoRootLeak(t, err, CodeIOError, "secret.txt", root, hostPath)
	if !strings.Contains(ce.Message, "<workspace>") {
		t.Fatalf("message was not redacted: %s", ce.Message)
	}

	// Context errors keep their stable messages.
	err = a.publicError(context.Canceled)
	ce = assertNoRootLeak(t, err, CodeIOError, "", root, hostPath)
	if !strings.Contains(ce.Message, "cancelled") {
		t.Fatalf("message = %q", ce.Message)
	}

	// A root passed with a trailing separator is still redacted.
	t.Run("trailing separator", func(t *testing.T) {
		a2, root2 := newTestAdapter(t, WorkspaceOptions{Root: root + string(os.PathSeparator)})
		hostPath2 := filepath.Join(root2, "secret.txt")
		leaky := newError(CodeIOError, "io error for %q: %s", "secret.txt", hostPath2)
		err := a2.publicError(leaky)
		assertNoRootLeak(t, err, CodeIOError, "secret.txt", root2, hostPath2)
	})
}

func TestErrorsIsClassificationNotStringParsing(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	rel := "secret.txt"

	// A message that looks like an escape must not override errors.Is.
	err := a.mapPathError(rel, &textError{text: "path escapes from parent", err: fs.ErrNotExist})
	assertNoRootLeak(t, err, CodeNotFound, rel, root)

	// A message that looks like not_found must not override a real EIO.
	err = a.mapPathError(rel, &textError{text: "no such file or directory", err: syscall.EIO})
	assertNoRootLeak(t, err, CodeIOError, rel, root)

	// A message that looks like not_found must not override permission.
	err = a.mapPathError(rel, &textError{text: "no such file or directory", err: fs.ErrPermission})
	assertNoRootLeak(t, err, CodePermissionDenied, rel, root)
}

func TestReplaceRootPrefix(t *testing.T) {
	cases := []struct {
		name        string
		msg         string
		candidate   string
		replacement string
		want        string
	}{
		{"quoted relative", `io error for "a/file"`, "a/", "<workspace>/", `io error for "<workspace>/file"`},
		{"substring not mangled", `io error for "data/file"`, "a/", "<workspace>/", `io error for "data/file"`},
		{"space separated", `open /tmp/xyz/file: input/output error`, "/tmp/xyz/", "<workspace>/", `open <workspace>/file: input/output error`},
		{"exact absolute", `io error for "/tmp/xyz"`, "/tmp/xyz", "<workspace>", `io error for "<workspace>"`},
		{"start of string", `/tmp/xyz/file`, "/tmp/xyz/", "<workspace>/", `<workspace>/file`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replaceRootPrefix(tc.msg, tc.candidate, tc.replacement); got != tc.want {
				t.Fatalf("replaceRootPrefix(%q, %q) = %q, want %q", tc.msg, tc.candidate, got, tc.want)
			}
		})
	}
}

func TestHandleAllOperationsNoRootLeak(t *testing.T) {
	base := t.TempDir()
	marker := "root-marker-" + filepath.Base(base)
	root := filepath.Join(base, marker)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}

	writeHostFile(t, root, "file.txt", []byte("hello"))
	writeHostFile(t, root, "dir/child.txt", []byte("x"))
	writeHostFile(t, root, "invalid.bin", []byte{0xff})
	writeHostFile(t, root, "big.txt", []byte("0123456789"))
	writeHostFile(t, root, "full/child.txt", []byte("x"))
	writeHostFile(t, root, "locked/child.txt", []byte("x"))
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root, MaxBytes: 4, MaxEntries: 1})
	readOnly, _ := newTestAdapter(t, WorkspaceOptions{Root: root, ReadOnly: true})

	cases := []struct {
		name     string
		op       string
		args     map[string]any
		code     string
		relative string
	}{
		{"read_text missing", OpReadText, map[string]any{"path": "missing"}, CodeNotFound, "missing"},
		{"read_text dir", OpReadText, map[string]any{"path": "dir"}, CodeIsDirectory, "dir"},
		{"read_text invalid utf8", OpReadText, map[string]any{"path": "invalid.bin"}, CodeInvalidUTF8, "invalid.bin"},
		{"read_text too large", OpReadText, map[string]any{"path": "big.txt"}, CodeTooLarge, "big.txt"},
		{"read_text permission", OpReadText, map[string]any{"path": "locked/child.txt"}, CodePermissionDenied, "locked/child.txt"},
		{"write_text missing parent", OpWriteText, map[string]any{"path": "missing/x", "text": "x"}, CodeNotFound, "missing/x"},
		{"write_text dir", OpWriteText, map[string]any{"path": "dir", "text": "x"}, CodeIsDirectory, "dir"},
		{"write_text too large", OpWriteText, map[string]any{"path": "x", "text": "01234"}, CodeTooLarge, ""},
		{"read_bytes missing", OpReadBytes, map[string]any{"path": "missing"}, CodeNotFound, "missing"},
		{"read_bytes dir", OpReadBytes, map[string]any{"path": "dir"}, CodeIsDirectory, "dir"},
		{"read_bytes too large", OpReadBytes, map[string]any{"path": "big.txt"}, CodeTooLarge, "big.txt"},
		{"write_bytes invalid", OpWriteBytes, map[string]any{"path": "x", "base64": "!!!!"}, CodeInvalidBase64, ""},
		{"write_bytes missing parent", OpWriteBytes, map[string]any{"path": "missing/x", "base64": "eA=="}, CodeNotFound, "missing/x"},
		{"exists not dir", OpExists, map[string]any{"path": "file.txt/child"}, CodeNotDirectory, "file.txt/child"},
		{"exists permission", OpExists, map[string]any{"path": "locked/child.txt"}, CodePermissionDenied, "locked/child.txt"},
		{"listdir missing", OpListDir, map[string]any{"path": "missing"}, CodeNotFound, "missing"},
		{"listdir file", OpListDir, map[string]any{"path": "file.txt"}, CodeNotDirectory, "file.txt"},
		{"listdir too many", OpListDir, map[string]any{"path": "."}, CodeTooLarge, "."},
		{"listdir permission", OpListDir, map[string]any{"path": "locked"}, CodePermissionDenied, "locked"},
		{"mkdir exists", OpMkdir, map[string]any{"path": "dir"}, CodeExists, "dir"},
		{"mkdir missing parent", OpMkdir, map[string]any{"path": "missing/x"}, CodeNotFound, "missing/x"},
		{"remove not empty", OpRemove, map[string]any{"path": "full"}, CodeNotEmpty, "full"},
		{"remove root", OpRemove, map[string]any{"path": "."}, CodeInvalidPath, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Handle(context.Background(), tc.op, tc.args)
			assertNoRootLeak(t, err, tc.code, tc.relative, root, marker, filepath.Join(root, "file.txt"))
		})
	}

	for _, tc := range []struct {
		name string
		op   string
		args map[string]any
	}{
		{"write_text", OpWriteText, map[string]any{"path": "x", "text": "x"}},
		{"write_bytes", OpWriteBytes, map[string]any{"path": "x", "base64": "eA=="}},
		{"mkdir", OpMkdir, map[string]any{"path": "x"}},
		{"remove", OpRemove, map[string]any{"path": "file.txt"}},
	} {
		t.Run("readonly "+tc.name, func(t *testing.T) {
			_, err := readOnly.Handle(context.Background(), tc.op, tc.args)
			assertNoRootLeak(t, err, CodePermissionDenied, "", root, marker)
		})
	}
}
