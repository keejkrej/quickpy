package quickpy

import (
	"context"
	"time"
)

// engine is the narrow boundary between the public SDK and the low-level
// wasmguest runtime. Tests inject a fake implementation.
type engine interface {
	Execute(ctx context.Context, req engineRequest) (engineResult, error)
	Close(ctx context.Context) error
}

type engineFactory func(ctx context.Context, opts engineOptions) (engine, error)

type engineOptions struct {
	Artifact        ArtifactSource
	MaxMemoryBytes  int64
	MaxMessageBytes int
	MaxOutputBytes  int
	MaxStateBytes   int
	DefaultTimeout  time.Duration
}

type engineRequest struct {
	Source       string
	Filename     string
	InitialState Value
	Timeout      time.Duration

	MaxMemoryBytes     int64
	MaxMessageBytes    int
	MaxOutputBytes     int
	MaxToolResultBytes int
	MaxStateBytes      int
	MaxToolCalls       int

	MaxConcurrentToolCalls int
	MaxPendingCalls        int
	MaxResumes             int

	Tools    map[string]ToolHandler
	Observer func(observation)
}

type engineResult struct {
	Status       Status
	State        Value
	StateWritten bool
	ToolCalls    int
	Duration     time.Duration
	Guest        GuestInfo
}
