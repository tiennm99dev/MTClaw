package version

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reset restores the three package vars and the resolve-once guard to their
// zero state, so each test in this file starts from a clean slate instead
// of being affected by resolve() having already run (and memoized its
// result) in an earlier test within the same process.
func reset(t *testing.T) {
	t.Helper()
	origVersion, origCommit, origDate := Version, Commit, Date
	Version, Commit, Date = "dev", "none", "unknown"
	resolveOnce = sync.Once{}
	t.Cleanup(func() {
		Version, Commit, Date = origVersion, origCommit, origDate
		resolveOnce = sync.Once{}
	})
}

func TestString_FormatsAllThreeFields(t *testing.T) {
	reset(t)
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-09-28T00:00:00Z"
	resolveOnce.Do(func() {}) // pretend resolve already ran, so it cannot overwrite these
	assert.Equal(t, "mtclaw v1.2.3 (commit abc1234, committed 2026-09-28T00:00:00Z)", String())
}

// TestString_ResolvesFromBuildInfoInARealBuild proves resolve() picks up
// real values from runtime/debug.ReadBuildInfo for an actual `go build`
// binary run directly inside this git checkout, without any -ldflags at
// all. This builds and runs a real binary rather than calling String()
// in-process: a `go test` binary is never VCS-stamped (ReadBuildInfo
// reports no vcs.* settings for it, whatever GOFLAGS is set to), which is
// exactly why the previous version of this test always skipped - see
// version.go's own doc comment for the same "go run"/`go test`
// distinction.
func TestString_ResolvesFromBuildInfoInARealBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real binary; skipped under -short")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	bin := filepath.Join(t.TempDir(), "mtclaw-version-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "version").CombinedOutput()
	require.NoError(t, err)
	got := strings.TrimSpace(string(out))

	assert.True(t, strings.HasPrefix(got, "mtclaw "))
	assert.NotContains(t, got, "commit none", "a plain go build inside this git checkout must resolve a real commit")
	assert.NotContains(t, got, "committed unknown", "a plain go build inside this git checkout must resolve a real commit date")
}

func TestResolve_NeverOverridesAnLdflagsValue(t *testing.T) {
	reset(t)
	Version, Commit, Date = "v9.9.9", "deadbeef", "2020-01-01T00:00:00Z"
	resolve()
	assert.Equal(t, "v9.9.9", Version)
	assert.Equal(t, "deadbeef", Commit)
	assert.Equal(t, "2020-01-01T00:00:00Z", Date)
}
