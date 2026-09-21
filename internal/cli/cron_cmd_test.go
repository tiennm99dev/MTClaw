package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tiennm99/MTClaw/internal/tools"
)

// TestNewCronRunApprover_WiresDenyAllApprover pins the other half of the
// approver-wiring invariant the newLoop refactor could silently break:
// `cron run` must always hand the agent loop DenyAllApprover, matching
// exactly what a real gateway picks for channel "cron" - a scheduled or
// manually-fired cron turn never has an interactive approver to ask, so it
// must fail closed rather than run unattended.
func TestNewCronRunApprover_WiresDenyAllApprover(t *testing.T) {
	approver := newCronRunApprover()

	var _ tools.Approver = approver
	approved, err := approver.Ask(context.Background(), tools.Request{})
	assert.False(t, approved)
	assert.ErrorIs(t, err, tools.ErrNoApprover)
}
