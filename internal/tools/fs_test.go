package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
