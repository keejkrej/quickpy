package capability

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf8"
)

// Default limits. A zero MaxBytes or MaxEntries in WorkspaceOptions selects
// these values; it never means "unbounded".
const (
	DefaultMaxBytes   int64 = 1 << 20 // 1 MiB
	DefaultMaxEntries       = 10_000
)

// WorkspaceOptions configures a local filesystem Adapter.
//
// Root is the host-chosen directory that bounds every operation. It is opened
// once with os.OpenRoot, so later working-directory changes do not move the
// capability.
//
// ReadOnly is explicit: false permits writes, true denies every mutating
// operation with permission_denied. The zero value is writable, so callers
// handling untrusted input should set ReadOnly: true deliberately.
//
// MaxBytes bounds text and binary reads and writes. MaxEntries bounds the
// number of directory entries returned by fs_listdir. Zero selects the
// package defaults; negative values are rejected.
type WorkspaceOptions struct {
	Root       string
	ReadOnly   bool
	MaxBytes   int64
	MaxEntries int
}

// NewWorkspace opens Root and returns a filesystem Adapter. The returned error
// is host-facing: it may name the root path. Errors returned later by Handle
// never do.
func NewWorkspace(opts WorkspaceOptions) (*Adapter, error) {
	if opts.Root == "" {
		return nil, newError(CodeInvalidArgument, "workspace root must not be empty")
	}

	maxBytes := opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	if maxBytes < 0 || maxBytes == math.MaxInt64 {
		return nil, newError(CodeInvalidArgument, "MaxBytes must be positive and below math.MaxInt64")
	}

	maxEntries := opts.MaxEntries
	if maxEntries == 0 {
		maxEntries = DefaultMaxEntries
	}
	if maxEntries < 0 || maxEntries == math.MaxInt {
		return nil, newError(CodeInvalidArgument, "MaxEntries must be positive and below math.MaxInt")
	}

	root, err := os.OpenRoot(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("capability: open workspace root: %w", err)
	}

	return &Adapter{
		root:       root,
		readOnly:   opts.ReadOnly,
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
	}, nil
}

// cleanRelativePath validates the v1 path policy and returns a cleaned POSIX
// relative path. It rejects empty paths, NUL, backslashes, colons (drive
// letters and Windows alternate data streams), absolute paths, and any path
// that escapes the root after cleaning. Non-escaping "." and ".." components
// are interpreted normally, and a trailing slash is accepted.
func cleanRelativePath(raw string) (string, error) {
	if raw == "" {
		return "", invalidPath("path must not be empty")
	}
	if strings.ContainsRune(raw, 0) {
		return "", invalidPath("path must not contain NUL")
	}
	if strings.ContainsAny(raw, "\\:") {
		return "", invalidPath("path must use POSIX separators and must not contain ':'")
	}
	if strings.HasPrefix(raw, "/") {
		return "", invalidPath("path must be relative")
	}

	cleaned := path.Clean(raw)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", invalidPath("path must not escape the workspace root")
	}
	return cleaned, nil
}

func (a *Adapter) readText(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path", "encoding"); err != nil {
		return nil, err
	}
	raw, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}
	encoding, err := optionalString(args, "encoding", "utf-8")
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(encoding, "utf-8") {
		return nil, toolArgument("unsupported encoding %q; only utf-8 is supported", encoding)
	}

	data, err := a.readLimited(ctx, p)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, newError(CodeInvalidUTF8, "%q is not valid UTF-8", p)
	}
	return map[string]any{"text": string(data)}, nil
}

func (a *Adapter) readBytes(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path"); err != nil {
		return nil, err
	}
	raw, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}

	data, err := a.readLimited(ctx, p)
	if err != nil {
		return nil, err
	}
	return map[string]any{"base64": base64.StdEncoding.EncodeToString(data)}, nil
}

// readLimited opens p through the root, rejects non-regular files, and reads at
// most maxBytes+1 bytes. The stat size is an early rejection; the LimitReader
// is the authoritative bound even if the file grows after the stat.
func (a *Adapter) readLimited(ctx context.Context, p string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	f, err := a.root.Open(p)
	if err != nil {
		return nil, a.mapPathError(p, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, a.mapPathError(p, err)
	}
	if info.IsDir() {
		return nil, newError(CodeIsDirectory, "%q is a directory", p)
	}
	if !info.Mode().IsRegular() {
		return nil, newError(CodeIOError, "%q is not a regular file", p)
	}
	if info.Size() > a.maxBytes {
		return nil, tooLarge("%q is larger than the %d byte limit", p, a.maxBytes)
	}

	data, err := readAllLimited(f, a.maxBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return nil, tooLarge("%q is larger than the %d byte limit", p, a.maxBytes)
		}
		return nil, a.mapPathError(p, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	return data, nil
}

var errTooLarge = errors.New("capability: content too large")

// readAllLimited reads at most max+1 bytes and reports errTooLarge when the
// reader produced more than max. It never allocates more than max+1 bytes.
func readAllLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errTooLarge
	}
	return data, nil
}

