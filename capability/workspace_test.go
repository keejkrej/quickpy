package capability

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestTextRoundTrip(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	text := "héllo, 世界\nline two\t✓"

	mustHandle(t, a, OpMkdir, map[string]any{"path": "sub", "parents": true})
	res := mustHandle(t, a, OpWriteText, map[string]any{"path": "sub/hello.txt", "text": text})
	if got := res["written"].(int64); got != int64(len(text)) {
		t.Fatalf("written = %d, want %d", got, len(text))
	}

	res = mustHandle(t, a, OpReadText, map[string]any{"path": "sub/hello.txt"})
	if got := res["text"].(string); got != text {
		t.Fatalf("text = %q, want %q", got, text)
	}
	if got := string(readHostFile(t, root, "sub/hello.txt")); got != text {
		t.Fatalf("host content = %q, want %q", got, text)
	}

	for _, encoding := range []string{"utf-8", "UTF-8"} {
		res = mustHandle(t, a, OpReadText, map[string]any{"path": "sub/hello.txt", "encoding": encoding})
		if got := res["text"].(string); got != text {
			t.Fatalf("encoding %q: text = %q", encoding, got)
		}
	}

	_, err := a.Handle(context.Background(), OpReadText, map[string]any{
		"path": "sub/hello.txt", "encoding": "latin-1",
	})
	wantCode(t, err, CodeToolArgument)
}

func TestBinaryRoundTrip(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	data := []byte{0x00, 0x01, 0xff, 0xfe, 0x00, 0x7f, 0x80}
	encoded := base64.StdEncoding.EncodeToString(data)

	mustHandle(t, a, OpMkdir, map[string]any{"path": "bin", "parents": true})
	res := mustHandle(t, a, OpWriteBytes, map[string]any{"path": "bin/data.bin", "base64": encoded})
	if got := res["written"].(int64); got != int64(len(data)) {
		t.Fatalf("written = %d, want %d", got, len(data))
	}

	res = mustHandle(t, a, OpReadBytes, map[string]any{"path": "bin/data.bin"})
	if got := res["base64"].(string); got != encoded {
		t.Fatalf("base64 = %q, want %q", got, encoded)
	}
	if got := readHostFile(t, root, "bin/data.bin"); string(got) != string(data) {
		t.Fatalf("host bytes = %v, want %v", got, data)
	}

	res = mustHandle(t, a, OpWriteBytes, map[string]any{"path": "bin/empty.bin", "base64": ""})
	if got := res["written"].(int64); got != 0 {
		t.Fatalf("empty written = %d", got)
	}
	res = mustHandle(t, a, OpReadBytes, map[string]any{"path": "bin/empty.bin"})
	if got := res["base64"].(string); got != "" {
		t.Fatalf("empty base64 = %q", got)
	}
}

