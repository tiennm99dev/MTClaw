package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
)

// binarySniffBytes caps how much of a file's head read_file inspects for a
// NUL byte before refusing it as binary content the model cannot usefully
// consume as text.
const binarySniffBytes = 8192

// listDirEntryCap bounds how many entries list_dir will ever return, so a
// huge or adversarially wide directory cannot blow up the response.
const listDirEntryCap = 2000

// listDirMaxDepth bounds how deep list_dir will ever recurse regardless of
// the requested depth.
const listDirMaxDepth = 8

type fsTools struct {
	roots         []string
	maxReadBytes  int
	maxWriteBytes int
}

func registerFilesystemTools(r *Registry, cfg config.FilesystemConfig) {
	f := &fsTools{
		roots:         cfg.Roots,
		maxReadBytes:  cfg.MaxReadBytes,
		maxWriteBytes: cfg.MaxWriteBytes,
	}
	r.Register("read_file", Tool{Spec: readFileSpec(), Run: f.readFile})
	r.Register("write_file", Tool{Spec: writeFileSpec(), Run: f.writeFile})
	r.Register("list_dir", Tool{Spec: listDirSpec(), Run: f.listDir})
}

func readFileSpec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "read_file",
		Description: "Read a text file confined to the configured workspace roots. Refuses binary content and truncates large files with an explicit marker.",
		Schema: objectSchema(map[string]any{
			"path":   stringProp("path to the file, absolute or relative to a configured root"),
			"offset": integerProp("byte offset to start reading from (default 0)"),
			"limit":  integerProp("maximum bytes to return (default and cap: the configured max_read_bytes)"),
		}, "path"),
	}
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int64  `json:"limit,omitempty"`
}

// readFile resolves path within the configured roots, refuses binary
// content (a NUL byte in the first binarySniffBytes), and returns content
// bounded by limit (default and cap: maxReadBytes) starting at offset, with
// an explicit truncation marker when the file has more to give.
func (f *fsTools) readFile(_ context.Context, args json.RawMessage, _ agent.Meta) (string, error) {
	var a readFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Sprintf("read_file: invalid arguments: %v", err), nil
	}

	resolved, err := Resolve(f.roots, a.Path)
	if err != nil {
		return fmt.Sprintf("read_file: %v", err), nil
	}

	file, err := os.Open(resolved)
	if err != nil {
		return fmt.Sprintf("read_file: %v", err), nil
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Sprintf("read_file: %v", err), nil
	}
	if info.IsDir() {
		return fmt.Sprintf("read_file: %q is a directory; use list_dir instead", a.Path), nil
	}

	sniff := make([]byte, binarySniffBytes)
	n, err := file.ReadAt(sniff, 0)
	if err != nil && err != io.EOF {
		return fmt.Sprintf("read_file: %v", err), nil
	}
	if bytes.IndexByte(sniff[:n], 0) >= 0 {
		return fmt.Sprintf("read_file: %q looks like binary content (a NUL byte was found in the first %d bytes); refusing to return it as text", a.Path, binarySniffBytes), nil
	}

	limit := a.Limit
	if limit <= 0 || limit > int64(f.maxReadBytes) {
		limit = int64(f.maxReadBytes)
	}
	if a.Offset < 0 {
		return "read_file: offset must not be negative", nil
	}

	buf := make([]byte, limit)
	n2, err := file.ReadAt(buf, a.Offset)
	if err != nil && err != io.EOF {
		return fmt.Sprintf("read_file: %v", err), nil
	}
	content := buf[:n2]
	// More remains past what we read if the file is longer than offset+n2.
	truncated := a.Offset+int64(n2) < info.Size()

	var b strings.Builder
	b.Write(content)
	if truncated {
		fmt.Fprintf(&b, "\n[truncated: showing %d bytes starting at offset %d; file is %d bytes total]", n2, a.Offset, info.Size())
	}
	return b.String(), nil
}

func writeFileSpec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "write_file",
		Description: "Write a text file confined to the configured workspace roots. Parent directories are created inside the root when needed.",
		Schema: objectSchema(map[string]any{
			"path":    stringProp("path to the file, absolute or relative to a configured root"),
			"content": stringProp("the full text content to write"),
			"mode":    enumProp("overwrite (default): replace the file; append: add to the end; create_new: fail if the file already exists", "overwrite", "append", "create_new"),
		}, "path", "content"),
	}
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode,omitempty"`
}

