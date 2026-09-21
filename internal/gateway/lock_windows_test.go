//go:build windows

package gateway

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// deadPID is a PID value far outside any range a real process ever
// occupies, used as a guaranteed-dead PID.
const deadPID = 2000000000

func TestProcessAlive_CurrentProcessIsAlive(t *testing.T) {
	assert.True(t, processAlive(os.Getpid()))
}

func TestProcessAlive_DeadPIDIsNotAlive(t *testing.T) {
	assert.False(t, processAlive(deadPID))
}