func (a *Adapter) writeText(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path", "text"); err != nil {
		return nil, err
	}
	raw, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}
	text, err := requireString(args, "text")
	if err != nil {
		return nil, err
	}
	if a.readOnly {
		return nil, permissionDenied("workspace is read-only")
	}
	if int64(len(text)) > a.maxBytes {
		return nil, tooLarge("text is larger than the %d byte limit", a.maxBytes)
	}
	if !utf8.ValidString(text) {
		return nil, newError(CodeInvalidUTF8, "text is not valid UTF-8")
	}

	data := []byte(text)
	if err := a.writeAtomic(ctx, p, data); err != nil {
		return nil, err
	}
	return map[string]any{"written": int64(len(data))}, nil
}

func (a *Adapter) writeBytes(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path", "base64"); err != nil {
		return nil, err
	}
	raw, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}
	encoded, err := requireString(args, "base64")
	if err != nil {
		return nil, err
	}
	if a.readOnly {
		return nil, permissionDenied("workspace is read-only")
	}

	data, err := decodeBase64Limited(encoded, a.maxBytes)
	if err != nil {
		return nil, err
	}
	if err := a.writeAtomic(ctx, p, data); err != nil {
		return nil, err
	}
	return map[string]any{"written": int64(len(data))}, nil
}

// decodeBase64Limited rejects whitespace and malformed padding, computes the
// exact decoded length before allocating, and enforces max before decode.
func decodeBase64Limited(s string, max int64) ([]byte, error) {
	if strings.ContainsAny(s, " \t\r\n\v\f") {
		return nil, newError(CodeInvalidBase64, "base64 must not contain whitespace")
	}
	if len(s)%4 != 0 {
		return nil, newError(CodeInvalidBase64, "base64 length must be a multiple of 4")
	}

	padding := 0
	if strings.HasSuffix(s, "==") {
		padding = 2
	} else if strings.HasSuffix(s, "=") {
		padding = 1
	}
	decodedLen := len(s)/4*3 - padding
	if decodedLen < 0 {
		return nil, newError(CodeInvalidBase64, "base64 padding is invalid")
	}
	if int64(decodedLen) > max {
		return nil, tooLarge("decoded content is larger than the %d byte limit", max)
	}

	dst := make([]byte, decodedLen)
	n, err := base64.StdEncoding.Strict().Decode(dst, []byte(s))
	if err != nil {
		return nil, newError(CodeInvalidBase64, "base64 is invalid")
	}
	if n != decodedLen {
		return nil, newError(CodeInvalidBase64, "base64 decoded length mismatch")
	}
	return dst[:n], nil
}

// writeAtomic writes data to a unique temporary file in the target directory
// and renames it over p. A failed or cancelled write removes the temporary
// file. Writes replace the directory entry, so writing through a symlink or
// hardlink replaces the link rather than modifying its target.
func (a *Adapter) writeAtomic(ctx context.Context, p string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	if p == "." {
		return newError(CodeIsDirectory, "%q is a directory", p)
	}

	dir := path.Dir(p)
	var (
		tmp string
		f   *os.File
		err error
	)
	for attempt := 0; attempt < 8; attempt++ {
		tmp = path.Join(dir, tempName())
		f, err = a.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return a.mapPathError(p, err)
		}
	}
	if f == nil {
		return newError(CodeIOError, "could not create a temporary file for %q", p)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = a.root.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return a.mapPathError(p, err)
	}
	if err := f.Close(); err != nil {
		return a.mapPathError(p, err)
	}
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	if err := a.root.Rename(tmp, p); err != nil {
		if fi, statErr := a.root.Lstat(p); statErr == nil && fi.IsDir() {
			return newError(CodeIsDirectory, "%q is a directory", p)
		}
		return a.mapPathError(p, err)
	}
	cleanup = false
	return nil
}

var tempCounter atomic.Uint64