func TestExists(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	writeHostFile(t, root, "file.txt", []byte("x"))
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{"file.txt", true},
		{"dir", true},
		{".", true},
		{"missing", false},
		{"missing/child", false},
	}
	for _, tc := range cases {
		res := mustHandle(t, a, OpExists, map[string]any{"path": tc.path})
		if got := res["exists"].(bool); got != tc.want {
			t.Fatalf("exists(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestListDir(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	writeHostFile(t, root, "b.txt", []byte("bb"))
	writeHostFile(t, root, "a.txt", []byte("a"))
	if err := os.Mkdir(filepath.Join(root, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	hasSymlink := runtime.GOOS != "windows"
	if hasSymlink {
		if err := os.Symlink("a.txt", filepath.Join(root, "d")); err != nil {
			t.Fatal(err)
		}
	}

	res := mustHandle(t, a, OpListDir, map[string]any{"path": "."})
	entries := res["entries"].([]any)

	wantNames := []string{"a.txt", "b.txt", "c"}
	if hasSymlink {
		wantNames = append(wantNames, "d")
	}
	if len(entries) != len(wantNames) {
		t.Fatalf("entries = %v, want %v", entries, wantNames)
	}
	for i, raw := range entries {
		entry := raw.(map[string]any)
		if got := entry["name"].(string); got != wantNames[i] {
			t.Fatalf("entry %d name = %q, want %q", i, got, wantNames[i])
		}
		if got := entry["name"].(string); got == "." || got == ".." {
			t.Fatalf("listdir returned %q", got)
		}
	}

	byName := map[string]map[string]any{}
	for _, raw := range entries {
		entry := raw.(map[string]any)
		byName[entry["name"].(string)] = entry
	}
	if got := byName["a.txt"]["type"].(string); got != "file" {
		t.Fatalf("a.txt type = %q", got)
	}
	if got := byName["a.txt"]["size"].(int64); got != 1 {
		t.Fatalf("a.txt size = %d", got)
	}
	if got := byName["b.txt"]["size"].(int64); got != 2 {
		t.Fatalf("b.txt size = %d", got)
	}
	if got := byName["c"]["type"].(string); got != "directory" {
		t.Fatalf("c type = %q", got)
	}
	if hasSymlink {
		if got := byName["d"]["type"].(string); got != "symlink" {
			t.Fatalf("d type = %q", got)
		}
		if got := byName["d"]["size"].(int64); got != int64(len("a.txt")) {
			t.Fatalf("d size = %d", got)
		}
	}

	// The default path is ".".
	res = mustHandle(t, a, OpListDir, nil)
	if got := len(res["entries"].([]any)); got != len(wantNames) {
		t.Fatalf("default listdir entries = %d, want %d", got, len(wantNames))
	}

	// Empty subdirectory.
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	res = mustHandle(t, a, OpListDir, map[string]any{"path": "empty"})
	if got := len(res["entries"].([]any)); got != 0 {
		t.Fatalf("empty listdir entries = %d", got)
	}

	_, err := a.Handle(context.Background(), OpListDir, map[string]any{"path": "a.txt"})
	wantCode(t, err, CodeNotDirectory)

	_, err = a.Handle(context.Background(), OpListDir, map[string]any{"path": "missing"})
	wantCode(t, err, CodeNotFound)

	_, err = a.Handle(context.Background(), OpListDir, map[string]any{"path": ""})
	wantCode(t, err, CodeInvalidPath)
}

func TestMkdir(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})

	res := mustHandle(t, a, OpMkdir, map[string]any{"path": "a"})
	if !res["created"].(bool) {
		t.Fatal("first mkdir did not create")
	}
	_, err := a.Handle(context.Background(), OpMkdir, map[string]any{"path": "a"})
	wantCode(t, err, CodeExists)

	res = mustHandle(t, a, OpMkdir, map[string]any{"path": "a/b/c", "parents": true})
	if !res["created"].(bool) {
		t.Fatal("parents mkdir did not create")
	}
	res = mustHandle(t, a, OpMkdir, map[string]any{"path": "a/b/c", "parents": true})
	if res["created"].(bool) {
		t.Fatal("existing parents mkdir reported created")
	}
	res = mustHandle(t, a, OpMkdir, map[string]any{"path": "a/b/c/", "parents": true})
	if res["created"].(bool) {
		t.Fatal("trailing-slash existing mkdir reported created")
	}

	_, err = a.Handle(context.Background(), OpMkdir, map[string]any{"path": "x/y"})
	wantCode(t, err, CodeNotFound)

	_, err = a.Handle(context.Background(), OpMkdir, map[string]any{"path": "a/b/c"})
	wantCode(t, err, CodeExists)

	res = mustHandle(t, a, OpMkdir, map[string]any{"path": ".", "parents": true})
	if res["created"].(bool) {
		t.Fatal("mkdir . parents reported created")
	}
	_, err = a.Handle(context.Background(), OpMkdir, map[string]any{"path": "."})
	wantCode(t, err, CodeExists)

	writeHostFile(t, root, "f", []byte("x"))
	_, err = a.Handle(context.Background(), OpMkdir, map[string]any{"path": "f", "parents": true})
	wantCode(t, err, CodeExists)

	_, err = a.Handle(context.Background(), OpMkdir, map[string]any{"path": "f/child", "parents": true})
	wantCode(t, err, CodeNotDirectory)
}

func TestRemove(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})

	writeHostFile(t, root, "file.txt", []byte("x"))
	res := mustHandle(t, a, OpRemove, map[string]any{"path": "file.txt"})
	if !res["removed"].(bool) {
		t.Fatal("remove file reported false")
	}
	res = mustHandle(t, a, OpExists, map[string]any{"path": "file.txt"})
	if res["exists"].(bool) {
		t.Fatal("file still exists")
	}

	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	res = mustHandle(t, a, OpRemove, map[string]any{"path": "dir"})
	if !res["removed"].(bool) {
		t.Fatal("remove empty dir reported false")
	}

	writeHostFile(t, root, "full/child.txt", []byte("x"))
	_, err := a.Handle(context.Background(), OpRemove, map[string]any{"path": "full"})
	wantCode(t, err, CodeNotEmpty)

	res = mustHandle(t, a, OpRemove, map[string]any{"path": "missing"})
	if res["removed"].(bool) {
		t.Fatal("remove missing reported true")
	}

	_, err = a.Handle(context.Background(), OpRemove, map[string]any{"path": "."})
	wantCode(t, err, CodeInvalidPath)

	if runtime.GOOS != "windows" {
		writeHostFile(t, root, "target.txt", []byte("keep"))
		if err := os.Symlink("target.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		res = mustHandle(t, a, OpRemove, map[string]any{"path": "link"})
		if !res["removed"].(bool) {
			t.Fatal("remove symlink reported false")
		}
		res = mustHandle(t, a, OpExists, map[string]any{"path": "target.txt"})
		if !res["exists"].(bool) {
			t.Fatal("remove symlink removed its target")
		}
	}
}

func TestReadOnly(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "existing.txt", []byte("keep"))
	a, _ := newTestAdapter(t, WorkspaceOptions{Root: root, ReadOnly: true})

	res := mustHandle(t, a, OpReadText, map[string]any{"path": "existing.txt"})
	if got := res["text"].(string); got != "keep" {
		t.Fatalf("read-only read = %q", got)
	}

	mutations := []struct {
		op   string
		args map[string]any
	}{
		{OpWriteText, map[string]any{"path": "new.txt", "text": "x"}},
		{OpWriteBytes, map[string]any{"path": "new.bin", "base64": "eA=="}},
		{OpMkdir, map[string]any{"path": "newdir"}},
		{OpRemove, map[string]any{"path": "existing.txt"}},
	}
	for _, m := range mutations {
		_, err := a.Handle(context.Background(), m.op, m.args)
		wantCode(t, err, CodePermissionDenied)
	}

	res = mustHandle(t, a, OpListDir, map[string]any{"path": "."})
	entries := res["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("read-only root entries = %v, want only existing.txt", entries)
	}
	if got := entries[0].(map[string]any)["name"].(string); got != "existing.txt" {
		t.Fatalf("read-only root entry = %q", got)
	}
	if got := string(readHostFile(t, root, "existing.txt")); got != "keep" {
		t.Fatalf("read-only mutation changed existing.txt: %q", got)
	}
}

