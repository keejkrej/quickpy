package wasmguest

import (
	"context"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
)

// MeasureStartup measures warm per-instance startup through instantiation and
// _initialize. It does not send a start message or run source/tool work.
func (r *Runtime) MeasureStartup(ctx context.Context, samples int) ([]time.Duration, error) {
	if samples <= 0 {
		samples = 1
	}
	if err := r.measureOneStartup(ctx); err != nil {
		return nil, err
	}
	out := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		if err := r.measureOneStartup(ctx); err != nil {
			return nil, err
		}
		out = append(out, time.Since(start))
	}
	return out, nil
}

func (r *Runtime) measureOneStartup(ctx context.Context) error {
	stdout := newBoundedBuffer(r.opts.MaxOutputBytes)
	stderr := newBoundedBuffer(r.opts.MaxOutputBytes)
	modCfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions().
		WithStdout(stdout).
		WithStderr(stderr).
		WithStdin(strings.NewReader(""))
	inst, err := r.rt.InstantiateModule(ctx, r.compiled, modCfg)
	if err != nil {
		return err
	}
	defer func() { _ = inst.Close(context.Background()) }()
	if initFn := inst.ExportedFunction("_initialize"); initFn != nil {
		if _, err := initFn.Call(ctx); err != nil {
			return err
		}
	}
	return nil
}
