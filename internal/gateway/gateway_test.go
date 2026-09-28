package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store"
)

// errChannelDied is a distinguishable sentinel a fakeGatewayChannel's Start
// returns to simulate a poll loop dying for a reason other than shutdown
// (a revoked token, a dead connection).
var errChannelDied = errors.New("fake channel: simulated failure")

// fakeGatewayChannel implements gatewayChannel (channel.Channel's Start
// plus the dispatcher's own Send/SendTyping) without any network, so
// Gateway.Run's startup/shutdown wiring can be exercised directly.
type fakeGatewayChannel struct {
	fakeChannel

	mu       sync.Mutex
	starts   int
	startErr error
	inject   []channel.Inbound
}

func (f *fakeGatewayChannel) Name() string { return "telegram" }

func (f *fakeGatewayChannel) Start(ctx context.Context, out chan<- channel.Inbound) error {
	f.mu.Lock()
	f.starts++
	err := f.startErr
	msgs := f.inject
	f.mu.Unlock()

	for _, m := range msgs {
		select {
		case out <- m:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeGatewayChannel) startCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

var _ gatewayChannel = (*fakeGatewayChannel)(nil)

// --- Gateway.Run shutdown order ---------------------------------------------

// TestGatewayRun_CleanShutdown_ClosesStoreAndReleasesLock proves the
// documented shutdown order: canceling ctx stops the channel's pump, drains
// in-flight turns, then closes the store and releases the instance lock -
// and Run returns nil for an ordinary, non-breaching shutdown.
func TestGatewayRun_CleanShutdown_ClosesStoreAndReleasesLock(t *testing.T) {
	st := newTestStore(t)
	ch := &fakeGatewayChannel{}
	disp := newDispatcher(st, ch, &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{NoReply: true}
	}}, testLog(), time.Minute, 4)

	var released int32
	g := &Gateway{
		log:     testLog(),
		store:   st,
		channel: ch,
		disp:    disp,
		release: func() error { atomic.StoreInt32(&released, 1); return nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- g.Run(ctx) }()

	waitCond(t, func() bool { return ch.startCalls() > 0 }, time.Second)
	cancel()

	select {
	case err := <-runDone:
		assert.NoError(t, err, "an ordinary shutdown must return nil, not a breach error")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx was canceled")
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&released), "the instance lock must be released on shutdown")

	// The store must actually be closed (drained == true path), not merely
	// left open: a subsequent store call fails once sql.DB is closed.
	_, err := st.Sessions().Ensure(context.Background(), "telegram", "closed-check", "")
	assert.Error(t, err, "the store must be closed after a clean shutdown")
}

// TestGatewayRun_ChannelFailure_ReturnsNonNilAndStillShutsDown proves the
// channel-failure path: if Start returns an error while ctx is not yet
// done (a revoked token, a dead connection - not a caller-driven shutdown),
// Run treats it as fatal, still drains and closes the store, and returns a
// non-nil error so the process exits non-zero.
func TestGatewayRun_ChannelFailure_ReturnsNonNilAndStillShutsDown(t *testing.T) {
	st := newTestStore(t)
	ch := &fakeGatewayChannel{startErr: errChannelDied}
	disp := newDispatcher(st, ch, &fakeRunner{}, testLog(), time.Minute, 4)

	var released int32
	g := &Gateway{
		log:     testLog(),
		store:   st,
		channel: ch,
		disp:    disp,
		release: func() error { atomic.StoreInt32(&released, 1); return nil },
	}

	runDone := make(chan error, 1)
	go func() { runDone <- g.Run(context.Background()) }()

	select {
	case err := <-runDone:
		require.Error(t, err, "a channel failure must make Run return a non-nil error")
		assert.ErrorIs(t, err, errChannelDied)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the channel failed")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&released))
}

// slowGatewayReplyChannel's Send signals started (once) and then blocks
// until unblock closes, standing in for a slow multi-chunk Telegram
// delivery still in flight when shutdown begins.
type slowGatewayReplyChannel struct {
	fakeGatewayChannel
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (c *slowGatewayReplyChannel) Send(ctx context.Context, chatID, threadID, text, replyTo string) error {
	c.once.Do(func() { close(c.started) })
	<-c.unblock
	return c.fakeGatewayChannel.Send(ctx, chatID, threadID, text, replyTo)
}

// TestGatewayRun_WaitsForDetachedReplyBeforeReturning proves Run does not
// return (and so does not close the store or release the lock) until a
// reply produced right before shutdown has actually finished sending - a
// turn's own reply is delivered off dispatcher.wg (which drain waits on) so
// a slow send never looks like a drain-deadline breach, but Run must still
// give it a real chance to go out rather than dropping it the instant
// dispatcher.wg itself has drained.
func TestGatewayRun_WaitsForDetachedReplyBeforeReturning(t *testing.T) {
	st := newTestStore(t)
	ch := &slowGatewayReplyChannel{started: make(chan struct{}), unblock: make(chan struct{})}
	disp := newDispatcher(st, ch, &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{Text: "a reply that takes a while to actually send"}
	}}, testLog(), time.Minute, 4)

	g := &Gateway{log: testLog(), store: st, channel: ch, disp: disp, release: func() error { return nil }}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- g.Run(ctx) }()

	waitCond(t, func() bool { return ch.startCalls() > 0 }, time.Second)
	disp.dispatch(inboundTo("chat1", "hi"))

	// Trigger shutdown only once the reply's Send has actually started -
	// the scenario the fix report says used to be dropped: a reply already
	// in flight when the process starts shutting down.
	select {
	case <-ch.started:
	case <-time.After(time.Second):
		t.Fatal("reply Send was never called")
	}
	cancel()

	select {
	case <-runDone:
		t.Fatal("Run returned before the reply still in flight finished sending")
	case <-time.After(150 * time.Millisecond):
	}

	close(ch.unblock)

	select {
	case err := <-runDone:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return once the in-flight reply finished sending")
	}

	assert.Len(t, ch.sentSnapshot(), 1, "the reply produced right at shutdown must actually have been sent")
}

