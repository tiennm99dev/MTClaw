package gateway

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadPID is a PID value far outside any range a real process ever
// occupies on Windows or POSIX (both cap out many orders of magnitude
// lower in practice), used as a guaranteed-dead PID. Spawning and waiting
// on a short-lived real process was tried first and was flaky: Windows can
// reuse a just-exited PID almost immediately, especially under `go test
// -race`'s extra scheduling overhead while many short-lived test processes
// start and stop.
const deadPID = 2000000000

func TestAcquire_SecondCallFailsWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")

	release, err := Acquire(path)
	require.NoError(t, err)
	defer release()

	_, err2 := Acquire(path)
	require.Error(t, err2)
	assert.Contains(t, err2.Error(), strconv.Itoa(os.Getpid()))
}

func TestAcquire_StaleLockFromDeadPIDIsRemovedAndReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(deadPID)), 0o644))

	release, err := Acquire(path)
	require.NoError(t, err)
	defer release()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(os.Getpid()), string(data), "the stale lock must be replaced with this process's own pid")
}

func TestAcquire_CorruptLockFileIsTreatedAsStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	require.NoError(t, os.WriteFile(path, []byte("not-a-pid"), 0o644))

	release, err := Acquire(path)
	require.NoError(t, err, "a lock file whose liveness cannot be verified must not block startup forever")
	defer release()
}

func TestAcquire_ReleaseRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")

	release, err := Acquire(path)
	require.NoError(t, err)
	require.NoError(t, release())

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "release must remove the lock file")
}

func TestProcessAlive_CurrentProcessIsAlive(t *testing.T) {
	assert.True(t, processAlive(os.Getpid()))
}

func TestProcessAlive_DeadPIDIsNotAlive(t *testing.T) {
	assert.False(t, processAlive(deadPID))
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

func TestHeld_StaleLockFromDeadPID_ReportsNotHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(deadPID)), 0o644))

	pid, held, err := Held(path)
	require.NoError(t, err)
	assert.False(t, held, "a dead pid must not be reported as held")
	assert.Equal(t, deadPID, pid)
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
