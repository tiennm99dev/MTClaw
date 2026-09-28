package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
)

func newFSTools(t *testing.T, maxRead, maxWrite int) (*fsTools, string) {
	t.Helper()
	root := t.TempDir()
	return &fsTools{roots: []string{root}, maxReadBytes: maxRead, maxWriteBytes: maxWrite}, root
}

func mustArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestReadFile_ReturnsContent(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello world"), 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "a.txt"}), agent.Meta{})
	require.NoError(t, err)
	assert.Equal(t, "hello world", out)
}

func TestReadFile_TruncationMarker(t *testing.T) {
	f, root := newFSTools(t, 5, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("0123456789"), 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "a.txt"}), agent.Meta{})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "01234"))
	assert.Contains(t, out, "[truncated")
}

func TestReadFile_RefusesBinaryContent(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	binary := append([]byte("some text"), 0x00, 0x01, 0x02)
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin.dat"), binary, 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "bin.dat"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "binary")
}

func TestReadFile_OutsideRootRefused(t *testing.T) {
	f, _ := newFSTools(t, 1024, 1024)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: filepath.Join(outside, "secret.txt")}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "read_file:")
	assert.NotContains(t, out, "nope")
}

// TestReadFile_OffsetPastEOFReturnsExplicitMarker proves an offset beyond
// the file's last byte is reported explicitly, not returned as "" - which
// would be indistinguishable from a genuinely empty file.
func TestReadFile_OffsetPastEOFReturnsExplicitMarker(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "a.txt", Offset: 10}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "offset 10 is past end of file (size 5)")
}

// TestReadFile_OffsetAtExactEOFReturnsExplicitMarker covers the boundary:
// an offset equal to a nonzero file size has nothing left to read either.
func TestReadFile_OffsetAtExactEOFReturnsExplicitMarker(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "a.txt", Offset: 5}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "offset 5 is past end of file (size 5)")
}

// TestReadFile_EmptyFileAtOffsetZeroReturnsEmptyNotAMarker proves offset 0
// against a genuinely empty file is not treated as "past EOF": there is
// nothing past the end to report, just nothing at all.
func TestReadFile_EmptyFileAtOffsetZeroReturnsEmptyNotAMarker(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "empty.txt"), []byte{}, 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "empty.txt"}), agent.Meta{})
	require.NoError(t, err)
	assert.Equal(t, "", out)
}

// TestReadFile_TruncationIsRuneSafe proves a byte limit that lands mid
// multi-byte character is trimmed back to the last complete rune, the same
// guarantee exec output already has.
func TestReadFile_TruncationIsRuneSafe(t *testing.T) {
	f, root := newFSTools(t, 10, 1024) // not a multiple of 3, the byte width of "あ"
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte(strings.Repeat("あ", 5)), 0o644))

	out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "a.txt"}), agent.Meta{})
	require.NoError(t, err)
	body := out[:strings.Index(out, "\n[truncated")]
	assert.True(t, utf8.ValidString(body), "read_file must never return a truncated multi-byte rune")
}

func TestReadFile_RefusesNonRegularFile(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "p")
	requireFIFO(t, fifo)

	f := &fsTools{roots: []string{root}, maxReadBytes: 1024, maxWriteBytes: 1024}

	done := make(chan struct{})
	go func() {
		defer close(done)
		out, err := f.readFile(context.Background(), mustArgs(t, readFileArgs{Path: "p"}), agent.Meta{})
		require.NoError(t, err)
		assert.Contains(t, out, "not a regular file")
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("read_file blocked on a FIFO instead of refusing it before ever calling os.Open")
	}
}

func TestWriteFile_RefusesExistingNonRegularTarget(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "p")
	requireFIFO(t, fifo)

	f := &fsTools{roots: []string{root}, maxReadBytes: 1024, maxWriteBytes: 1024}

	done := make(chan struct{})
	go func() {
		defer close(done)
		out, err := f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: "p", Content: "x"}), agent.Meta{})
		require.NoError(t, err)
		assert.Contains(t, out, "not a regular file")
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("write_file blocked on an existing FIFO instead of refusing it before ever calling os.OpenFile")
	}
}

func TestReadFile_InvalidArgsReturnsResultString(t *testing.T) {
	f, _ := newFSTools(t, 1024, 1024)
	out, err := f.readFile(context.Background(), json.RawMessage(`{not valid json`), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "invalid arguments")
}

func TestWriteFile_OverwriteAndAppend(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)

	_, err := f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: "note.txt", Content: "first"}), agent.Meta{})
	require.NoError(t, err)

	_, err = f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: "note.txt", Content: " second", Mode: "append"}), agent.Meta{})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(root, "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "first second", string(content))

	_, err = f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: "note.txt", Content: "replaced"}), agent.Meta{})
	require.NoError(t, err)
	content, err = os.ReadFile(filepath.Join(root, "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "replaced", string(content))
}

func TestWriteFile_CreateNewFailsOnExisting(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "exists.txt"), []byte("x"), 0o644))

	out, err := f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: "exists.txt", Content: "y", Mode: "create_new"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "already exists")
}

func TestWriteFile_CreatesParentDirsInsideRoot(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	out, err := f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: filepath.Join("a", "b", "c.txt"), Content: "deep"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "wrote")

	content, err := os.ReadFile(filepath.Join(root, "a", "b", "c.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deep", string(content))
}

func TestWriteFile_OutsideRootRefused(t *testing.T) {
	f, _ := newFSTools(t, 1024, 1024)
	outside := t.TempDir()

	out, err := f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: filepath.Join(outside, "x.txt"), Content: "y"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "write_file:")

	_, statErr := os.Stat(filepath.Join(outside, "x.txt"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestWriteFile_ExceedsMaxWriteBytes(t *testing.T) {
	f, _ := newFSTools(t, 1024, 4)
	out, err := f.writeFile(context.Background(), mustArgs(t, writeFileArgs{Path: "big.txt", Content: "way too long"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "max_write_bytes")
}

func TestListDir_EntryCapAndCounts(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))

	out, err := f.listDir(context.Background(), mustArgs(t, listDirArgs{Path: "."}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "f0.txt")
	assert.Contains(t, out, "sub")
	assert.Contains(t, out, "dir")
}

func TestListDir_NeverFollowsSymlinks(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))

	out, err := f.listDir(context.Background(), mustArgs(t, listDirArgs{Path: ".", Depth: 3}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "symlink")
	assert.NotContains(t, out, "secret.txt")
}

func TestListDir_RecursesWithDepth(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "deep.txt"), []byte("x"), 0o644))

	out, err := f.listDir(context.Background(), mustArgs(t, listDirArgs{Path: ".", Depth: 3}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "deep.txt")
}

func TestListDir_NotADirectory(t *testing.T) {
	f, root := newFSTools(t, 1024, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644))

	out, err := f.listDir(context.Background(), mustArgs(t, listDirArgs{Path: "file.txt"}), agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "not a directory")
}

func TestRegisterFilesystemTools_RegistersAllThree(t *testing.T) {
	r := NewRegistry()
	registerFilesystemTools(r, config.FilesystemConfig{Roots: []string{t.TempDir()}, MaxReadBytes: 1024, MaxWriteBytes: 1024})
	names := map[string]bool{}
	for _, s := range r.Specs() {
		names[s.Name] = true
	}
	assert.True(t, names["read_file"])
	assert.True(t, names["write_file"])
	assert.True(t, names["list_dir"])
}
