//go:build linux

package cli

import (
	"bytes"
	"context"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCronRunCmd_DisablesEnvironRead proves `cron run` closes the
// /proc/<pid>/environ vector the same way `mtclaw gateway` does: both
// `prompt` and `cron run` go through state.newLoop, which calls
// tools.DisableEnvironRead once, so a same-uid process cannot read this
// process's OPENAI_API_KEY straight out of /proc/<pid>/environ regardless
// of what the exec tool's own spawned child inherits. Before this fix,
// `cron run`'s RunE never reached tools.DisableEnvironRead at all, leaving
// that vector open for a manually-fired job even though `docs/security.md`
// promised it was closed for every exec-capable command.
func TestCronRunCmd_DisablesEnvironRead(t *testing.T) {
	restoreDumpable(t)

	srv := fakeOpenAIChatCompletion(t, "hello from the job")
	configPath, _ := cronTestConfigPath(t, srv)

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "cron", "run", "daily"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	assert.Equal(t, 0, getDumpable(t), "cron run must disable /proc/<pid>/environ read the same way gateway does")
}

// getDumpable reads this process's current PR_GET_DUMPABLE value directly,
// independent of tools.DisableEnvironRead, so the test asserts on the real
// kernel-visible effect rather than merely that some function was called.
func getDumpable(t *testing.T) int {
	t.Helper()
	r, _, errno := syscall.Syscall(syscall.SYS_PRCTL, uintptr(syscall.PR_GET_DUMPABLE), 0, 0)
	require.Zero(t, int(errno), "PR_GET_DUMPABLE prctl call failed")
	return int(r)
}

// restoreDumpable resets PR_SET_DUMPABLE back to 1 (the default) once the
// test finishes: DisableEnvironRead's effect is process-wide and otherwise
// would leak into every test that runs afterward in this same test binary.
func restoreDumpable(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_, _, _ = syscall.Syscall(syscall.SYS_PRCTL, uintptr(syscall.PR_SET_DUMPABLE), 1, 0)
	})
}
