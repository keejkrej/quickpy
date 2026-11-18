package quickpybridge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/keejkrej/quickpy"
	"github.com/keejkrej/quickpy/capability"
)

// Compile-time checks that the real types satisfy the bridge's small
// interfaces.
var (
	_ ToolRegistrar = (*quickpy.Runtime)(nil)
	_ adapter       = (*capability.Adapter)(nil)
)

type stubAdapter struct {
	ops    []string
	handle func(ctx context.Context, operation string, args map[string]any) (any, error)
}

func (s *stubAdapter) Operations() []string { return s.ops }

func (s *stubAdapter) Handle(ctx context.Context, operation string, args map[string]any) (any, error) {
	if s.handle != nil {
		return s.handle(ctx, operation, args)
	}
	return nil, nil
}

type fakeRegistrar struct {
	names         []string
	handlers      map[string]quickpy.ToolHandler
	failAt        int // 1-based registration attempt that fails; 0 means never
	unregistered  []string
	unregisterErr error
}

func (f *fakeRegistrar) RegisterTool(name string, h quickpy.ToolHandler) error {
	if f.failAt > 0 && len(f.names)+1 == f.failAt {
		return errors.New("register failed")
	}
	if f.handlers == nil {
		f.handlers = make(map[string]quickpy.ToolHandler)
	}
	if _, ok := f.handlers[name]; ok {
		return errors.New("duplicate tool " + name)
	}
	f.names = append(f.names, name)
	f.handlers[name] = h
	return nil
}

func (f *fakeRegistrar) UnregisterTool(name string) error {
	f.unregistered = append(f.unregistered, name)
	delete(f.handlers, name)
	return f.unregisterErr
}

// registerOnly deliberately does not implement UnregisterTool.
type registerOnly struct {
	names    []string
	failAt   int
	handlers map[string]quickpy.ToolHandler
}

func (f *registerOnly) RegisterTool(name string, h quickpy.ToolHandler) error {
	if f.failAt > 0 && len(f.names)+1 == f.failAt {
		return errors.New("register failed")
	}
	if f.handlers == nil {
		f.handlers = make(map[string]quickpy.ToolHandler)
	}
	f.names = append(f.names, name)
	f.handlers[name] = h
	return nil
}

func TestInstallRegistersSortedOperations(t *testing.T) {
	reg := &fakeRegistrar{}
	a := &stubAdapter{ops: []string{"fs_write_text", "fs_read_text"}}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	want := []string{"fs_read_text", "fs_write_text"}
	if !reflect.DeepEqual(reg.names, want) {
		t.Fatalf("registered %v, want %v", reg.names, want)
	}
	for _, name := range want {
		if reg.handlers[name] == nil {
			t.Fatalf("handler for %q is nil", name)
		}
	}
}

func TestInstallNilAdapter(t *testing.T) {
	if err := Install(&fakeRegistrar{}, nil); err == nil {
		t.Fatal("Install(nil adapter) succeeded")
	}
}

func TestInstallNilRegistrar(t *testing.T) {
	if err := install(nil, &stubAdapter{}); err == nil {
		t.Fatal("install(nil registrar) succeeded")
	}
}

