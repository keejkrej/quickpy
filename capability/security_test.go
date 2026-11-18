package capability

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSymlinkEscapeDenied(t *testing.T) {
	skipOnWindows(t)

	root := t.TempDir()
	outside := t.TempDir()
	writeHostFile(t, outside, "secret.txt", []byte("secret"))

	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	// A relative symlink that climbs out of the root is equally denied.
	if err := os.Symlink("../"+filepath.Base(outside), filepath.Join(root, "relative")); err != nil {
		t.Fatal(err)
	}

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})
	ctx := context.Background()

	for _, p := range []string{"escape/secret.txt", "relative/secret.txt"} {
		_, err := a.Handle(ctx, OpReadText, map[string]any{"path": p})
		ce := wantCode(t, err, CodeInvalidPath)
		if strings.Contains(ce.Message, outside) {
			t.Fatalf("error leaks the outside path: %v", ce)
		}

		_, err = a.Handle(ctx, OpWriteText, map[string]any{"path": p, "text": "pwned"})
		wantCode(t, err, CodeInvalidPath)

		_, err = a.Handle(ctx, OpExists, map[string]any{"path": p})
		wantCode(t, err, CodeInvalidPath)
	}

	_, err := a.Handle(ctx, OpListDir, map[string]any{"path": "escape"})
	wantCode(t, err, CodeInvalidPath)

	// Listing the root must not follow the escaping symlink for metadata.
	res := mustHandle(t, a, OpListDir, map[string]any{"path": "."})
	found := false
	for _, raw := range res["entries"].([]any) {
		entry := raw.(map[string]any)
		if entry["name"].(string) == "escape" {
			found = true
			if got := entry["type"].(string); got != "symlink" {
				t.Fatalf("escape entry type = %q, want symlink", got)
			}
		}
	}
	if !found {
		t.Fatal("listdir did not report the escaping symlink")
	}

	// exists uses lstat, so the link itself exists without traversal.
	res = mustHandle(t, a, OpExists, map[string]any{"path": "escape"})
	if !res["exists"].(bool) {
		t.Fatal("escaping symlink reported as missing")
	}

	// A final-component symlink to an outside file is replaced by an atomic
	// write, not followed.
	writeHostFile(t, outside, "target.txt", []byte("outside"))
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(root, "filelink")); err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(ctx, OpReadText, map[string]any{"path": "filelink"})
	wantCode(t, err, CodeInvalidPath)
	mustHandle(t, a, OpWriteText, map[string]any{"path": "filelink", "text": "replaced"})
	if got := string(readHostFile(t, outside, "target.txt")); got != "outside" {
		t.Fatalf("write followed the final symlink: outside target = %q", got)
	}
	if got := string(readHostFile(t, root, "filelink")); got != "replaced" {
		t.Fatalf("filelink content = %q", got)
	}

	if got := string(readHostFile(t, outside, "secret.txt")); got != "secret" {
		t.Fatalf("outside sentinel changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("outside file created: %v", err)
	}
}

func TestSymlinkInsideAllowed(t *testing.T) {
	skipOnWindows(t)

	root := t.TempDir()
	writeHostFile(t, root, "real/file.txt", []byte("inside"))
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})

	res := mustHandle(t, a, OpReadText, map[string]any{"path": "link/file.txt"})
	if got := res["text"].(string); got != "inside" {
		t.Fatalf("read through symlink = %q", got)
	}
	res = mustHandle(t, a, OpListDir, map[string]any{"path": "link"})
	if got := len(res["entries"].([]any)); got != 1 {
		t.Fatalf("listdir through symlink = %d entries", got)
	}

	// Writing through a symlinked directory writes inside the root.
	mustHandle(t, a, OpWriteText, map[string]any{"path": "link/file.txt", "text": "updated"})
	if got := string(readHostFile(t, root, "real/file.txt")); got != "updated" {
		t.Fatalf("real/file.txt = %q", got)
	}

	// Writing to a final-component symlink replaces the link, not its target.
	mustHandle(t, a, OpWriteText, map[string]any{"path": "alias", "text": "replaced"})
	if got := string(readHostFile(t, root, "real/file.txt")); got != "updated" {
		t.Fatalf("alias write followed the link: real/file.txt = %q", got)
	}
	if got := string(readHostFile(t, root, "alias")); got != "replaced" {
		t.Fatalf("alias content = %q", got)
	}
}

func TestSymlinkLoop(t *testing.T) {
	skipOnWindows(t)

	root := t.TempDir()
	if err := os.Symlink("b", filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})
	_, err := a.Handle(context.Background(), OpReadText, map[string]any{"path": "a"})
	wantCode(t, err, CodeInvalidPath)
}

func TestDanglingSymlinkExists(t *testing.T) {
	skipOnWindows(t)

	root := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})
	res := mustHandle(t, a, OpExists, map[string]any{"path": "dangling"})
	if !res["exists"].(bool) {
		t.Fatal("dangling symlink reported as missing")
	}
	_, err := a.Handle(context.Background(), OpReadText, map[string]any{"path": "dangling"})
	wantCode(t, err, CodeNotFound)
}

