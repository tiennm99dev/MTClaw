package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
)

// TestProgressReporter_ReusedToolCallID_StopsPriorTimer is the L5 regression
// test: a second EventToolStarted for a tool call id already tracked must
// stop the earlier timer before replacing it, so a provider reusing an id
// within one turn cannot leak a stray "running X..." notice.
func TestProgressReporter_ReusedToolCallID_StopsPriorTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := &fakeChannel{}
	p := newProgressReporter(ctx, ch, "100", "", testLog())
	p.start()

	p.onEvent(agent.Event{Kind: agent.EventToolStarted, ToolCallID: "call-1", ToolName: "exec"})
	p.mu.Lock()
	first := p.timer["call-1"]
	p.mu.Unlock()
	require.NotNil(t, first)

	p.onEvent(agent.Event{Kind: agent.EventToolStarted, ToolCallID: "call-1", ToolName: "exec"})

	// The first timer must have been stopped - waiting past slowToolNotice
	// must never fire it a second time on top of the replacement.
	time.Sleep(20 * time.Millisecond)
	assert.False(t, first.Stop(), "the prior timer must already be stopped, not still pending")

	p.stop()
}

// TestProgressReporter_TimerFiresAfterCtxDone_SendsNothing proves onEvent's
// own slow-tool-notice closure checks ctx before sending, closing the
// window between a timer firing and stop()'s own Stop() call losing the
// race - a notice must never reach the channel once the turn's context is
// already done. It drives the real onEvent through slowNoticeDelay (a short
// stand-in for the real slowToolNotice, which at 8s would make this test
// too slow to be worth running), so reverting onEvent's ctx check would
// fail this test instead of it passing unconditionally.
func TestProgressReporter_TimerFiresAfterCtxDone_SendsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := &fakeChannel{}
	p := newProgressReporter(ctx, ch, "100", "", testLog())
	p.slowNoticeDelay = 5 * time.Millisecond

	p.onEvent(agent.Event{Kind: agent.EventToolStarted, ToolCallID: "call-1", ToolName: "exec"})

	cancel() // the turn ends before the timer fires
	time.Sleep(20 * time.Millisecond)

	assert.Empty(t, ch.sentSnapshot(), "no slow-tool notice must be sent once ctx is already done")
}