func tempName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf(".capability-tmp-%x", b[:])
	}
	return fmt.Sprintf(".capability-tmp-%d-%d", os.Getpid(), tempCounter.Add(1))
}

func (a *Adapter) exists(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path"); err != nil {
		return nil, err
	}
	raw, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	// Lstat does not follow the final component, so a dangling symlink exists
	// and a symlink to an outside target is never traversed here.
	_, err = a.root.Lstat(p)
	if err == nil {
		return map[string]any{"exists": true}, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{"exists": false}, nil
	}
	return nil, a.mapPathError(p, err)
}

func (a *Adapter) listDir(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path"); err != nil {
		return nil, err
	}
	raw, err := optionalString(args, "path", ".")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	f, err := a.root.Open(p)
	if err != nil {
		return nil, a.mapPathError(p, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, a.mapPathError(p, err)
	}
	if !info.IsDir() {
		return nil, newError(CodeNotDirectory, "%q is not a directory", p)
	}

	// Read at most maxEntries+1 so a huge directory cannot allocate without
	// bound. A non-empty error at the end of the directory is io.EOF.
	entries, err := f.ReadDir(a.maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, a.mapPathError(p, err)
	}
	if len(entries) > a.maxEntries {
		return nil, tooLarge("%q has more than %d entries", p, a.maxEntries)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		typ := "file"
		if entry.IsDir() {
			typ = "directory"
		} else if entry.Type()&os.ModeSymlink != 0 {
			typ = "symlink"
		}

		var size int64
		if fi, err := entry.Info(); err == nil {
			size = fi.Size()
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, a.mapPathError(entry.Name(), err)
		}

		out = append(out, map[string]any{
			"name": entry.Name(),
			"type": typ,
			"size": size,
		})
	}

	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	return map[string]any{"entries": out}, nil
}

func (a *Adapter) mkdir(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path", "parents"); err != nil {
		return nil, err
	}
	raw, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}
	parents, err := optionalBool(args, "parents", false)
	if err != nil {
		return nil, err
	}
	if a.readOnly {
		return nil, permissionDenied("workspace is read-only")
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	if parents {
		fi, statErr := a.root.Lstat(p)
		if statErr == nil {
			if fi.IsDir() {
				return map[string]any{"created": false}, nil
			}
			return nil, newError(CodeExists, "%q already exists and is not a directory", p)
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, a.mapPathError(p, statErr)
		}
		if err := a.root.MkdirAll(p, 0o755); err != nil {
			return nil, a.mapPathError(p, err)
		}
		return map[string]any{"created": true}, nil
	}

	if err := a.root.Mkdir(p, 0o755); err != nil {
		return nil, a.mapPathError(p, err)
	}
	return map[string]any{"created": true}, nil
}

func (a *Adapter) remove(ctx context.Context, args map[string]any) (any, error) {
	if err := checkKeys(args, "path"); err != nil {
		return nil, err
	}
	raw, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	p, err := cleanRelativePath(raw)
	if err != nil {
		return nil, err
	}
	if p == "." {
		return nil, invalidPath("cannot remove the workspace root")
	}
	if a.readOnly {
		return nil, permissionDenied("workspace is read-only")
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	fi, err := a.root.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{"removed": false}, nil
		}
		return nil, a.mapPathError(p, err)
	}

	// Check emptiness explicitly so not_empty is stable across platforms.
	// Windows reports a non-empty directory as fs.ErrExist, not ENOTEMPTY.
	if fi.IsDir() {
		d, err := a.root.Open(p)
		if err != nil {
			return nil, a.mapPathError(p, err)
		}
		entries, readErr := d.ReadDir(1)
		closeErr := d.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, a.mapPathError(p, readErr)
		}
		if closeErr != nil {
			return nil, a.mapPathError(p, closeErr)
		}
		if len(entries) > 0 {
			return nil, newError(CodeNotEmpty, "%q is not empty", p)
		}
	}

	if err := a.root.Remove(p); err != nil {
		if fi, statErr := a.root.Lstat(p); statErr == nil && fi.IsDir() && errors.Is(err, fs.ErrExist) {
			return nil, newError(CodeNotEmpty, "%q is not empty", p)
		}
		return nil, a.mapPathError(p, err)
	}
	return map[string]any{"removed": true}, nil
}