// TestSymlinkRaceDoesNotEscape toggles a symlink between an outside directory
// and the root while a worker reads and writes through it. os.Root must never
// let the outside sentinel change.
func TestSymlinkRaceDoesNotEscape(t *testing.T) {
	skipOnWindows(t)

	root := t.TempDir()
	outside := t.TempDir()
	writeHostFile(t, outside, "sentinel.txt", []byte("outside"))
	writeHostFile(t, root, "sentinel.txt", []byte("inside"))

	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})
	ctx := context.Background()

	stop := make(chan struct{})
	var toggler sync.WaitGroup
	toggler.Add(1)
	go func() {
		defer toggler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(link)
			_ = os.Symlink(outside, link)
			_ = os.Remove(link)
			_ = os.Symlink(root, link)
		}
	}()

	for i := 0; i < 200; i++ {
		_, _ = a.Handle(ctx, OpWriteText, map[string]any{"path": "link/sentinel.txt", "text": "pwned"})
		_, _ = a.Handle(ctx, OpReadText, map[string]any{"path": "link/sentinel.txt"})
	}

	close(stop)
	toggler.Wait()

	if got := string(readHostFile(t, outside, "sentinel.txt")); got != "outside" {
		t.Fatalf("outside sentinel changed: %q", got)
	}
}

// TestHardlinkLimitation documents the one containment gap os.Root cannot
// close: a hardlink created inside the root to an outside inode is readable.
// Atomic writes replace the directory entry, so they do not modify the
// outside inode.
func TestHardlinkLimitation(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeHostFile(t, outside, "outside.txt", []byte("outside"))

	if err := os.Link(filepath.Join(outside, "outside.txt"), filepath.Join(root, "linked.txt")); err != nil {
		t.Skipf("hardlink not supported here: %v", err)
	}

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})
	res := mustHandle(t, a, OpReadText, map[string]any{"path": "linked.txt"})
	if got := res["text"].(string); got != "outside" {
		t.Fatalf("hardlink read = %q", got)
	}

	mustHandle(t, a, OpWriteText, map[string]any{"path": "linked.txt", "text": "new"})
	if got := string(readHostFile(t, outside, "outside.txt")); got != "outside" {
		t.Fatalf("atomic write modified the outside inode: %q", got)
	}
	if got := string(readHostFile(t, root, "linked.txt")); got != "new" {
		t.Fatalf("linked.txt = %q", got)
	}
}

func TestPermissionDenied(t *testing.T) {
	skipOnWindows(t)
	skipIfRoot(t)

	root := t.TempDir()
	writeHostFile(t, root, "secret.txt", []byte("secret"))
	secret := filepath.Join(root, "secret.txt")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

	lockedDir := filepath.Join(root, "locked")
	if err := os.Mkdir(lockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, root, "locked/child.txt", []byte("x"))
	if err := os.Chmod(lockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })

	readOnlyDir := filepath.Join(root, "readonly")
	if err := os.Mkdir(readOnlyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyDir, 0o755) })

	execOnly := filepath.Join(root, "exec-only")
	if err := os.WriteFile(execOnly, []byte("x"), 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(execOnly, 0o600) })

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})
	ctx := context.Background()

	_, err := a.Handle(ctx, OpReadText, map[string]any{"path": "secret.txt"})
	wantCode(t, err, CodePermissionDenied)

	_, err = a.Handle(ctx, OpReadText, map[string]any{"path": "exec-only"})
	wantCode(t, err, CodePermissionDenied)

	_, err = a.Handle(ctx, OpListDir, map[string]any{"path": "locked"})
	wantCode(t, err, CodePermissionDenied)

	_, err = a.Handle(ctx, OpExists, map[string]any{"path": "locked/child.txt"})
	wantCode(t, err, CodePermissionDenied)

	_, err = a.Handle(ctx, OpWriteText, map[string]any{"path": "readonly/file.txt", "text": "x"})
	wantCode(t, err, CodePermissionDenied)
}

func TestLongPathName(t *testing.T) {
	skipOnWindows(t)

	a, _ := newTestAdapter(t, WorkspaceOptions{})
	name := strings.Repeat("a", 300)
	_, err := a.Handle(context.Background(), OpReadText, map[string]any{"path": name})
	wantCode(t, err, CodeInvalidPath)
}

func TestExistsNotDirectory(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	writeHostFile(t, root, "f", []byte("x"))
	_, err := a.Handle(context.Background(), OpExists, map[string]any{"path": "f/child"})
	wantCode(t, err, CodeNotDirectory)
}

func TestSocketIsIOError(t *testing.T) {
	skipOnWindows(t)

	root := t.TempDir()
	sock := filepath.Join(root, "sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket not supported here: %v", err)
	}
	defer listener.Close()

	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root})
	_, err = a.Handle(context.Background(), OpReadText, map[string]any{"path": "sock"})
	ce := wantCode(t, err, CodeIOError)
	if strings.Contains(ce.Message, root) {
		t.Fatalf("io_error leaks the workspace root: %v", ce)
	}
}
