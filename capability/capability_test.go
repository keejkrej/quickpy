package capability

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOperationsDeterministic(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})

	want := []string{
		"fs_exists",
		"fs_listdir",
		"fs_mkdir",
		"fs_read_bytes",
		"fs_read_text",
		"fs_remove",
		"fs_write_bytes",
		"fs_write_text",
	}
	got := a.Operations()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Operations() = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(a.Operations(), got) {
		t.Fatal("Operations() is not deterministic")
	}

	// The returned slice must be a copy.
	got[0] = "mutated"
	if a.Operations()[0] != "fs_exists" {
		t.Fatal("Operations() returned an aliased slice")
	}
}

func TestUnknownOperation(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})
	_, err := a.Handle(context.Background(), "fs_nope", nil)
	wantCode(t, err, CodeToolArgument)
}

func TestArgumentValidation(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})

	cases := []struct {
		name string
		op   string
		args map[string]any
	}{
		{"read_text nil args", OpReadText, nil},
		{"read_text path type", OpReadText, map[string]any{"path": 1}},
		{"read_text unknown key", OpReadText, map[string]any{"path": "x", "bogus": true}},
		{"read_text encoding type", OpReadText, map[string]any{"path": "x", "encoding": 1}},
		{"read_text encoding value", OpReadText, map[string]any{"path": "x", "encoding": "latin-1"}},
		{"write_text missing text", OpWriteText, map[string]any{"path": "x"}},
		{"write_text text type", OpWriteText, map[string]any{"path": "x", "text": 1}},
		{"write_bytes missing base64", OpWriteBytes, map[string]any{"path": "x"}},
		{"write_bytes base64 type", OpWriteBytes, map[string]any{"path": "x", "base64": 1}},
		{"exists missing path", OpExists, nil},
		{"mkdir missing path", OpMkdir, nil},
		{"mkdir parents type", OpMkdir, map[string]any{"path": "x", "parents": "yes"}},
		{"remove missing path", OpRemove, nil},
		{"listdir path type", OpListDir, map[string]any{"path": 1}},
		{"listdir unknown key", OpListDir, map[string]any{"path": "x", "bogus": 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Handle(context.Background(), tc.op, tc.args)
			wantCode(t, err, CodeToolArgument)
		})
	}
}

func TestUnknownKeysAreDeterministic(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})
	_, err := a.Handle(context.Background(), OpReadText, map[string]any{
		"path":  "x",
		"zeta":  1,
		"alpha": 2,
	})
	ce := wantCode(t, err, CodeToolArgument)
	if !strings.Contains(ce.Message, `"alpha"`) {
		t.Fatalf("message %q does not report the sorted first unknown key", ce.Message)
	}
}

func TestNilArgsListDirDefaultsToRoot(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})
	res := mustHandle(t, a, OpListDir, nil)
	entries, ok := res["entries"].([]any)
	if !ok {
		t.Fatalf("entries type %T", res["entries"])
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want empty", entries)
	}
}

func TestErrorType(t *testing.T) {
	var nilErr *Error
	if got := nilErr.Error(); got != "<nil>" {
		t.Fatalf("nil Error() = %q", got)
	}

	err := newError(CodeNotFound, "missing %q", "x")
	if !strings.Contains(err.Error(), CodeNotFound) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Error() = %q", err.Error())
	}

	var target *Error
	if !errors.As(error(err), &target) {
		t.Fatal("errors.As failed")
	}
	if target.Code != CodeNotFound {
		t.Fatalf("Code = %q", target.Code)
	}
}

func TestClose(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_, err := a.Handle(context.Background(), OpListDir, nil)
	wantCode(t, err, CodeIOError)
	if len(a.Operations()) != 8 {
		t.Fatal("Operations() changed after Close")
	}
}

func TestContextCancellation(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Handle(ctx, OpListDir, nil)
	ce := wantCode(t, err, CodeIOError)
	if !strings.Contains(ce.Message, "cancelled") {
		t.Fatalf("message = %q", ce.Message)
	}

	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	_, err = a.Handle(deadline, OpListDir, nil)
	ce = wantCode(t, err, CodeIOError)
	if !strings.Contains(ce.Message, "deadline") {
		t.Fatalf("message = %q", ce.Message)
	}

	// A nil context is treated as Background.
	if _, err := a.Handle(nil, OpListDir, nil); err != nil {
		t.Fatalf("nil context: %v", err)
	}
}

