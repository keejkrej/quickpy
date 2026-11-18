package integration_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/keejkrej/quickpy"
	"github.com/keejkrej/quickpy/capability"
	"github.com/keejkrej/quickpy/capability/quickpybridge"
)

func newWorkspaceRuntime(t *testing.T, opts capability.WorkspaceOptions) *quickpy.Runtime {
	t.Helper()
	a, err := capability.NewWorkspace(opts)
	if err != nil {
		t.Fatalf("capability.NewWorkspace: %v", err)
	}
	// newRuntime registers its cleanup after this one, so Runtime.Close runs
	// before Adapter.Close (testing cleanups run in reverse registration order).
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("Adapter.Close: %v", err)
		}
	})
	r := newRuntime(t, quickpy.RuntimeOptions{
		MaxMemoryBytes:  64 << 20,
		MaxMessageBytes: 16 << 20,
	})
	if err := quickpybridge.Install(r, a); err != nil {
		t.Fatalf("quickpybridge.Install: %v", err)
	}
	return r
}

func TestWorkspaceFilesystemOperationsPersist(t *testing.T) {
	root := t.TempDir()
	r := newWorkspaceRuntime(t, capability.WorkspaceOptions{Root: root})
	text := "Grüße, 世界 👋\nsecond line"
	binary := []byte{0x00, 0x7f, 0x80, 0xff}

	setup := `import asyncio, quickpy
async def main():
    print("mkdir", await quickpy.fs.mkdir("tree/sub", parents=True))
    print("mkdir_again", await quickpy.fs.mkdir("tree/sub", parents=True))
    print("exists_missing", await quickpy.fs.exists("tree/sub/missing.txt"))
    print("write_text", await quickpy.fs.write_text("tree/sub/text.txt", "Grüße, 世界 👋\nsecond line"))
    print("write_bytes", await quickpy.fs.write_bytes("tree/data.bin", b"\x00\x7f\x80\xff"))
    print("exists_text", await quickpy.fs.exists("tree/sub/text.txt"))
    await quickpy.fs.write_text("tree/delete.txt", "remove me")
    print("mkdir_empty", await quickpy.fs.mkdir("tree/empty"))
    entries = await quickpy.fs.listdir("tree")
    for entry in entries:
        size = entry["size"] if entry["type"] == "file" else "-"
        print("entry", entry["name"], entry["type"], size)
    print("remove_file", await quickpy.fs.remove("tree/delete.txt"))
    print("remove_empty_dir", await quickpy.fs.remove("tree/empty"))
quickpy.run(main())`
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: setup}))
	requireStatus(t, res, quickpy.StatusCompleted)
	wantSetup := fmt.Sprintf(`mkdir True
mkdir_again False
exists_missing False
write_text %d
write_bytes 4
exists_text True
mkdir_empty True
entry data.bin file 4
entry delete.txt file 9
entry empty directory -
entry sub directory -
remove_file True
remove_empty_dir True
`, len(text))
	if got := textOf(res.Outputs); got != wantSetup {
		t.Fatalf("setup output = %q, want %q", got, wantSetup)
	}

	if got, err := os.ReadFile(filepath.Join(root, "tree", "sub", "text.txt")); err != nil {
		t.Fatalf("read host text: %v", err)
	} else if string(got) != text {
		t.Fatalf("host text = %q, want %q", got, text)
	}
	if got, err := os.ReadFile(filepath.Join(root, "tree", "data.bin")); err != nil {
		t.Fatalf("read host bytes: %v", err)
	} else if !bytes.Equal(got, binary) {
		t.Fatalf("host bytes = %v, want %v", got, binary)
	}
	for _, name := range []string{"tree/sub", "tree/data.bin", "tree/sub/text.txt"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Errorf("host path %q: %v", name, err)
		}
	}
	for _, name := range []string{"tree/delete.txt", "tree/empty"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("removed host path %q still present: %v", name, err)
		}
	}

	// Execute uses fresh guest globals. These reads therefore prove persistence
	// in the real host workspace, not values retained in a Python global.
	readBack := `import asyncio, binascii, quickpy
async def main():
    text, data = await asyncio.gather(
        quickpy.fs.read_text("tree/sub/text.txt"),
        quickpy.fs.read_bytes("tree/data.bin"),
    )
    print("text", text)
    print("bytes", str(binascii.hexlify(data), "ascii"))
quickpy.run(main())`
	res = mustWait(t, execute(t, r, quickpy.ExecuteRequest{Source: readBack}))
	requireStatus(t, res, quickpy.StatusCompleted)
	wantRead := "text " + text + "\nbytes 007f80ff\n"
	if got := textOf(res.Outputs); got != wantRead {
		t.Fatalf("read-back output = %q, want %q", got, wantRead)
	}
}

func TestWorkspaceReadOnlyAndContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	const sentinel = "outside sentinel must not change"
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "keepdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	outsideSentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(outsideSentinel, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, outsideSentinel)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	relativeEscape := filepath.ToSlash(rel)
	absoluteEscape := filepath.ToSlash(outsideSentinel)
	beforeRoot := workspaceSnapshot(t, root)
	beforeOutside := workspaceSnapshot(t, outside)

	r := newWorkspaceRuntime(t, capability.WorkspaceOptions{Root: root, ReadOnly: true})
	var rec eventRecorder
	source := fmt.Sprintf(`import asyncio, quickpy
async def expect_read_only(label, call):
    try:
        await call
        print("UNEXPECTED", label)
    except OSError as e:
        print("readonly", label, e.errno, e)
async def expect_invalid(label, call):
    try:
        await call
        print("UNEXPECTED", label)
    except ValueError as e:
        print("invalid", label, e)
async def main():
    await expect_read_only("write_text", quickpy.fs.write_text("new.txt", "new"))
    await expect_read_only("write_bytes", quickpy.fs.write_bytes("new.bin", b"\x00\xff"))
    await expect_read_only("mkdir", quickpy.fs.mkdir("newdir", parents=True))
    await expect_read_only("remove", quickpy.fs.remove("keep.txt"))
    await expect_invalid("read_relative", quickpy.fs.read_text(%q))
    await expect_invalid("write_relative", quickpy.fs.write_text(%q, "changed"))
    await expect_invalid("read_absolute", quickpy.fs.read_text(%q))
    await expect_invalid("write_absolute", quickpy.fs.write_text(%q, "changed"))
quickpy.run(main())`, relativeEscape, relativeEscape, absoluteEscape, absoluteEscape)
	res := mustWait(t, execute(t, r, quickpy.ExecuteRequest{
		Source:    source,
		EventSink: rec.sink,
	}))
	requireStatus(t, res, quickpy.StatusCompleted)
	out := textOf(res.Outputs)
	for _, name := range []string{"write_text", "write_bytes", "mkdir", "remove"} {
		requireContains(t, out, "readonly "+name+" 13 (13, 'workspace is read-only')")
	}
	requireContains(t, out, "invalid read_relative invalid_path:")
	requireContains(t, out, "invalid write_relative invalid_path:")
	requireContains(t, out, "invalid read_absolute invalid_path:")
	requireContains(t, out, "invalid write_absolute invalid_path:")
	if strings.Contains(out, "UNEXPECTED") {
		t.Fatalf("a denied operation succeeded: %q", out)
	}
	for _, hostPath := range []string{root, outside} {
		if strings.Contains(out, hostPath) {
			t.Errorf("guest error leaks host path %q: %q", hostPath, out)
		}
		if resolved, err := filepath.EvalSymlinks(hostPath); err == nil && strings.Contains(out, resolved) {
			t.Errorf("guest error leaks resolved host path %q: %q", resolved, out)
		}
	}

	var failures []string
	for _, ev := range rec.snapshot() {
		if te, ok := ev.(quickpy.ToolEvent); ok && te.Phase == "finished" {
			failures = append(failures, te.Name+"="+te.Error)
		}
	}
	sort.Strings(failures)
	wantFailures := []string{
		"fs_mkdir=permission_denied",
		"fs_read_text=invalid_path",
		"fs_read_text=invalid_path",
		"fs_remove=permission_denied",
		"fs_write_bytes=permission_denied",
		"fs_write_text=invalid_path",
		"fs_write_text=invalid_path",
		"fs_write_text=permission_denied",
	}
	if got, want := strings.Join(failures, "\n"), strings.Join(wantFailures, "\n"); got != want {
		t.Fatalf("finished tool errors =\n%s\nwant:\n%s", got, want)
	}

	if got := workspaceSnapshot(t, root); !mapsEqual(got, beforeRoot) {
		t.Errorf("read-only workspace changed: before=%v after=%v", beforeRoot, got)
	}
	if got := workspaceSnapshot(t, outside); !mapsEqual(got, beforeOutside) {
		t.Errorf("outside workspace changed: before=%v after=%v", beforeOutside, got)
	}
	if got, err := os.ReadFile(outsideSentinel); err != nil {
		t.Fatalf("read outside sentinel: %v", err)
	} else if string(got) != sentinel {
		t.Fatalf("outside sentinel = %q, want %q", got, sentinel)
	}
}

func TestWorkspaceSideEffectsNotRolledBack(t *testing.T) {
	root := t.TempDir()
	r := newWorkspaceRuntime(t, capability.WorkspaceOptions{Root: root})
	source := `import asyncio, quickpy
async def main():
    await quickpy.fs.write_text("committed.txt", "persisted")
    raise RuntimeError("after write")
quickpy.run(main())`
	res, err := wait(t, execute(t, r, quickpy.ExecuteRequest{Source: source}))
	requireErrorKind(t, err, quickpy.KindScriptError)
	requireStatus(t, res, quickpy.StatusFailed)
	if got, err := os.ReadFile(filepath.Join(root, "committed.txt")); err != nil {
		t.Fatalf("read committed file: %v", err)
	} else if string(got) != "persisted" {
		t.Fatalf("committed file = %q, want %q", got, "persisted")
	}
}

func workspaceSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if entry.IsDir() {
			snapshot[name] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[name] = "file:" + string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
