//go:build quickpy_embed_example

package main

import (
	_ "embed"
	"errors"
)

// quickpyWASM is the generated guest artifact. It is copied into this ignored
// path by build.sh and is never committed.
//
//go:embed quickpy.wasm
var quickpyWASM []byte

func embeddedArtifact() ([]byte, error) {
	if len(quickpyWASM) == 0 {
		return nil, errors.New("embedded quickpy.wasm is empty")
	}
	return quickpyWASM, nil
}