func TestNewWorkspaceOptions(t *testing.T) {
	if _, err := NewWorkspace(WorkspaceOptions{}); err == nil {
		t.Fatal("empty root accepted")
	} else {
		wantCode(t, err, CodeInvalidArgument)
	}

	dir := t.TempDir()
	for _, opts := range []WorkspaceOptions{
		{Root: dir, MaxBytes: -1},
		{Root: dir, MaxBytes: math.MaxInt64},
		{Root: dir, MaxEntries: -1},
		{Root: dir, MaxEntries: math.MaxInt},
	} {
		if _, err := NewWorkspace(opts); err == nil {
			t.Fatalf("options %+v accepted", opts)
		} else {
			wantCode(t, err, CodeInvalidArgument)
		}
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkspace(WorkspaceOptions{Root: file}); err == nil {
		t.Fatal("file root accepted")
	}

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: dir})
	if a.maxBytes != DefaultMaxBytes {
		t.Fatalf("maxBytes = %d, want %d", a.maxBytes, DefaultMaxBytes)
	}
	if a.maxEntries != DefaultMaxEntries {
		t.Fatalf("maxEntries = %d, want %d", a.maxEntries, DefaultMaxEntries)
	}

	b, _ := newTestAdapter(t, WorkspaceOptions{Root: dir, MaxBytes: 7, MaxEntries: 3})
	if b.maxBytes != 7 || b.maxEntries != 3 {
		t.Fatalf("explicit limits not preserved: %d, %d", b.maxBytes, b.maxEntries)
	}
}

func TestResultTypes(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})

	res := mustHandle(t, a, OpWriteText, map[string]any{"path": "a.txt", "text": "hi"})
	if _, ok := res["written"].(int64); !ok {
		t.Fatalf("written type %T", res["written"])
	}

	res = mustHandle(t, a, OpReadText, map[string]any{"path": "a.txt"})
	if _, ok := res["text"].(string); !ok {
		t.Fatalf("text type %T", res["text"])
	}

	res = mustHandle(t, a, OpReadBytes, map[string]any{"path": "a.txt"})
	if _, ok := res["base64"].(string); !ok {
		t.Fatalf("base64 type %T", res["base64"])
	}

	res = mustHandle(t, a, OpExists, map[string]any{"path": "a.txt"})
	if _, ok := res["exists"].(bool); !ok {
		t.Fatalf("exists type %T", res["exists"])
	}

	res = mustHandle(t, a, OpMkdir, map[string]any{"path": "d"})
	if _, ok := res["created"].(bool); !ok {
		t.Fatalf("created type %T", res["created"])
	}

	res = mustHandle(t, a, OpListDir, map[string]any{"path": "."})
	entries, ok := res["entries"].([]any)
	if !ok {
		t.Fatalf("entries type %T", res["entries"])
	}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("entry type %T", raw)
		}
		if _, ok := entry["name"].(string); !ok {
			t.Fatalf("name type %T", entry["name"])
		}
		if _, ok := entry["type"].(string); !ok {
			t.Fatalf("type type %T", entry["type"])
		}
		if _, ok := entry["size"].(int64); !ok {
			t.Fatalf("size type %T", entry["size"])
		}
	}

	res = mustHandle(t, a, OpRemove, map[string]any{"path": "a.txt"})
	if _, ok := res["removed"].(bool); !ok {
		t.Fatalf("removed type %T", res["removed"])
	}
}

func TestRootPathNotLeakedInErrors(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "file.txt", []byte("hello"))
	writeHostFile(t, root, "dir/child.txt", []byte("x"))
	writeHostFile(t, root, "invalid.bin", []byte{0xff})

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root, MaxBytes: 4})

	checks := []struct {
		name string
		op   string
		args map[string]any
	}{
		{"not_found", OpReadText, map[string]any{"path": "missing"}},
		{"invalid_path", OpReadText, map[string]any{"path": "../escape"}},
		{"is_directory", OpReadText, map[string]any{"path": "dir"}},
		{"not_directory", OpListDir, map[string]any{"path": "file.txt"}},
		{"too_large", OpReadText, map[string]any{"path": "file.txt"}},
		{"invalid_utf8", OpReadText, map[string]any{"path": "invalid.bin"}},
		{"invalid_base64", OpWriteBytes, map[string]any{"path": "x", "base64": "!!!!"}},
		{"exists", OpMkdir, map[string]any{"path": "dir"}},
		{"not_empty", OpRemove, map[string]any{"path": "dir"}},
		{"tool_argument", "fs_nope", nil},
	}

	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Handle(context.Background(), tc.op, tc.args)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), root) {
				t.Fatalf("error leaks the workspace root: %v", err)
			}
		})
	}

	readOnly, _ := newTestAdapter(t, WorkspaceOptions{Root: root, ReadOnly: true})
	_, err := readOnly.Handle(context.Background(), OpWriteText, map[string]any{"path": "x", "text": "y"})
	wantCode(t, err, CodePermissionDenied)
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error leaks the workspace root: %v", err)
	}

	// The sanitizer itself must handle a root-qualified post-open PathError,
	// not only errors produced by ordinary open calls.
	t.Run("sanitizer", func(t *testing.T) {
		hostPath := filepath.Join(root, "file.txt")
		hostile := &fs.PathError{Op: "read", Path: hostPath, Err: syscall.EIO}
		err := a.mapPathError("file.txt", hostile)
		assertNoRootLeak(t, err, CodeIOError, "file.txt", root, hostPath)
	})
}
