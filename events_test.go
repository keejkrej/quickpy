package quickpy

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestEventOrdering(t *testing.T) {
	var mu sync.Mutex
	var events []Event
	sink := func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			req.Observer(observation{Kind: observationText, Text: "a"})
			req.Observer(observation{Kind: observationTool, ToolName: "t", CallID: 7, Phase: "started"})
			req.Observer(observation{Kind: observationText, Text: "b"})
			req.Observer(observation{Kind: observationTool, ToolName: "t", CallID: 7, Phase: "finished"})
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{MaxMemoryBytes: 1 << 20, MaxExecutions: 1, EventSink: sink}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{
		TextEvent{Text: "a"},
		ToolEvent{Phase: "started", Name: "t", CallID: "7"},
		TextEvent{Text: "b"},
		ToolEvent{Phase: "finished", Name: "t", CallID: "7"},
	}
	mu.Lock()
	got := append([]Event(nil), events...)
	mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	wantOutputs := []Event{TextEvent{Text: "a"}, TextEvent{Text: "b"}}
	if !reflect.DeepEqual(res.Outputs, wantOutputs) {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}

func TestEventSinkOverride(t *testing.T) {
	var runtimeEvents, execEvents []Event
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			req.Observer(observation{Kind: observationText, Text: "x"})
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
		EventSink:      func(ev Event) { runtimeEvents = append(runtimeEvents, ev) },
	}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{
		Source:    "x",
		Filename:  "x.py",
		EventSink: func(ev Event) { execEvents = append(execEvents, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runtimeEvents) != 0 {
		t.Fatalf("runtime sink got %d events", len(runtimeEvents))
	}
	if len(execEvents) != 1 {
		t.Fatalf("exec sink got %d events", len(execEvents))
	}
}

func TestSinkPanic(t *testing.T) {
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			req.Observer(observation{Kind: observationText, Text: "x"})
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
		EventSink:      func(ev Event) { panic("boom") },
	}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindHostFault {
		t.Fatalf("got %v, want HostFault", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestOutputClipping(t *testing.T) {
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			req.Observer(observation{Kind: observationText, Text: "12345"})
			req.Observer(observation{Kind: observationText, Text: "67890"})
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
		MaxOutputBytes: 7,
	}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindLimitExceeded {
		t.Fatalf("got %v, want LimitExceeded", err)
	}
	if res.Status != StatusLimitExceeded {
		t.Fatalf("status = %q", res.Status)
	}
	want := []Event{TextEvent{Text: "12345"}, TextEvent{Text: "67"}}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}

func TestConsoleAndImageEvents(t *testing.T) {
	var events []Event
	eng := &fakeEngine{
		executeFn: func(ctx context.Context, req engineRequest) (engineResult, error) {
			req.Observer(observation{Kind: observationConsole, Level: "warn", Text: "careful"})
			req.Observer(observation{Kind: observationImage, MIME: "image/png", Data: []byte{1, 2, 3}})
			return engineResult{Status: StatusCompleted}, nil
		},
	}
	r := testRuntime(t, RuntimeOptions{
		MaxMemoryBytes: 1 << 20,
		MaxExecutions:  1,
		EventSink:      func(ev Event) { events = append(events, ev) },
	}, eng)
	e, err := r.Execute(context.Background(), ExecuteRequest{Source: "x", Filename: "x.py"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{
		ConsoleEvent{Level: "warn", Text: "careful"},
		ImageEvent{MIME: "image/png", Data: []byte{1, 2, 3}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v", events)
	}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs = %#v", res.Outputs)
	}
}
