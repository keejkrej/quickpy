package integration_test

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/keejkrej/quickpy"
)

// requireArtifactABI probes the real artifact with a trivial execution and
// skips when the artifact is not the requested ABI. The final M1 acceptance
// artifact is ABI 4; an older artifact is a pending build, not a test failure.
// The probe still goes through artifactPath, so the whole package skips
// cleanly when QUICKPY_TEST_ARTIFACT is unset.
func requireArtifactABI(t *testing.T, want uint32) {
	t.Helper()
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("abi")`}))
	if res.Guest.ABIVersion != want {
		t.Skipf("artifact ABI %d, want %d; this assertion requires the final ABI %d artifact", res.Guest.ABIVersion, want, want)
	}
}

// requireArtifactABIAtLeast probes the real artifact and skips when its ABI is
// older than want. It is used for semantics that are additive across ABIs, so
// an ABI 4 artifact still exercises the ABI 3 tool conformance.
func requireArtifactABIAtLeast(t *testing.T, want uint32) {
	t.Helper()
	r := newRuntime(t, quickpy.RuntimeOptions{})
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: `print("abi")`}))
	if res.Guest.ABIVersion < want {
		t.Skipf("artifact ABI %d, want >= %d; this assertion requires the final ABI %d artifact", res.Guest.ABIVersion, want, want)
	}
}

// requireABI3 is the common gate for ABI 3 semantics: structured tool error
// classes, error.tool_cause, engine identity, and the guest-side
// max_tool_calls cap. ABI 4 includes these semantics, so this gate accepts
// ABI 3 or newer.
func requireABI3(t *testing.T) {
	t.Helper()
	requireArtifactABIAtLeast(t, 3)
}

// requireABI4 is the common gate for ABI 4-only semantics: structured console
// and image output. It skips when the artifact is an older ABI so a pending
// build reports a clear skip instead of a misleading failure.
func requireABI4(t *testing.T) {
	t.Helper()
	requireArtifactABI(t, 4)
}

// requireToolCause checks the public Error.Tool shape. wantCallID is false for
// failures raised before a call is registered (ToolArgumentError and
// ToolLimitError), where the guest has no call id to report.
func requireToolCause(t *testing.T, tc *quickpy.ToolCause, wantTool, wantCode string, wantCallID bool) {
	t.Helper()
	if tc == nil {
		t.Fatal("Error.Tool is nil, want a structured ToolCause")
	}
	if tc.Tool != wantTool {
		t.Fatalf("ToolCause.Tool = %q, want %q", tc.Tool, wantTool)
	}
	if tc.Code != wantCode {
		t.Fatalf("ToolCause.Code = %q, want %q", tc.Code, wantCode)
	}
	if tc.Message == "" {
		t.Fatal("ToolCause.Message is empty")
	}
	if wantCallID && tc.CallID == "" {
		t.Fatal("ToolCause.CallID is empty, want the opaque guest call id")
	}
	if !wantCallID && tc.CallID != "" {
		t.Fatalf("ToolCause.CallID = %q, want empty for a pre-dispatch failure", tc.CallID)
	}
}

// toolEventCallID returns the CallID of the finished ToolEvent for name. It
// fails when the event is missing or has an empty CallID.
func toolEventCallID(t *testing.T, events []quickpy.Event, name string) string {
	t.Helper()
	for _, ev := range events {
		te, ok := ev.(quickpy.ToolEvent)
		if !ok || te.Name != name || te.Phase != "finished" {
			continue
		}
		if te.CallID == "" {
			t.Fatalf("finished ToolEvent for %q has an empty CallID", name)
		}
		return te.CallID
	}
	t.Fatalf("no finished ToolEvent for %q in %#v", name, events)
	return ""
}

// processRSSKiB reads the current process RSS with the same read-only OS
// method used by cmd/quickpy-spike: ps -o rss= -p <pid>. It is a coarse,
// process-level sample, not a per-execution memory measurement.
func processRSSKiB(t *testing.T) (int64, bool) {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
