package quickpy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syntheticArtifact reads a committed synthetic guest fixture. These fixtures
// are small, checked-in test inputs, so the bytes path can be exercised without
// a generated MicroPython artifact.
func syntheticArtifact(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("internal", "wasmguest", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestNewPlumbsArtifactBytesToEngine(t *testing.T) {
	original := []byte("artifact-bytes-transport")
	caller := append([]byte(nil), original...)

	var got engineOptions
	r, err := newRuntime(context.Background(), RuntimeOptions{
		Artifact:       ArtifactSource{Bytes: caller},
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
	}, func(ctx context.Context, eo engineOptions) (engine, error) {
		got = eo
		return &fakeEngine{}, nil
	})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer r.Close(context.Background())

	if got.Artifact.Path != "" {
		t.Fatalf("engine path = %q, want empty for bytes artifact", got.Artifact.Path)
	}
	if !bytes.Equal(got.Artifact.Bytes, original) {
		t.Fatalf("engine bytes = %q, want %q", got.Artifact.Bytes, original)
	}

	// The public layer owns a copy: mutating the caller's slice after New must
	// not change what the engine received or what the runtime retained.
	caller[0] = 'X'
	if !bytes.Equal(got.Artifact.Bytes, original) {
		t.Fatalf("engine bytes changed after caller mutation: %q", got.Artifact.Bytes)
	}
	if !bytes.Equal(r.opts.Artifact.Bytes, original) {
		t.Fatalf("runtime bytes changed after caller mutation: %q", r.opts.Artifact.Bytes)
	}
}

func TestNewPlumbsArtifactPathToEngine(t *testing.T) {
	var got engineOptions
	r, err := newRuntime(context.Background(), RuntimeOptions{
		Artifact:       ArtifactSource{Path: "guest/build/quickpy.wasm"},
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
	}, func(ctx context.Context, eo engineOptions) (engine, error) {
		got = eo
		return &fakeEngine{}, nil
	})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer r.Close(context.Background())

	if got.Artifact.Path != "guest/build/quickpy.wasm" {
		t.Fatalf("engine path = %q", got.Artifact.Path)
	}
	if got.Artifact.Bytes != nil {
		t.Fatalf("engine bytes = %v, want nil for path artifact", got.Artifact.Bytes)
	}
}

func TestNewFromBytesWithUnwritableTMPDIR(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory is still writable")
	}
	wasm := syntheticArtifact(t, "completed.wasm")

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	t.Setenv("TMPDIR", dir)

	r, err := New(RuntimeOptions{
		Artifact:       ArtifactSource{Bytes: wasm},
		MaxMemoryBytes: 16 << 20,
		MaxExecutions:  1,
		DefaultTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New from bytes with read-only TMPDIR: %v", err)
	}
	defer r.Close(context.Background())

	e, err := r.Execute(context.Background(), ExecuteRequest{Source: `print("a")`, Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	res, err := e.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Guest.ABIVersion != 4 || res.Guest.Engine != "micropython" || res.Guest.Version != "1.29.0" {
		t.Fatalf("guest identity = %+v", res.Guest)
	}
	if len(res.Outputs) != 1 {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	if text, ok := res.Outputs[0].(TextEvent); !ok || text.Text != "hello from synthetic guest" {
		t.Fatalf("output = %#v", res.Outputs[0])
	}
}

func TestNewFromBytesWithMissingTMPDIR(t *testing.T) {
	wasm := syntheticArtifact(t, "completed.wasm")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))

	r, err := New(RuntimeOptions{
		Artifact:       ArtifactSource{Bytes: wasm},
		MaxMemoryBytes: 16 << 20,
		MaxExecutions:  1,
		DefaultTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New from bytes with missing TMPDIR: %v", err)
	}
	defer r.Close(context.Background())

	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "pass", Filename: "main.py"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	res, err := e.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestNewFromBytesInvalidArtifact(t *testing.T) {
	_, err := New(RuntimeOptions{
		Artifact:       ArtifactSource{Bytes: []byte("not a wasm module")},
		MaxMemoryBytes: 16 << 20,
		MaxExecutions:  1,
	})
	if err == nil {
		t.Fatal("expected compile error")
	}
	var qe *Error
	if !errors.As(err, &qe) {
		t.Fatalf("error type = %T", err)
	}
	if qe.Kind != KindProtocolError {
		t.Fatalf("kind = %q, want %q", qe.Kind, KindProtocolError)
	}
	if !strings.Contains(qe.Message, "compile artifact") {
		t.Fatalf("message = %q", qe.Message)
	}
}

func TestNewFromEmptyBytesIsInvalidRequest(t *testing.T) {
	_, err := New(RuntimeOptions{
		Artifact:       ArtifactSource{Bytes: []byte{}},
		MaxMemoryBytes: 16 << 20,
		MaxExecutions:  1,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindInvalidRequest {
		t.Fatalf("error = %v", err)
	}
}