// TestGatewayRun_InboundMessageReachesDispatcher proves the direct
// channel-to-dispatcher wiring (no intermediate relay or global-queue
// goroutine in between) actually delivers a message the channel injects.
func TestGatewayRun_InboundMessageReachesDispatcher(t *testing.T) {
	st := newTestStore(t)
	handled := make(chan string, 1)
	ch := &fakeGatewayChannel{inject: []channel.Inbound{{Channel: "telegram", ChatID: "42", Text: "hi"}}}
	disp := newDispatcher(st, ch, &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		handled <- userText
		return agent.Result{NoReply: true}
	}}, testLog(), time.Minute, 4)

	g := &Gateway{log: testLog(), store: st, channel: ch, disp: disp, release: func() error { return nil }}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- g.Run(ctx) }()

	select {
	case text := <-handled:
		assert.Equal(t, "hi", text)
	case <-time.After(2 * time.Second):
		t.Fatal("the injected inbound message never reached the dispatcher")
	}
	cancel()
	<-runDone
}

// --- telegramDeps ------------------------------------------------------------

func TestTelegramDeps_Status_ReportsSessionFields(t *testing.T) {
	st := newTestStore(t)
	disp := newDispatcher(st, &fakeChannel{}, &fakeRunner{}, testLog(), time.Minute, 4)
	deps := &telegramDeps{cfg: config.Config{Agent: config.AgentConfig{Model: "gpt-5"}}, store: st, disp: disp}

	sess, err := st.Sessions().Ensure(context.Background(), "telegram", "chat1", "")
	require.NoError(t, err)
	require.NoError(t, st.Messages().Append(context.Background(), sess.ID, []store.Message{{Role: "user", Content: "hi"}}))

	status, err := deps.Status(context.Background(), "chat1", "")
	require.NoError(t, err)
	assert.Equal(t, 1, status.Messages)
	assert.Equal(t, "gpt-5", status.Model)
}

func TestTelegramDeps_Cancel_DelegatesToDispatcher(t *testing.T) {
	st := newTestStore(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		close(started)
		<-ctx.Done()
		close(canceled)
		return agent.Result{Err: &provider.Error{Kind: provider.ErrCanceled}}
	}}
	disp := newDispatcher(st, &fakeChannel{}, runner, testLog(), time.Minute, 4)
	deps := &telegramDeps{cfg: config.Config{}, store: st, disp: disp}

	disp.dispatch(inboundTo("chat1", "hi"))
	<-started

	assert.True(t, deps.Cancel("chat1", ""), "Cancel must report a turn was actually running")
	waitCond(t, func() bool {
		select {
		case <-canceled:
			return true
		default:
			return false
		}
	}, time.Second)

	assert.False(t, deps.Cancel("nonexistent-chat", ""))
}

