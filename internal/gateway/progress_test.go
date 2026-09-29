package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
)

// TestProgressReporter_ReusedToolCallID_StopsPriorTimer proves a second
// EventToolStarted for a tool call id already tracked stops the earlier
// timer before replacing it, so a provider reusing an id within one turn
// cannot leak a stray "running X..." notice.
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

// TestProgressReporter_NoticeNeverSentAfterStopReturns proves the slow-tool
// notice cannot land after stop() has returned (and so after the turn's
// reply is on its way): stop waits out a notice already in flight and
// suppresses any that has not started. The timer is set to fire right as
// stop runs, so the callback and stop race on every iteration.
func TestProgressReporter_NoticeNeverSentAfterStopReturns(t *testing.T) {
	for i := 0; i < 300; i++ {
		ch := &stopOrderChannel{}
		p := newProgressReporter(context.Background(), ch, "100", "", testLog())
		p.slowNoticeDelay = 50 * time.Microsecond
		p.start()
		p.onEvent(agent.Event{Kind: agent.EventToolStarted, ToolCallID: "call-1", ToolName: "exec"})

		time.Sleep(50 * time.Microsecond)
		p.stop()
		ch.stopReturned.Store(true)

		time.Sleep(time.Millisecond) // let a leaked callback run
		require.False(t, ch.sentAfterStop.Load(), "iteration %d: a notice was sent after stop returned", i)
	}
}

// stopOrderChannel flags any Send that begins after stopReturned is set.
type stopOrderChannel struct {
	stopReturned  atomic.Bool
	sentAfterStop atomic.Bool
}

func (c *stopOrderChannel) Send(ctx context.Context, _, _, _, _ string) error {
	if c.stopReturned.Load() {
		c.sentAfterStop.Store(true)
	}
	return nil
}

func (c *stopOrderChannel) SendTyping(context.Context, string, string) error { return nil }
