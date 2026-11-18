// Package integration contains real-artifact integration tests for the public
// QuickPy Go SDK.
//
// The tests are gated by the QUICKPY_TEST_ARTIFACT environment variable. When
// it is unset, every test skips, so the default `go test ./...` never requires
// a built guest artifact. When it is set, it must point at a real MicroPython
// QuickPy artifact (ABI 2), for example:
//
//	QUICKPY_TEST_ARTIFACT=guest/build/quickpy.wasm go test -count=1 -v ./integration
//
// Relative paths are resolved from the repository root, not from the
// integration directory. The tests exercise the public package
// github.com/keejkrej/quickpy only; they do not import internal/wasmguest.
package integration
