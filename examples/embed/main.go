// Command embed is a reference for shipping QuickPy as a single native binary
// with the guest artifact embedded via go:embed.
//
// The default build (no build tag) compiles but reports that no artifact was
// embedded, so `go build ./...` and `go test ./...` never require a generated
// asset. To build the real example, prepare the ignored asset and build with
// the quickpy_embed_example tag:
//
//	QUICKPY_WASM=guest/build/quickpy.wasm ./examples/embed/build.sh
//
// The resulting binary is self-contained: it does not read the artifact from
// disk, does not need a writable TMPDIR, and can run from any working
// directory.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/keejkrej/quickpy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embed example:", err)
		os.Exit(1)
	}
}

func run() error {
	artifact, err := embeddedArtifact()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(artifact)
	fmt.Printf("artifact: %d bytes sha256=%s\n", len(artifact), hex.EncodeToString(sum[:]))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, err := quickpy.New(quickpy.RuntimeOptions{
		Artifact:       quickpy.ArtifactSource{Bytes: artifact},
		MaxMemoryBytes: 32 << 20,
		MaxExecutions:  1,
		DefaultTimeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("quickpy.New: %w", err)
	}
	defer func() { _ = rt.Close(context.Background()) }()

	// Generate the Python source at runtime. Nothing about this program is
	// baked into the binary beyond the guest artifact itself.
	const n = 7
	source := fmt.Sprintf("total = 0\nfor i in range(%d):\n    total += i * i\nprint('generated total:', total)\n", n)

	e, err := rt.Execute(ctx, quickpy.ExecuteRequest{Source: source, Filename: "generated.py"})
	if err != nil {
		return fmt.Errorf("Execute: %w", err)
	}
	res, err := e.Wait(ctx)
	if err != nil {
		return fmt.Errorf("Wait: %w", err)
	}
	if res.Status != quickpy.StatusCompleted {
		return fmt.Errorf("status = %s", res.Status)
	}
	if res.Guest.ABIVersion != 4 {
		return fmt.Errorf("ABI version = %d, want 4", res.Guest.ABIVersion)
	}
	if res.Guest.Engine != "micropython" || res.Guest.Version == "" || res.Guest.Version == "unknown" {
		return fmt.Errorf("guest identity = %+v", res.Guest)
	}

	for _, ev := range res.Outputs {
		if text, ok := ev.(quickpy.TextEvent); ok {
			fmt.Print(text.Text)
		}
	}
	fmt.Printf("status=%s guest: engine=%s version=%s abi=%d protocol=%s\n",
		res.Status, res.Guest.Engine, res.Guest.Version, res.Guest.ABIVersion, res.Guest.ProtocolVersion)
	return nil
}
