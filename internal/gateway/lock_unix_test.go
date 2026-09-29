//go:build !windows

package gateway

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquire_WaitsOutAMomentaryProbe proves a lock taken for a few
// milliseconds by another descriptor (what a concurrent Held probe does)
// does not make a starting gateway refuse to run.
func TestAcquire_WaitsOutAMomentaryProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	probe, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	require.NoError(t, err)
	defer probe.Close()
	require.NoError(t, syscall.Flock(int(probe.Fd()), syscall.LOCK_EX))

	fd := int(probe.Fd())
	unlocked := make(chan struct{})
	go func() {
		defer close(unlocked)
		time.Sleep(30 * time.Millisecond)
		_ = syscall.Flock(fd, syscall.LOCK_UN)
	}()

	release, err := Acquire(path)
	<-unlocked
	require.NoError(t, err, "a lock held for ~30ms must not fail Acquire")
	require.NoError(t, release())
}

func TestTryFlock_OnlyWouldBlockMeansHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	require.NoError(t, err)
	defer holder.Close()
	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX))

	other, err := os.OpenFile(path, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer other.Close()

	held, err := tryFlock(int(other.Fd()), syscall.LOCK_SH, 1)
	require.NoError(t, err)
	assert.True(t, held, "EWOULDBLOCK means a peer holds the lock")

	// A closed descriptor fails with EBADF, which says nothing about a
	// peer and must surface as an error rather than as "held".
	closed, err := os.OpenFile(path, os.O_RDONLY, 0)
	require.NoError(t, err)
	fd := int(closed.Fd())
	require.NoError(t, closed.Close())
	held, err = tryFlock(fd, syscall.LOCK_SH, 1)
	require.Error(t, err)
	assert.False(t, held)
}

func TestHeld_ConcurrentProbesDoNotSeeEachOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	probe, err := os.OpenFile(path, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer probe.Close()
	require.NoError(t, syscall.Flock(int(probe.Fd()), syscall.LOCK_SH))

	_, held, err := Held(path)
	require.NoError(t, err)
	assert.False(t, held, "a shared probe lock is not a gateway")
}
