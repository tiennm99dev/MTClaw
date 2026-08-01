package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve_RejectsEmptyPath(t *testing.T) {
	root := t.TempDir()
	_, err := Resolve([]string{root}, "")
	assert.ErrorIs(t, err, ErrPathEmpty)
}

func TestResolve_RejectsNULByte(t *testing.T) {
	root := t.TempDir()
	_, err := Resolve([]string{root}, "foo\x00bar")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NUL byte")
}

func TestResolve_RelativePathInsideRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("hi"), 0o644))

	got, err := Resolve([]string{root}, "file.txt")
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(filepath.Join(root, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestResolve_TraversalOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	_, err := Resolve([]string{root}, "../escaped.txt")
	assert.Error(t, err)
}

func TestResolve_AbsolutePathOutsideRootRejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.MkdirAll(root, 0o755))
	outside := t.TempDir()

	_, err := Resolve([]string{root}, filepath.Join(outside, "x"))
	assert.Error(t, err)
}

// TestResolve_PrefixConfusionRejected is the "/data-evil vs /data" case from
// the phase 5 spec: a naive strings.HasPrefix(path, root) check would wrongly
// accept a sibling directory that merely starts with the same characters.
func TestResolve_PrefixConfusionRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	evilSibling := filepath.Join(base, "data-evil")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(evilSibling, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(evilSibling, "secret.txt"), []byte("x"), 0o644))

	_, err := Resolve([]string{root}, filepath.Join(evilSibling, "secret.txt"))
	assert.Error(t, err)
}

func TestResolve_SymlinkEscapingRootRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644))

	link := filepath.Join(root, "escape")
	require.NoError(t, os.Symlink(outside, link))

	_, err := Resolve([]string{root}, filepath.Join(link, "secret.txt"))
	assert.Error(t, err)
}

func TestResolve_SymlinkInsideRootAccepted(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "f.txt"), []byte("x"), 0o644))

	link := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(real, link))

	got, err := Resolve([]string{root}, filepath.Join(link, "f.txt"))
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(filepath.Join(real, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestResolve_NonExistentFileInExistingDir exercises the deepest-existing-
// ancestor + rejoin-tail logic write_file depends on: a brand new file must
// still resolve, not fail just because it does not exist yet.
func TestResolve_NonExistentFileInExistingDir(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve([]string{root}, "brand-new-file.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "brand-new-file.txt"), filepath.Clean(got))
}

func TestResolve_NonExistentNestedPathInExistingDir(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve([]string{root}, filepath.Join("sub", "new", "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "sub", "new", "file.txt"), filepath.Clean(got))
}

func TestResolve_MultipleRootsFirstMatchWins(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootB, "f.txt"), []byte("x"), 0o644))

	got, err := Resolve([]string{rootA, rootB}, filepath.Join(rootB, "f.txt"))
	require.NoError(t, err)
	want, _ := filepath.EvalSymlinks(filepath.Join(rootB, "f.txt"))
	assert.Equal(t, want, got)
}

func TestResolve_CaseInsensitiveOnWindowsAndDarwin(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("case-insensitive containment only applies on windows/darwin")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644))

	upper := toUpperPath(root)
	_, err := Resolve([]string{upper}, "file.txt")
	assert.NoError(t, err)
}

// toUpperPath upper-cases root wholesale; it only needs to produce a
// differently-cased but equivalent path for the test above, not a
// general-purpose case transform.
func toUpperPath(p string) string {
	out := make([]rune, 0, len(p))
	for _, r := range p {
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		out = append(out, r)
	}
	return string(out)
}

func TestResolve_NoRootsConfigured(t *testing.T) {
	_, err := Resolve(nil, "file.txt")
	assert.Error(t, err)
}