// TestTelegramDeps_Reset_CancelsRunningTurnThenResets proves /new during a
// running turn cancels that turn and only then deletes its session history
// - on the session's own worker goroutine, so the canceled turn's own
// end-of-turn flush cannot land after the delete and leave the "fresh"
// conversation carrying the old turn's messages.
func TestTelegramDeps_Reset_CancelsRunningTurnThenResets(t *testing.T) {
	st := newTestStore(t)
	turnRunning := make(chan struct{})
	proceedFlush := make(chan struct{})
	var flushed int32

	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		close(turnRunning)
		<-ctx.Done() // Reset cancels this turn
		<-proceedFlush
		require.NoError(t, st.Messages().Append(context.Background(), sessionID, []store.Message{{Role: "user", Content: "leftover from the canceled turn"}}))
		atomic.StoreInt32(&flushed, 1)
		return agent.Result{Err: &provider.Error{Kind: provider.ErrCanceled}}
	}}
	disp := newDispatcher(st, &fakeChannel{}, runner, testLog(), time.Minute, 4)
	deps := &telegramDeps{cfg: config.Config{}, store: st, disp: disp}

	disp.dispatch(inboundTo("chat1", "hi"))
	<-turnRunning

	resetDone := make(chan error, 1)
	go func() { resetDone <- deps.Reset(context.Background(), "chat1", "") }()

	// Reset must block until the canceled turn's own flush actually
	// completes - it must not race ahead of it.
	select {
	case <-resetDone:
		t.Fatal("Reset returned before the canceled turn's own flush completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(proceedFlush)

	select {
	case err := <-resetDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Reset never returned")
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&flushed))

	sess, err := st.Sessions().Ensure(context.Background(), "telegram", "chat1", "")
	require.NoError(t, err)
	count, err := st.Messages().CountBySession(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "the canceled turn's leftover message must not survive Reset")
}

// TestTelegramDeps_Reset_QueuedMessageDuringTurn_NeverRunsAgainstDeletedHistory
// proves the /new-during-a-turn-with-a-queued-message race is fixed
// deterministically, not by luck of Go's select: with a turn in flight and a
// second message already queued behind it when Reset fires, the queued
// message must never run - not against the pre-reset history, and not
// against the post-reset one either, since it was addressed to a
// conversation that no longer exists by the time it would have run. Before
// the control-priority and drop-queued-messages fix, runWorker's own select
// picked between the queued message and Reset's control func at random, so
// this failed about half the time.
func TestTelegramDeps_Reset_QueuedMessageDuringTurn_NeverRunsAgainstDeletedHistory(t *testing.T) {
	st := newTestStore(t)
	turnRunning := make(chan struct{})
	var queuedRan int32

	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		if userText == "queued" {
			atomic.StoreInt32(&queuedRan, 1)
			return agent.Result{NoReply: true}
		}
		close(turnRunning)
		<-ctx.Done() // Reset cancels this turn
		return agent.Result{Err: &provider.Error{Kind: provider.ErrCanceled}}
	}}
	disp := newDispatcher(st, &fakeChannel{}, runner, testLog(), time.Minute, 4)
	deps := &telegramDeps{cfg: config.Config{}, store: st, disp: disp}

	disp.dispatch(inboundTo("chat1", "first"))
	<-turnRunning
	disp.dispatch(inboundTo("chat1", "queued")) // queued behind the running turn

	require.NoError(t, deps.Reset(context.Background(), "chat1", ""))

	// Give a wrongly-processed queued message a chance to actually run
	// before asserting it did not: Reset returning is not itself proof the
	// queued message was dropped rather than about to run.
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int32(0), atomic.LoadInt32(&queuedRan), "the message queued behind the canceled turn must be dropped, never run")

	sess, err := st.Sessions().Ensure(context.Background(), "telegram", "chat1", "")
	require.NoError(t, err)
	count, err := st.Messages().CountBySession(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "Reset must leave a clean, empty session with nothing from either message")
}

// TestTelegramDeps_Reset_NoRunningTurn_StillResets proves the common case
// (no turn in flight) still works: cancelSession is a harmless no-op and
// Reset runs immediately.
func TestTelegramDeps_Reset_NoRunningTurn_StillResets(t *testing.T) {
	st := newTestStore(t)
	disp := newDispatcher(st, &fakeChannel{}, &fakeRunner{}, testLog(), time.Minute, 4)
	deps := &telegramDeps{cfg: config.Config{}, store: st, disp: disp}

	sess, err := st.Sessions().Ensure(context.Background(), "telegram", "chat1", "")
	require.NoError(t, err)
	require.NoError(t, st.Messages().Append(context.Background(), sess.ID, []store.Message{{Role: "user", Content: "hi"}}))

	require.NoError(t, deps.Reset(context.Background(), "chat1", ""))

	count, err := st.Messages().CountBySession(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}