// writeFile resolves path within the configured roots, creates any missing
// parent directories (which Resolve has already proven stay inside a root),
// and writes content according to mode, bounded by maxWriteBytes.
func (f *fsTools) writeFile(_ context.Context, args json.RawMessage, _ agent.Meta) (string, error) {
	var a writeFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Sprintf("write_file: invalid arguments: %v", err), nil
	}

	mode := a.Mode
	if mode == "" {
		mode = "overwrite"
	}

	var flags int
	switch mode {
	case "overwrite":
		flags = os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	case "append":
		flags = os.O_CREATE | os.O_APPEND | os.O_WRONLY
	case "create_new":
		flags = os.O_CREATE | os.O_EXCL | os.O_WRONLY
	default:
		return fmt.Sprintf("write_file: invalid mode %q; must be one of overwrite, append, create_new", a.Mode), nil
	}

	if len(a.Content) > f.maxWriteBytes {
		return fmt.Sprintf("write_file: content is %d bytes, which exceeds the configured max_write_bytes of %d", len(a.Content), f.maxWriteBytes), nil
	}

	resolved, err := Resolve(f.roots, a.Path)
	if err != nil {
		return fmt.Sprintf("write_file: %v", err), nil
	}

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return fmt.Sprintf("write_file: create parent directories: %v", err), nil
	}

	file, err := os.OpenFile(resolved, flags, 0o644)
	if err != nil {
		if mode == "create_new" && os.IsExist(err) {
			return fmt.Sprintf("write_file: %q already exists; use mode \"overwrite\" or \"append\" to modify it", a.Path), nil
		}
		return fmt.Sprintf("write_file: %v", err), nil
	}
	defer file.Close()

	n, err := file.WriteString(a.Content)
	if err != nil {
		return fmt.Sprintf("write_file: %v", err), nil
	}
	return fmt.Sprintf("write_file: wrote %d bytes to %q (mode: %s)", n, a.Path, mode), nil
}

func listDirSpec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "list_dir",
		Description: "List a directory's entries (name, type, size), confined to the configured workspace roots. Never follows symlinks.",
		Schema: objectSchema(map[string]any{
			"path":  stringProp("path to the directory, absolute or relative to a configured root"),
			"depth": integerProp("how many directory levels to recurse (default 1, capped at 8)"),
		}, "path"),
	}
}

type listDirArgs struct {
	Path  string `json:"path"`
	Depth int    `json:"depth,omitempty"`
}

// listDir resolves path within the configured roots and lists its entries
// up to depth levels deep (default 1, capped at listDirMaxDepth), capping
// the total entry count at listDirEntryCap. It never descends into a
// symlinked directory - regardless of whether the symlink's target is
// itself inside a root - which is the simplest rule that both prevents
// escaping the root and avoids symlink-cycle loops.
func (f *fsTools) listDir(_ context.Context, args json.RawMessage, _ agent.Meta) (string, error) {
	var a listDirArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Sprintf("list_dir: invalid arguments: %v", err), nil
	}

	depth := a.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > listDirMaxDepth {
		depth = listDirMaxDepth
	}

	resolved, err := Resolve(f.roots, a.Path)
	if err != nil {
		return fmt.Sprintf("list_dir: %v", err), nil
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Sprintf("list_dir: %v", err), nil
	}
	if !info.IsDir() {
		return fmt.Sprintf("list_dir: %q is not a directory; use read_file instead", a.Path), nil
	}

	var lines []string
	count := 0
	capped := walkDir(resolved, depth, "", &lines, &count)

	var b strings.Builder
	fmt.Fprintf(&b, "list_dir: %s\n", a.Path)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if capped {
		fmt.Fprintf(&b, "[entry cap of %d reached; output truncated]\n", listDirEntryCap)
	}
	return b.String(), nil
}

// walkDir appends one line per entry under dir to out, recursing while
// depth remains and count is under listDirEntryCap. It returns true if the
// cap was hit before the tree was fully listed.
func walkDir(dir string, depth int, prefix string, out *[]string, count *int) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		*out = append(*out, prefix+fmt.Sprintf("[error reading directory: %v]", err))
		return false
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if *count >= listDirEntryCap {
			return true
		}

		info, err := entry.Info() // Lstat-based: a symlink's own info, not its target's
		if err != nil {
			*out = append(*out, prefix+entry.Name()+"\t[error: "+err.Error()+"]")
			*count++
			continue
		}

		typ := "file"
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			typ = "symlink"
		case entry.IsDir():
			typ = "dir"
		}

		size := int64(0)
		if typ == "file" {
			size = info.Size()
		}
		*out = append(*out, fmt.Sprintf("%s%s\t%s\t%d", prefix, entry.Name(), typ, size))
		*count++

		if typ == "dir" && depth > 1 {
			if walkDir(filepath.Join(dir, entry.Name()), depth-1, prefix+"  ", out, count) {
				return true
			}
		}
	}
	return false
}