func TestLimits(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{MaxBytes: 4})

	res := mustHandle(t, a, OpWriteText, map[string]any{"path": "a.txt", "text": "abcd"})
	if got := res["written"].(int64); got != 4 {
		t.Fatalf("exact text written = %d", got)
	}
	_, err := a.Handle(context.Background(), OpWriteText, map[string]any{"path": "b.txt", "text": "abcde"})
	wantCode(t, err, CodeTooLarge)

	res = mustHandle(t, a, OpWriteBytes, map[string]any{
		"path": "a.bin", "base64": base64.StdEncoding.EncodeToString([]byte("abcd")),
	})
	if got := res["written"].(int64); got != 4 {
		t.Fatalf("exact bytes written = %d", got)
	}
	_, err = a.Handle(context.Background(), OpWriteBytes, map[string]any{
		"path": "b.bin", "base64": base64.StdEncoding.EncodeToString([]byte("abcde")),
	})
	wantCode(t, err, CodeTooLarge)

	res = mustHandle(t, a, OpReadText, map[string]any{"path": "a.txt"})
	if got := res["text"].(string); got != "abcd" {
		t.Fatalf("exact read = %q", got)
	}

	writeHostFile(t, root, "big.txt", []byte("abcde"))
	_, err = a.Handle(context.Background(), OpReadText, map[string]any{"path": "big.txt"})
	wantCode(t, err, CodeTooLarge)
	_, err = a.Handle(context.Background(), OpReadBytes, map[string]any{"path": "big.txt"})
	wantCode(t, err, CodeTooLarge)

	// The LimitReader path is authoritative even when a stat size is not.
	if _, err := readAllLimited(strings.NewReader("abcd"), 4); err != nil {
		t.Fatalf("readAllLimited exact: %v", err)
	}
	if _, err := readAllLimited(strings.NewReader("abcde"), 4); !errors.Is(err, errTooLarge) {
		t.Fatalf("readAllLimited over = %v, want errTooLarge", err)
	}

	// Entry cap.
	if err := os.Mkdir(filepath.Join(root, "many"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, root, "many/e1", []byte("1"))
	writeHostFile(t, root, "many/e2", []byte("2"))
	small, _ := newTestAdapter(t, WorkspaceOptions{Root: root, MaxEntries: 2})
	res = mustHandle(t, small, OpListDir, map[string]any{"path": "many"})
	if got := len(res["entries"].([]any)); got != 2 {
		t.Fatalf("exact entries = %d", got)
	}
	writeHostFile(t, root, "many/e3", []byte("3"))
	_, err = small.Handle(context.Background(), OpListDir, map[string]any{"path": "many"})
	wantCode(t, err, CodeTooLarge)
}