func TestHandlerConvertsArgsAndResult(t *testing.T) {
	var gotOp string
	var gotArgs map[string]any
	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_read_text"},
		handle: func(_ context.Context, op string, args map[string]any) (any, error) {
			gotOp = op
			gotArgs = args
			return map[string]any{"text": "hello"}, nil
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	h := reg.handlers["fs_read_text"]
	got, err := h(context.Background(), quickpy.ToolCall{
		Name: "fs_read_text",
		Args: map[string]any{"path": "a.txt", "encoding": "utf-8"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if gotOp != "fs_read_text" {
		t.Fatalf("operation = %q", gotOp)
	}
	if !reflect.DeepEqual(gotArgs, map[string]any{"path": "a.txt", "encoding": "utf-8"}) {
		t.Fatalf("args = %#v", gotArgs)
	}
	if !reflect.DeepEqual(got, map[string]any{"text": "hello"}) {
		t.Fatalf("result = %#v", got)
	}
}

func TestHandlerNilArgsBecomesEmptyObject(t *testing.T) {
	var gotArgs map[string]any
	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_exists"},
		handle: func(_ context.Context, _ string, args map[string]any) (any, error) {
			gotArgs = args
			return map[string]any{"exists": false}, nil
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := reg.handlers["fs_exists"](context.Background(), quickpy.ToolCall{}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if gotArgs == nil || len(gotArgs) != 0 {
		t.Fatalf("args = %#v, want empty non-nil map", gotArgs)
	}
}

func TestHandlerRejectsNonObjectArgs(t *testing.T) {
	called := false
	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_read_text"},
		handle: func(context.Context, string, map[string]any) (any, error) {
			called = true
			return nil, nil
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	_, err := reg.handlers["fs_read_text"](context.Background(), quickpy.ToolCall{Args: "not an object"})
	if called {
		t.Fatal("adapter was called for non-object args")
	}
	var te *quickpy.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %T %v, want *quickpy.ToolError", err, err)
	}
	if te.Code != capability.CodeToolArgument {
		t.Fatalf("code = %q, want %q", te.Code, capability.CodeToolArgument)
	}
}

func TestHandlerPropagatesCapabilityError(t *testing.T) {
	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_read_text"},
		handle: func(context.Context, string, map[string]any) (any, error) {
			return nil, &capability.Error{Code: capability.CodeNotFound, Message: "missing"}
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	_, err := reg.handlers["fs_read_text"](context.Background(), quickpy.ToolCall{})
	var te *quickpy.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %T %v, want *quickpy.ToolError", err, err)
	}
	if te.Code != capability.CodeNotFound || te.Message != "missing" {
		t.Fatalf("tool error = %#v", te)
	}
}

func TestHandlerPassesContext(t *testing.T) {
	type key struct{}
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), key{}, "value"), deadline)
	defer cancel()

	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_exists"},
		handle: func(got context.Context, _ string, _ map[string]any) (any, error) {
			if got.Value(key{}) != "value" {
				t.Errorf("context value not forwarded")
			}
			gotDeadline, ok := got.Deadline()
			if !ok || !gotDeadline.Equal(deadline) {
				t.Errorf("deadline = %v, %v; want %v", gotDeadline, ok, deadline)
			}
			return map[string]any{"exists": true}, nil
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := reg.handlers["fs_exists"](ctx, quickpy.ToolCall{}); err != nil {
		t.Fatalf("handler: %v", err)
	}
}

func TestHandlerPreservesContextCancellation(t *testing.T) {
	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_read_text"},
		handle: func(context.Context, string, map[string]any) (any, error) {
			return nil, &capability.Error{Code: capability.CodeIOError, Message: "operation cancelled"}
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := reg.handlers["fs_read_text"](ctx, quickpy.ToolCall{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestHandlerPreservesContextDeadline(t *testing.T) {
	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_read_text"},
		handle: func(context.Context, string, map[string]any) (any, error) {
			return nil, &capability.Error{Code: capability.CodeIOError, Message: "operation deadline exceeded"}
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := reg.handlers["fs_read_text"](ctx, quickpy.ToolCall{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestHandlerUnknownErrorUnchanged(t *testing.T) {
	boom := errors.New("boom")
	reg := &fakeRegistrar{}
	a := &stubAdapter{
		ops: []string{"fs_exists"},
		handle: func(context.Context, string, map[string]any) (any, error) {
			return nil, boom
		},
	}
	if err := install(reg, a); err != nil {
		t.Fatalf("install: %v", err)
	}
	_, err := reg.handlers["fs_exists"](context.Background(), quickpy.ToolCall{})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want original boom", err)
	}
	var te *quickpy.ToolError
	if errors.As(err, &te) {
		t.Fatalf("unknown error was converted to ToolError: %#v", te)
	}
}

func TestInstallRollsBackOnFailure(t *testing.T) {
	reg := &fakeRegistrar{failAt: 2}
	a := &stubAdapter{ops: []string{"fs_read_text", "fs_write_text", "fs_exists"}}
	err := install(reg, a)
	if err == nil {
		t.Fatal("install succeeded despite registration failure")
	}
	if !strings.Contains(err.Error(), "register failed") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(reg.unregistered, []string{"fs_exists"}) {
		t.Fatalf("unregistered = %v", reg.unregistered)
	}
	if len(reg.handlers) != 0 {
		t.Fatalf("handlers left after rollback: %v", reg.handlers)
	}
}

func TestInstallWithoutRollbackReportsPartial(t *testing.T) {
	reg := &registerOnly{failAt: 2}
	a := &stubAdapter{ops: []string{"fs_read_text", "fs_write_text"}}
	err := install(reg, a)
	if err == nil {
		t.Fatal("install succeeded despite registration failure")
	}
	if !strings.Contains(err.Error(), "partially installed") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "fs_read_text") {
		t.Fatalf("error does not name partial tool: %v", err)
	}
}

func TestInstallDuplicateOperationRollsBack(t *testing.T) {
	reg := &fakeRegistrar{}
	a := &stubAdapter{ops: []string{"fs_read_text", "fs_read_text"}}
	err := install(reg, a)
	if err == nil {
		t.Fatal("install succeeded despite duplicate operation")
	}
	if !strings.Contains(err.Error(), "duplicate tool") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(reg.unregistered, []string{"fs_read_text"}) {
		t.Fatalf("unregistered = %v", reg.unregistered)
	}
}

func TestInstallRollbackErrorIsReported(t *testing.T) {
	reg := &fakeRegistrar{failAt: 2, unregisterErr: errors.New("unregister failed")}
	a := &stubAdapter{ops: []string{"fs_read_text", "fs_write_text"}}
	err := install(reg, a)
	if err == nil {
		t.Fatal("install succeeded despite registration failure")
	}
	if !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("error = %v", err)
	}
}
