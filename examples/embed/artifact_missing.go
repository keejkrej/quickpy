//go:build !quickpy_embed_example

package main

import "errors"

// embeddedArtifact is the default-build stub. It keeps `go build ./...` and
// `go test ./...` working without a generated guest artifact; it never
// pretends to be a real WASM module.
func embeddedArtifact() ([]byte, error) {
	return nil, errors.New("no embedded artifact: run examples/embed/build.sh and build with -tags quickpy_embed_example")
}