func TestInvalidUTF8(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})

	mustHandle(t, a, OpWriteBytes, map[string]any{
		"path": "bad.bin", "base64": base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}),
	})
	_, err := a.Handle(context.Background(), OpReadText, map[string]any{"path": "bad.bin"})
	wantCode(t, err, CodeInvalidUTF8)

	_, err = a.Handle(context.Background(), OpWriteText, map[string]any{
		"path": "bad.txt", "text": string([]byte{0xff}),
	})
	wantCode(t, err, CodeInvalidUTF8)

	res := mustHandle(t, a, OpExists, map[string]any{"path": "bad.txt"})
	if res["exists"].(bool) {
		t.Fatal("invalid UTF-8 text was written")
	}
}

func TestInvalidBase64(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})

	for _, encoded := range []string{"a", "abc", "ab=c", "!!!!", "AA A", "AA\nA", "====", "A===", "AA=A", "AB=="} {
		_, err := a.Handle(context.Background(), OpWriteBytes, map[string]any{
			"path": "x.bin", "base64": encoded,
		})
		wantCode(t, err, CodeInvalidBase64)
	}

	res := mustHandle(t, a, OpWriteBytes, map[string]any{"path": "ok.bin", "base64": "AA=="})
	if got := res["written"].(int64); got != 1 {
		t.Fatalf("written = %d", got)
	}
	res = mustHandle(t, a, OpReadBytes, map[string]any{"path": "ok.bin"})
	if got := res["base64"].(string); got != "AA==" {
		t.Fatalf("base64 = %q", got)
	}
}

func TestPathValidation(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	writeHostFile(t, root, "b.txt", []byte("b"))

	bad := []string{
		"",
		"/etc/passwd",
		`\\server\share`,
		`sub\file`,
		"C:/Windows",
		"C:file",
		"a:b",
		"a\x00b",
		"../x",
		"a/../../x",
		"..",
		"a/../..",
	}
	for _, p := range bad {
		_, err := a.Handle(context.Background(), OpReadText, map[string]any{"path": p})
		wantCode(t, err, CodeInvalidPath)
	}

	// Non-escaping "." and ".." components and trailing slashes are cleaned.
	for _, p := range []string{"a/../b.txt", "./b.txt", "b.txt/"} {
		res := mustHandle(t, a, OpReadText, map[string]any{"path": p})
		if got := res["text"].(string); got != "b" {
			t.Fatalf("read %q = %q", p, got)
		}
	}

	res := mustHandle(t, a, OpMkdir, map[string]any{"path": "sub/"})
	if !res["created"].(bool) {
		t.Fatal("mkdir sub/ did not create")
	}

	_, err := a.Handle(context.Background(), OpReadText, map[string]any{"path": "."})
	wantCode(t, err, CodeIsDirectory)
}