// mapPathError converts host filesystem errors into the stable v1 code set.
// The message uses only the caller-supplied relative path and static
// operation/errno context; it never copies raw host error text.
func (a *Adapter) mapPathError(p string, err error) *Error {
	if err == nil {
		return nil
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}

	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return contextError(err)
	case errors.Is(err, fs.ErrNotExist):
		return newError(CodeNotFound, "%q does not exist", p)
	case errors.Is(err, fs.ErrPermission):
		return newError(CodePermissionDenied, "permission denied for %q", p)
	case errors.Is(err, syscall.ENOTDIR):
		return newError(CodeNotDirectory, "%q is not a directory", p)
	case errors.Is(err, syscall.EISDIR):
		return newError(CodeIsDirectory, "%q is a directory", p)
	case errors.Is(err, syscall.ENOTEMPTY):
		return newError(CodeNotEmpty, "%q is not empty", p)
	case errors.Is(err, fs.ErrExist):
		return newError(CodeExists, "%q already exists", p)
	case errors.Is(err, syscall.ENAMETOOLONG):
		return newError(CodeInvalidPath, "%q is too long", p)
	case errors.Is(err, syscall.ELOOP):
		return newError(CodeInvalidPath, "%q has a symbolic link loop", p)
	case isEscapeError(err):
		return newError(CodeInvalidPath, "%q escapes the workspace root", p)
	default:
		return newError(CodeIOError, "io error for %q%s", p, staticErrorContext(err))
	}
}

// safePathOps is the whitelist of operation names that may appear in a public
// io_error message. The names come from the standard library's PathError.Op
// and are static; anything else is omitted.
var safePathOps = map[string]bool{
	"open":     true,
	"read":     true,
	"write":    true,
	"stat":     true,
	"lstat":    true,
	"readdir":  true,
	"mkdir":    true,
	"remove":   true,
	"rename":   true,
	"close":    true,
	"chmod":    true,
	"chown":    true,
	"chtimes":  true,
	"link":     true,
	"symlink":  true,
	"readlink": true,
}

// staticErrorContext returns a path-free suffix for an io_error message. It
// uses only whitelisted operation names and syscall errno text; it never
// copies the raw error string, which may contain a root-qualified path.
func staticErrorContext(err error) string {
	var op string
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && safePathOps[pathErr.Op] {
		op = pathErr.Op
	}

	var errno syscall.Errno
	var errnoText string
	if errors.As(err, &errno) {
		errnoText = errno.Error()
	}

	switch {
	case op != "" && errnoText != "":
		return ": " + op + ": " + errnoText
	case op != "":
		return ": " + op
	case errnoText != "":
		return ": " + errnoText
	default:
		return ""
	}
}

// redactRoot removes the host workspace root from a public message as a
// last-resort defense. Operation code should already build messages from
// relative paths only; this catches a future regression.
func (a *Adapter) redactRoot(msg string) string {
	if a.root == nil || msg == "" {
		return msg
	}
	root := a.root.Name()
	if root == "" {
		return msg
	}

	candidates := []string{root}
	if cleaned := filepath.Clean(root); cleaned != root {
		candidates = append(candidates, cleaned)
	}

	sep := string(os.PathSeparator)
	for _, candidate := range candidates {
		if candidate == "" || candidate == "." || candidate == sep {
			continue
		}
		msg = replaceRootPrefix(msg, candidate+sep, "<workspace>"+sep)
		if filepath.IsAbs(candidate) {
			msg = replaceRootPrefix(msg, candidate, "<workspace>")
		}
	}
	return msg
}

// replaceRootPrefix replaces candidate only when it starts at a path boundary
// (the start of the string or after a non-path character). This avoids
// mangling a legitimate relative path that happens to contain the root as a
// substring, such as root "a" and relative path "data/file".
func replaceRootPrefix(msg, candidate, replacement string) string {
	if candidate == "" {
		return msg
	}
	var b strings.Builder
	for {
		i := strings.Index(msg, candidate)
		if i < 0 {
			b.WriteString(msg)
			return b.String()
		}
		if i == 0 || !isPathByte(msg[i-1]) {
			b.WriteString(msg[:i])
			b.WriteString(replacement)
			msg = msg[i+len(candidate):]
			continue
		}
		b.WriteString(msg[:i+1])
		msg = msg[i+1:]
	}
}

func isPathByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', ':', '(', '[', '=', ',':
		return false
	}
	return true
}

// isEscapeError recognizes os.Root's unexported "path escapes from parent"
// sentinel. The string is stable in the Go standard library; the check is a
// classification aid, not the security boundary (os.Root enforces the bound).
func isEscapeError(err error) bool {
	return strings.Contains(err.Error(), "escapes from parent")
}
