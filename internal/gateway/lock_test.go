package gateway

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquire_SecondCallFailsWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")

	release, err := Acquire(path)
	require.NoError(t, err)
	defer release()

	_, err2 := Acquire(path)
	require.Error(t, err2)
	assert.Contains(t, err2.Error(), strconv.Itoa(os.Getpid()))
}

// TestAcquire_ExistingFileContentIsIrrelevant is the L1 regression test: the
// kernel lock, not the file's bytes, is what gates acquisition now, so a
// pre-existing file - whatever it contains, stale pid or garbage - must
// never block startup or need a "stale lock" heuristic to remove it first.
func TestAcquire_ExistingFileContentIsIrrelevant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	require.NoError(t, os.WriteFile(path, []byte("not-a-pid, definitely not json either"), 0o644))

	release, err := Acquire(path)
	require.NoError(t, err, "existing file content must never block acquiring the lock")
	defer release()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(os.Getpid()), string(data), "Acquire must overwrite the file with this process's own pid")
}

// TestAcquire_ReleaseThenReacquireSucceeds replaces the old
// "release removes the file" assertion: flock-based locking intentionally
// leaves the lock file in place after release (removing it would reopen the
// exact TOCTOU window flock exists to close) - what actually matters is
// that release genuinely drops the kernel lock, so a subsequent Acquire
// succeeds.
func TestAcquire_ReleaseThenReacquireSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")

	release, err := Acquire(path)
	require.NoError(t, err)
	require.NoError(t, release())

	release2, err := Acquire(path)
	require.NoError(t, err, "release must actually drop the lock, not just close the fd")
	require.NoError(t, release2())
}

// --- Held (cron run's read-only lock check) ---------------------------

func TestHeld_NoLockFile_ReportsNotHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	pid, held, err := Held(path)
	require.NoError(t, err)
	assert.False(t, held)
	assert.Zero(t, pid)
}

func TestHeld_LiveProcess_ReportsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	release, err := Acquire(path)
	require.NoError(t, err)
	defer release()

	pid, held, err := Held(path)
	require.NoError(t, err)
	assert.True(t, held)
	assert.Equal(t, os.Getpid(), pid)
}

// TestHeld_UnlockedFileWithStalePIDContent_ReportsNotHeld proves Held
// trusts the kernel lock, not the file's recorded pid: a file whose content
// names some other (possibly long-dead) pid but that nothing currently
// holds a flock on must report not held.
func TestHeld_UnlockedFileWithStalePIDContent_ReportsNotHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	const stalePID = 2000000000
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(stalePID)), 0o644))

	pid, held, err := Held(path)
	require.NoError(t, err)
	assert.False(t, held, "a file nobody holds the kernel lock on must not be reported as held")
	assert.Equal(t, stalePID, pid, "the recorded pid is still surfaced for the caller's message, just not trusted for liveness")
}

// TestHeld_ReadOnlyLockFile_StillReportsHeld is the L7 regression test:
// Held must open the lock file read-only, not read-write, so a caller that
// lacks write permission on it (e.g. `mtclaw doctor` run as a different
// user) still gets an accurate answer - flock works fine on a read-only fd -
// instead of failing to open the file at all.
func TestHeld_ReadOnlyLockFile_StillReportsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	release, err := Acquire(path)
	require.NoError(t, err)
	defer release()

	require.NoError(t, os.Chmod(path, 0o400))

	pid, held, err := Held(path)
	require.NoError(t, err, "Held must succeed on a lock file this process cannot write")
	assert.True(t, held)
	assert.Equal(t, os.Getpid(), pid)
}

func TestHeld_NeverMutatesTheLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	release, err := Acquire(path)
	require.NoError(t, err)
	defer release()

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, _, err = Held(path)
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "Held must never create, remove, or rewrite the lock file")
}