func TestWriteAtomic(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})

	writeHostFile(t, root, "f.txt", []byte("old"))
	mustHandle(t, a, OpWriteText, map[string]any{"path": "f.txt", "text": "new"})
	if got := string(readHostFile(t, root, "f.txt")); got != "new" {
		t.Fatalf("replaced content = %q", got)
	}

	assertNoTempFiles := func() {
		t.Helper()
		res := mustHandle(t, a, OpListDir, map[string]any{"path": "."})
		for _, raw := range res["entries"].([]any) {
			name := raw.(map[string]any)["name"].(string)
			if strings.HasPrefix(name, ".capability-tmp-") {
				t.Fatalf("temporary file left behind: %q", name)
			}
		}
	}
	assertNoTempFiles()

	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := a.Handle(context.Background(), OpWriteText, map[string]any{"path": "dir", "text": "x"})
	wantCode(t, err, CodeIsDirectory)
	assertNoTempFiles()

	_, err = a.Handle(context.Background(), OpWriteText, map[string]any{"path": "missing/f.txt", "text": "x"})
	wantCode(t, err, CodeNotFound)
	assertNoTempFiles()

	writeHostFile(t, root, "p", []byte("x"))
	_, err = a.Handle(context.Background(), OpWriteText, map[string]any{"path": "p/child", "text": "x"})
	wantCode(t, err, CodeNotDirectory)
	assertNoTempFiles()

	_, err = a.Handle(context.Background(), OpWriteText, map[string]any{"path": ".", "text": "x"})
	wantCode(t, err, CodeIsDirectory)
	assertNoTempFiles()
}

func TestReadDirectory(t *testing.T) {
	a, root := newTestAdapter(t, WorkspaceOptions{})
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := a.Handle(context.Background(), OpReadText, map[string]any{"path": "dir"})
	wantCode(t, err, CodeIsDirectory)
	_, err = a.Handle(context.Background(), OpReadBytes, map[string]any{"path": "dir"})
	wantCode(t, err, CodeIsDirectory)
}

func TestUnicodeFilenames(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})
	name := "héllo-世界.txt"

	mustHandle(t, a, OpWriteText, map[string]any{"path": name, "text": "hi"})
	res := mustHandle(t, a, OpReadText, map[string]any{"path": name})
	if got := res["text"].(string); got != "hi" {
		t.Fatalf("text = %q", got)
	}

	res = mustHandle(t, a, OpListDir, map[string]any{"path": "."})
	found := false
	for _, raw := range res["entries"].([]any) {
		if raw.(map[string]any)["name"].(string) == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("listdir did not return %q", name)
	}
}

func TestConcurrentAccess(t *testing.T) {
	a, _ := newTestAdapter(t, WorkspaceOptions{})
	ctx := context.Background()

	const workers = 8
	const iterations = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers*iterations)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				name := fmt.Sprintf("w%d-%d.txt", w, i)
				if _, err := a.Handle(ctx, OpWriteText, map[string]any{"path": name, "text": name}); err != nil {
					errs <- err
					return
				}
				res, err := a.Handle(ctx, OpReadText, map[string]any{"path": name})
				if err != nil {
					errs <- err
					return
				}
				if got := res.(map[string]any)["text"].(string); got != name {
					errs <- fmt.Errorf("read %q = %q", name, got)
					return
				}
				if _, err := a.Handle(ctx, OpExists, map[string]any{"path": name}); err != nil {
					errs <- err
					return
				}
				if _, err := a.Handle(ctx, OpListDir, map[string]any{"path": "."}); err != nil {
					errs <- err
					return
				}
				if _, err := a.Handle(ctx, OpRemove, map[string]any{"path": name}); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Concurrent writers to one path must leave a complete value, never a
	// partial write.
	var shared sync.WaitGroup
	for w := 0; w < workers; w++ {
		shared.Add(1)
		go func(w int) {
			defer shared.Done()
			for i := 0; i < 20; i++ {
				value := fmt.Sprintf("value-%d-%d", w, i)
				if _, err := a.Handle(ctx, OpWriteText, map[string]any{"path": "shared.txt", "text": value}); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	shared.Wait()

	res := mustHandle(t, a, OpReadText, map[string]any{"path": "shared.txt"})
	if got := res["text"].(string); !strings.HasPrefix(got, "value-") {
		t.Fatalf("shared.txt = %q, want a complete value", got)
	}
}
