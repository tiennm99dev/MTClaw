package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/provider"
)

// --- serialization -----------------------------------------------------

func TestDispatch_SameSession_TurnsNeverOverlap(t *testing.T) {
	type interval struct{ start, end time.Time }
	var mu sync.Mutex
	var intervals []interval
	done := make(chan struct{}, 2)

	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		start := time.Now()
		time.Sleep(40 * time.Millisecond)
		end := time.Now()
		mu.Lock()
		intervals = append(intervals, interval{start, end})
		mu.Unlock()
		done <- struct{}{}
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, 4)

	in := inboundTo("same-session", "hi")
	d.dispatch(in)
	d.dispatch(in)

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("turn did not complete in time")
		}
	}

	require.Len(t, intervals, 2)
	a, b := intervals[0], intervals[1]
	overlap := a.start.Before(b.end) && b.start.Before(a.end)
	assert.False(t, overlap, "two turns in the same session must never run concurrently")
}

func TestDispatch_DifferentSessions_TurnsRunConcurrently(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		wg.Done()
		// Only reachable by both calls if they are running at the same
		// time: if the dispatcher accidentally serialized these two
		// distinct sessions onto one worker, the second call could never
		// start while this one blocks here, and the test times out.
		wg.Wait()
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, 4)

	d.dispatch(inboundTo("s1", "hi"))
	d.dispatch(inboundTo("s2", "hi"))

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for concurrent turns across different sessions")
	}
}

// --- overflow ------------------------------------------------------------

func TestDispatch_SessionQueueFull_OneHonestReplyNoLostMessages(t *testing.T) {
	block := make(chan struct{})
	var mu sync.Mutex
	processed := 0
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		<-block
		mu.Lock()
		processed++
		mu.Unlock()
		return agent.Result{NoReply: true}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)

	in := inboundTo("busy", "msg")
	d.dispatch(in) // enters runTurn immediately and blocks on <-block

	// Give the worker a moment to actually pull this first message off the
	// queue (so the queue is empty again) before filling it.
	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		w, ok := d.workers[sessionKey("telegram", "busy", "")]
		return ok && len(w.queue) == 0
	}, time.Second)

	for i := 0; i < sessionQueueSize; i++ {
		d.dispatch(in)
	}
	d.dispatch(in) // this one must overflow

	close(block)
	waitCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return processed == 1+sessionQueueSize
	}, 2*time.Second)

	sent := ch.sentSnapshot()
	require.Len(t, sent, 1, "exactly one overflow reply, no more")
	assert.Contains(t, sent[0].text, "still working")
}

// TestDispatch_SessionQueueFull_RepeatedOverflow_OneReplyPerBurst proves
// overflowing a session's queue several times in a row (one burst) produces
// exactly one "still working" reply, not one per dropped message - while a
// second, later burst (after the worker actually made progress in between)
// gets its own notice.
func TestDispatch_SessionQueueFull_RepeatedOverflow_OneReplyPerBurst(t *testing.T) {
	// Each burst's occupying message blocks on its own fixed gate (picked by
	// its text, never reassigned) and signals occupying1/occupying2 the
	// instant it reaches that blocking point - waiting on that signal,
	// rather than merely on the message having left the queue, matters
	// because runTurn does real work (Sessions().Ensure against the real
	// temp store, semaphore acquisition) between dequeuing a message and
	// actually calling the runner, so "queue is empty" alone does not prove
	// the occupier has started blocking yet.
	gate1, gate2 := make(chan struct{}), make(chan struct{})
	occupying1, occupying2 := make(chan struct{}), make(chan struct{})
	var processed int32
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		switch userText {
		case "occupy1":
			close(occupying1)
			<-gate1
		case "occupy2":
			close(occupying2)
			<-gate2
		}
		atomic.AddInt32(&processed, 1)
		return agent.Result{NoReply: true}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)
	fillMsg := inboundTo("burst", "fill")

	runBurst := func(occupyText string, occupying <-chan struct{}, wantReplies int) {
		d.dispatch(inboundTo("burst", occupyText)) // occupies the worker
		select {
		case <-occupying:
		case <-time.After(2 * time.Second):
			t.Fatal("the occupying turn never reached its blocking point")
		}

		for i := 0; i < sessionQueueSize; i++ {
			d.dispatch(fillMsg)
		}
		for i := 0; i < 5; i++ {
			d.dispatch(fillMsg) // five overflowing dispatches, one burst
		}

		waitCond(t, func() bool { return len(ch.sentSnapshot()) >= wantReplies }, time.Second)
		time.Sleep(50 * time.Millisecond) // let a wrongly-repeated notice (if the bug were present) arrive
		require.Lenf(t, ch.sentSnapshot(), wantReplies, "burst ending at %d total replies", wantReplies)
	}

	runBurst("occupy1", occupying1, 1)
	assert.Contains(t, ch.sentSnapshot()[0].text, "still working")

	// Release everything queued so far (the occupier plus sessionQueueSize
	// queued messages, none of which block - only "occupy1"/"occupy2" do)
	// and confirm the worker actually made progress before starting a
	// second, separate burst.
	close(gate1)
	waitCond(t, func() bool { return atomic.LoadInt32(&processed) == int32(1+sessionQueueSize) }, 2*time.Second)

	runBurst("occupy2", occupying2, 2) // a second, later burst must get its own notice
	close(gate2)
}

// TestDispatch_SessionQueueFull_OnDoneCalledWithError proves a message
// dropped because its session queue is already full must still invoke
// OnDone (with a non-nil error), not just skip it silently - for
// cron.Scheduler, a skipped OnDone means the job's overlap-guard flag
// never clears and the job is wedged until the gateway restarts.
func TestDispatch_SessionQueueFull_OnDoneCalledWithError(t *testing.T) {
	block := make(chan struct{})
	var mu sync.Mutex
	processed := 0
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		<-block
		mu.Lock()
		processed++
		mu.Unlock()
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, 4)

	in := inboundTo("busy-ondone", "msg")
	d.dispatch(in) // enters runTurn immediately and blocks on <-block

	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		w, ok := d.workers[sessionKey("telegram", "busy-ondone", "")]
		return ok && len(w.queue) == 0
	}, time.Second)

	for i := 0; i < sessionQueueSize; i++ {
		d.dispatch(in)
	}

	var gotErr error
	done := make(chan struct{})
	overflow := in
	overflow.OnDone = func(err error) { gotErr = err; close(done) }
	d.dispatch(overflow) // this one must overflow

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDone was never called for the message dropped by a full session queue")
	}
	assert.Error(t, gotErr)

	close(block)
	waitCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return processed == 1+sessionQueueSize
	}, 2*time.Second)
}

// TestDispatch_SessionQueueFull_CronInbound_NoSendAttempted proves a
// dropped cron Inbound's ChatID is a synthetic job key
// ("job:<name>"), not a real chat, so replySessionBusy must never attempt a
// Send for it - OnDone(err) is the only signal the scheduler needs.
func TestDispatch_SessionQueueFull_CronInbound_NoSendAttempted(t *testing.T) {
	block := make(chan struct{})
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		<-block
		return agent.Result{NoReply: true}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)

	in := channel.Inbound{Channel: "cron", ChatID: "job:overflow-job", Text: "run"}
	d.dispatch(in) // enters runTurn immediately and blocks on <-block

	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		w, ok := d.workers[sessionKey("cron", "job:overflow-job", "")]
		return ok && len(w.queue) == 0
	}, time.Second)

	for i := 0; i < sessionQueueSize; i++ {
		d.dispatch(in)
	}

	var gotErr error
	done := make(chan struct{})
	overflow := in
	overflow.OnDone = func(err error) { gotErr = err; close(done) }
	d.dispatch(overflow) // this one must overflow

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDone was never called for the dropped cron message")
	}
	assert.Error(t, gotErr)
	assert.Empty(t, ch.sentSnapshot(), "a dropped cron inbound's synthetic chat id must never be sent to")

	close(block)
}

// TestDispatch_EnsureSessionFailure_OnDoneCalledWithError proves Sessions().Ensure
// failing in runTurn must still invoke OnDone, not just log and return.
func TestDispatch_EnsureSessionFailure_OnDoneCalledWithError(t *testing.T) {
	d := newTestDispatcher(t, &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{NoReply: true}
	}}, &fakeChannel{}, time.Minute, 4)

	require.NoError(t, d.store.Close()) // force every subsequent Sessions().Ensure call to fail

	var gotErr error
	done := make(chan struct{})
	in := inboundTo("ensure-fail", "hi")
	in.OnDone = func(err error) { gotErr = err; close(done) }
	d.dispatch(in)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDone was never called after Sessions().Ensure failed")
	}
	assert.Error(t, gotErr)
}

// TestDispatch_WorkerExitWithQueuedItem_OnDoneCalledForEachQueued proves a
// worker exiting at shutdown with messages still sitting in its queue must
// still fire OnDone for every one of them via drainQueueOnDone, which
// runWorker's rootCtx.Done() branch calls.
func TestDispatch_WorkerExitWithQueuedItem_OnDoneCalledForEachQueued(t *testing.T) {
	d := newTestDispatcher(t, &fakeRunner{}, &fakeChannel{}, time.Minute, 4)

	rootCtx, cancel := context.WithCancel(context.Background())
	d.rootCtx = rootCtx
	cancel() // simulate the gateway already shutting down

	w := &worker{key: "shutdown-drain", queue: make(chan channel.Inbound, sessionQueueSize)}
	var mu sync.Mutex
	var gotErrs []error
	for i := 0; i < 3; i++ {
		in := inboundTo("shutdown-drain", "queued")
		in.OnDone = func(err error) {
			mu.Lock()
			gotErrs = append(gotErrs, err)
			mu.Unlock()
		}
		require.True(t, trySend(w.queue, in))
	}

	d.drainQueueOnDone(w, d.rootCtx.Err())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotErrs, 3, "every message still queued when the worker exits must still get its OnDone call")
	for _, err := range gotErrs {
		assert.ErrorIs(t, err, context.Canceled)
	}
}

// TestDispatch_AfterRootCtxCanceled_OnDoneFiresImmediately proves dispatch's
// own closed/rootCtx guard: a message dispatched after rootCtx is already
// canceled must get its OnDone call right away, without ever being hidden
// behind a spawned worker's queue.
func TestDispatch_AfterRootCtxCanceled_OnDoneFiresImmediately(t *testing.T) {
	d := newTestDispatcher(t, &fakeRunner{}, &fakeChannel{}, time.Minute, 4)

	rootCtx, cancel := context.WithCancel(context.Background())
	d.rootCtx = rootCtx
	cancel()

	var gotErr error
	done := make(chan struct{})
	in := inboundTo("after-shutdown", "hi")
	in.OnDone = func(err error) { gotErr = err; close(done) }
	d.dispatch(in)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnDone was never called for a message dispatched after rootCtx was already canceled")
	}
	assert.ErrorIs(t, gotErr, context.Canceled)

	d.mu.Lock()
	_, exists := d.workers[sessionKey("telegram", "after-shutdown", "")]
	d.mu.Unlock()
	assert.False(t, exists, "no worker should be spawned for a message dropped before it ever ran")
}

// TestDispatch_RunWorker_RootCtxDoneRemovesItselfFromMap proves runWorker's
// rootCtx.Done() branch: a live worker exiting because the gateway is
// shutting down must remove itself from
// d.workers exactly like the idle-reap branch does, so a dispatch racing
// the exit sees a miss and spawns a fresh worker instead of enqueuing into
// a queue nothing will ever drain.
func TestDispatch_RunWorker_RootCtxDoneRemovesItselfFromMap(t *testing.T) {
	d := newTestDispatcher(t, &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{NoReply: true}
	}}, &fakeChannel{}, time.Minute, 4)

	rootCtx, cancel := context.WithCancel(context.Background())
	d.rootCtx = rootCtx

	key := sessionKey("telegram", "ctx-done", "")
	d.dispatch(inboundTo("ctx-done", "hi"))
	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, ok := d.workers[key]
		return ok
	}, time.Second)

	cancel()

	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, exists := d.workers[key]
		return !exists
	}, time.Second)
	waitGroupDone(t, &d.wg, time.Second)
}

// slowSendChannel's Send blocks until unblock closes, standing in for a
// slow multi-chunk Telegram delivery.
type slowSendChannel struct {
	fakeChannel
	unblock chan struct{}
}

func (c *slowSendChannel) Send(ctx context.Context, chatID, threadID, text, replyTo string) error {
	<-c.unblock
	return c.fakeChannel.Send(ctx, chatID, threadID, text, replyTo)
}

// TestDispatch_ReplySendRunsDetachedFromWorkerGroup proves a reply's own
// outbound Send does not hold up d.wg (which drain waits on) - otherwise a
// slow multi-chunk delivery makes an otherwise-clean shutdown look like a
// drain-deadline breach, even though the turn's own work (history, OnDone)
// already finished.
func TestDispatch_ReplySendRunsDetachedFromWorkerGroup(t *testing.T) {
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{Text: "a reply that will take a while to actually send"}
	}}
	ch := &slowSendChannel{unblock: make(chan struct{})}
	d := newTestDispatcher(t, runner, ch, 20*time.Millisecond, 4)

	d.dispatch(inboundTo("slow-send", "hi"))

	// The worker's own turn finishes and it goes idle (and is eventually
	// reaped) long before the reply's Send ever returns - proving the send
	// is not tracked by d.wg.
	waitGroupDone(t, &d.wg, time.Second)
	assert.Empty(t, ch.sentSnapshot(), "the send is still blocked; it must not have completed yet")

	close(ch.unblock)
	waitCond(t, func() bool { return len(ch.sentSnapshot()) == 1 }, time.Second)
}

// --- idle reap -------------------------------------------------------------

func TestDispatch_IdleReap_RespawnsAndProcessesLaterMessages(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		mu.Lock()
		calls++
		mu.Unlock()
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, 20*time.Millisecond, 4)

	key := sessionKey("telegram", "idle", "")
	in := inboundTo("idle", "hi")
	d.dispatch(in)
	waitCond(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 1 }, time.Second)

	// Wait past the idle timeout, and confirm the worker reaped itself
	// (removed from the map) with no goroutine left running.
	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, exists := d.workers[key]
		return !exists
	}, time.Second)
	waitGroupDone(t, &d.wg, time.Second)

	// A later message must respawn a fresh worker and still be processed.
	d.dispatch(in)
	waitCond(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 2 }, time.Second)
}

// waitGroupDone asserts wg reaches zero within timeout, proving no worker
// goroutine is left running (the "no goroutine leak" requirement).
func waitGroupDone(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("worker goroutines are still running; idle reap leaked a goroutine")
	}
}

// --- reap/enqueue race -----------------------------------------------------

// TestDispatch_ReapEnqueueRace_NoMessageIsEverDropped exercises the
// reap/enqueue race directly: with the idle timeout set to ~1ms, the
// worker's decision to reap and the dispatcher's decision to enqueue race
// constantly. Every dispatched message must still eventually be processed -
// a count, not a smoke test. Run with -race; the shared mutex discipline in
// dispatch/runWorker is exactly what this is asserting.
func TestDispatch_ReapEnqueueRace_NoMessageIsEverDropped(t *testing.T) {
	const n = 1500
	processedCh := make(chan struct{}, 1)
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		processedCh <- struct{}{}
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Millisecond, 4)

	in := inboundTo("race", "x")
	processed := 0
	for i := 0; i < n; i++ {
		d.dispatch(in)
		select {
		case <-processedCh:
			processed++
		case <-time.After(2 * time.Second):
			t.Fatalf("message %d/%d was never processed: the reap/enqueue race dropped it", i+1, n)
		}
	}
	assert.Equal(t, n, processed)
}

// --- global concurrency cap -------------------------------------------------

func TestDispatch_GlobalConcurrencyCap_LimitsConcurrentTurns(t *testing.T) {
	const cap = 2
	const sessions = 5
	var mu sync.Mutex
	current, maxSeen, completed := 0, 0, 0
	release := make(chan struct{})

	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		mu.Lock()
		current++
		if current > maxSeen {
			maxSeen = current
		}
		mu.Unlock()

		<-release

		mu.Lock()
		current--
		completed++
		mu.Unlock()
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, cap)

	for i := 0; i < sessions; i++ {
		d.dispatch(inboundTo(strconv.Itoa(i), "hi"))
	}

	// Let every session pile up against the semaphore.
	waitCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return current == cap
	}, 2*time.Second)

	mu.Lock()
	assert.LessOrEqual(t, maxSeen, cap, "at most `cap` turns must run concurrently")
	mu.Unlock()

	close(release)
	waitCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return completed == sessions
	}, 2*time.Second)
}

// --- cancellation / stop ---------------------------------------------------

func TestDispatch_CancelSession_StopsInFlightTurn_WorkerSurvives(t *testing.T) {
	started := make(chan struct{})
	var calls int
	var mu sync.Mutex
	var canceledErr error

	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(started)
			<-ctx.Done()
			mu.Lock()
			canceledErr = ctx.Err()
			mu.Unlock()
			return agent.Result{Err: &provider.Error{Kind: provider.ErrCanceled}}
		}
		return agent.Result{Text: "ok"}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)

	key := sessionKey("telegram", "stop-me", "")
	in := inboundTo("stop-me", "long task")
	d.dispatch(in)
	<-started

	ok := d.cancelSession(key)
	assert.True(t, ok, "cancel must report a turn was actually running")

	waitCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return canceledErr != nil
	}, time.Second)
	assert.True(t, errors.Is(canceledErr, context.Canceled))

	// The worker must still be usable afterward.
	d.dispatch(in)
	waitCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 2
	}, time.Second)

	// reply runs detached from the worker (see runTurn), so it can still be
	// in flight for a moment after the worker itself reports calls == 2.
	waitCond(t, func() bool { return len(ch.sentSnapshot()) == 1 }, time.Second)
	sent := ch.sentSnapshot()
	require.Len(t, sent, 1, "the canceled turn must not also get a spurious error reply")
	assert.Equal(t, "ok", sent[0].text)

	// Now idle: cancelSession must report false ("nothing running").
	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		w, exists := d.workers[key]
		return exists && w.cancel == nil
	}, time.Second)
	assert.False(t, d.cancelSession(key))
}

func TestDispatch_CancelSession_UnknownKeyReturnsFalse(t *testing.T) {
	d := newTestDispatcher(t, &fakeRunner{}, &fakeChannel{}, time.Minute, 4)
	assert.False(t, d.cancelSession(sessionKey("telegram", "never-seen", "")))
}

// TestDispatch_CancelSession_WhileQueuedBehindGlobalSemaphore proves /stop
// for a turn still waiting on the global concurrency slot (queued behind
// other sessions' turns, never having reached Ensure yet) reports a turn
// was actually running and stops it from ever running at all - not
// "nothing running" followed by the turn going ahead once a slot frees up.
func TestDispatch_CancelSession_WhileQueuedBehindGlobalSemaphore(t *testing.T) {
	holdSlot := make(chan struct{})
	var callNum int32
	var ranQueued int32
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		if atomic.AddInt32(&callNum, 1) == 1 {
			// The first call into the runner is always the occupier
			// (dispatched, and so already inside runTurn, before the
			// second session is ever dispatched below).
			<-holdSlot
			return agent.Result{NoReply: true}
		}
		atomic.AddInt32(&ranQueued, 1)
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, 1) // concurrency=1: one slot total

	d.dispatch(inboundTo("occupier-chat", "hold"))
	waitCond(t, func() bool { return atomic.LoadInt32(&callNum) == 1 }, time.Second)

	// A second session's turn must queue behind the taken semaphore slot,
	// never reaching Ensure/runner.Run yet.
	key := sessionKey("telegram", "queued-behind-sem", "")
	d.dispatch(inboundTo("queued-behind-sem", "hi"))
	// w.cancel must be set as soon as the turn is queued, before the
	// semaphore is even acquired.
	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		w, ok := d.workers[key]
		return ok && w.cancel != nil
	}, time.Second)

	assert.True(t, d.cancelSession(key), "/stop must report a turn was running even while only queued behind the semaphore")

	close(holdSlot)
	// Give the queued turn a moment to (wrongly) run if cancellation did not
	// actually stop it before the semaphore freed up.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&ranQueued), "a canceled turn must never reach the runner at all")
}

// TestDispatch_ShutdownRace_QueuedMessageGetsNoErrorLogNoise proves
// runWorker's select, which can pick an already-queued message over an
// equally-ready rootCtx.Done() case (Go's select chooses randomly among
// ready cases), still resolves a message that loses that race exactly like
// one that never got a chance to dequeue at all - OnDone fired with the
// shutdown's own context.Canceled, no attempt to run the turn, and no
// ERROR-level log line for what is normal shutdown behavior.
func TestDispatch_ShutdownRace_QueuedMessageGetsNoErrorLogNoise(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	var ranTurn int32
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		atomic.AddInt32(&ranTurn, 1)
		return agent.Result{NoReply: true}
	}}

	st := newTestStore(t)
	d := newDispatcher(st, &fakeChannel{}, runner, log, time.Minute, 4)
	rootCtx, cancel := context.WithCancel(context.Background())
	d.rootCtx = rootCtx

	w := &worker{key: "shutdown-race", queue: make(chan channel.Inbound, 1), control: make(chan func())}
	d.mu.Lock()
	d.workers[w.key] = w
	d.mu.Unlock()
	d.wg.Add(1)

	var gotErr error
	done := make(chan struct{})
	in := inboundTo("shutdown-race", "queued before shutdown")
	in.OnDone = func(err error) { gotErr = err; close(done) }
	require.True(t, trySend(w.queue, in))

	cancel() // the gateway is already shutting down by the time runWorker starts
	go d.runWorker(w)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDone was never called for the message queued at shutdown")
	}

	assert.ErrorIs(t, gotErr, context.Canceled)
	assert.Equal(t, int32(0), atomic.LoadInt32(&ranTurn), "a message that lost the race to shutdown must never actually run")
	assert.NotContains(t, buf.String(), "ensure session failed", "a normal shutdown-time drop must not log an ERROR for it")

	waitGroupDone(t, &d.wg, time.Second)
}

// --- DeliverTo / OnDone / Timeout (cron plumbing) ---------------------------
//
// The dispatcher honors Inbound.DeliverTo/Timeout/OnDone generically - it
// has no idea cron exists. These tests pin that behavior down directly,
// since internal/cron is the one real caller and depends on it being
// correct.

// TestDispatch_DeliverTo_RoutesReplyToOverrideNotOrigin proves a turn whose
// Inbound carries DeliverTo sends its reply there, not to ChatID/ThreadID -
// exactly what a cron job needs, since its "origin" chat id is a synthetic
// job key with nobody reading it.
func TestDispatch_DeliverTo_RoutesReplyToOverrideNotOrigin(t *testing.T) {
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{Text: "the briefing"}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)

	in := channel.Inbound{
		Channel:   "cron",
		ChatID:    "job:morning-briefing",
		Text:      "summarize today",
		DeliverTo: channel.DeliverTarget{ChatID: "999888"},
	}
	d.dispatch(in)

	waitCond(t, func() bool { return len(ch.sentSnapshot()) == 1 }, 2*time.Second)
	sent := ch.sentSnapshot()
	require.Len(t, sent, 1)
	assert.Equal(t, "999888", sent[0].chatID, "reply must go to DeliverTo.ChatID, not the synthetic origin chat")
	assert.Equal(t, "the briefing", sent[0].text)
}

// TestDispatch_NoReply_WithDeliverTo_SendsNothing proves a NoReply result
// suppresses delivery even when DeliverTo is set: a cron job that decides
// there is nothing to report must stay quiet, not post an empty message to
// deliver_to.
func TestDispatch_NoReply_WithDeliverTo_SendsNothing(t *testing.T) {
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{NoReply: true}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)

	done := make(chan struct{})
	in := channel.Inbound{
		Channel:   "cron",
		ChatID:    "job:quiet-job",
		Text:      "check something",
		DeliverTo: channel.DeliverTarget{ChatID: "999888"},
		OnDone:    func(error) { close(done) },
	}
	d.dispatch(in)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDone was never called")
	}
	assert.Empty(t, ch.sentSnapshot(), "a NoReply turn must send nothing even with DeliverTo set")
}

// TestDispatch_OnDone_CalledWithTurnResultError proves OnDone receives the
// turn's own error (nil on success) - internal/cron uses this to record
// cron_runs as ok/error and clear its overlap-guard flag.
func TestDispatch_OnDone_CalledWithTurnResultError(t *testing.T) {
	boom := errors.New("boom")
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{Err: boom}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, 4)

	var gotErr error
	done := make(chan struct{})
	in := channel.Inbound{
		Channel: "cron",
		ChatID:  "job:failing",
		Text:    "do it",
		OnDone:  func(err error) { gotErr = err; close(done) },
	}
	d.dispatch(in)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDone was never called")
	}
	assert.ErrorIs(t, gotErr, boom)
}

// TestDispatch_Timeout_BoundsTurnContext proves Inbound.Timeout bounds the
// turn's own context in addition to the dispatcher's root context - a cron
// job's job.timeout applies even though the gateway itself is not
// shutting down.
func TestDispatch_Timeout_BoundsTurnContext(t *testing.T) {
	deadlineHit := make(chan struct{})
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			close(deadlineHit)
		}
		return agent.Result{NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, 4)

	in := channel.Inbound{Channel: "cron", ChatID: "job:slow", Text: "do it", Timeout: 20 * time.Millisecond}
	d.dispatch(in)

	select {
	case <-deadlineHit:
	case <-time.After(2 * time.Second):
		t.Fatal("Inbound.Timeout never bounded the turn's context")
	}
}

// TestDispatch_CronJobTimeout_DeliversTimeoutNotice proves that, unlike an
// interactive /stop or a shutdown drain (both already explained to the
// user by whoever triggered them), a cron fire's own job.timeout elapsing
// must still tell the deliver target something happened, not go silent -
// nobody else is watching a cron fire.
func TestDispatch_CronJobTimeout_DeliversTimeoutNotice(t *testing.T) {
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		<-ctx.Done()
		return agent.Result{Err: &provider.Error{Kind: provider.ErrCanceled, Err: ctx.Err()}}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)

	in := channel.Inbound{
		Channel:   "cron",
		ChatID:    "job:slow",
		Text:      "do it",
		Timeout:   20 * time.Millisecond,
		DeliverTo: channel.DeliverTarget{ChatID: "999888"},
	}
	d.dispatch(in)

	waitCond(t, func() bool { return len(ch.sentSnapshot()) == 1 }, 2*time.Second)
	sent := ch.sentSnapshot()
	require.Len(t, sent, 1)
	assert.Equal(t, "999888", sent[0].chatID)
	assert.Contains(t, sent[0].text, "timed out")
}

// TestDispatch_CronShutdownCancel_StaysSilent proves the job.timeout notice
// is not sent for a plain shutdown cancellation (context.Canceled, not
// context.DeadlineExceeded) - the existing suppression for /stop and
// shutdown drain must still hold.
func TestDispatch_CronShutdownCancel_StaysSilent(t *testing.T) {
	started := make(chan struct{})
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		close(started)
		<-ctx.Done()
		return agent.Result{Err: &provider.Error{Kind: provider.ErrCanceled, Err: ctx.Err()}}
	}}
	ch := &fakeChannel{}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)
	rootCtx, cancel := context.WithCancel(context.Background())
	d.rootCtx = rootCtx

	in := channel.Inbound{
		Channel:   "cron",
		ChatID:    "job:slow",
		Text:      "do it",
		Timeout:   time.Minute,
		DeliverTo: channel.DeliverTarget{ChatID: "999888"},
	}
	d.dispatch(in)
	<-started
	cancel()

	// Give runTurn a moment to finish and (not) reply.
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, ch.sentSnapshot(), "a shutdown/interactive cancellation must stay silent, not just a job timeout")
}

// --- /new against a message the worker has just dequeued ---------------------

// TestRunOnWorker_MessageJustDequeuedIsCanceledNotAwaited proves a control
// func (the /new reset) issued right after a message was handed to a parked
// worker cancels that turn instead of waiting out its whole run. The worker
// dequeues and registers the turn's cancel func in one critical section, so
// runOnWorker either sees the turn to cancel or sees the message still
// queued and drops it.
func TestRunOnWorker_MessageJustDequeuedIsCanceledNotAwaited(t *testing.T) {
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		if userText == "warm" {
			return agent.Result{NoReply: true}
		}
		<-ctx.Done() // a turn that only ends when canceled
		return agent.Result{Err: ctx.Err(), NoReply: true}
	}}
	d := newTestDispatcher(t, runner, &fakeChannel{}, time.Minute, 4)

	for i := 0; i < 30; i++ {
		chat := "reset-race-" + strconv.Itoa(i)
		warmed := make(chan struct{})
		warm := inboundTo(chat, "warm")
		warm.OnDone = func(error) { close(warmed) }
		d.dispatch(warm)
		<-warmed
		time.Sleep(time.Millisecond) // let the worker park in its select

		d.dispatch(inboundTo(chat, "hello"))

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ran := false
		start := time.Now()
		err := d.runOnWorker(ctx, sessionKey("telegram", chat, ""), func() { ran = true })
		cancel()

		require.NoError(t, err, "iteration %d: the reset must not wait for the uncanceled turn", i)
		assert.True(t, ran)
		assert.Less(t, time.Since(start), 500*time.Millisecond)
	}
}

// TestRunOnWorker_PendingControlBlocksIdleReap proves a worker that a
// runOnWorker caller has claimed does not reap itself before picking up the
// caller's func - otherwise the caller blocks on a worker that is gone.
func TestRunOnWorker_PendingControlBlocksIdleReap(t *testing.T) {
	d := newTestDispatcher(t, &fakeRunner{fn: func(context.Context, string, string, string, agent.Progress) agent.Result {
		return agent.Result{NoReply: true}
	}}, &fakeChannel{}, 20*time.Millisecond, 4)

	key := sessionKey("telegram", "claimed", "")
	d.mu.Lock()
	w := d.spawnWorkerLocked(key)
	w.pendingControl++
	d.mu.Unlock()

	time.Sleep(150 * time.Millisecond) // several idle timeouts
	d.mu.Lock()
	still := d.workers[key] == w
	w.pendingControl--
	d.mu.Unlock()
	assert.True(t, still, "a worker with a pending control caller must not reap itself")

	waitCond(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, ok := d.workers[key]
		return !ok
	}, time.Second)
}

// --- reply ordering ----------------------------------------------------------

// orderChannel records the order Send calls complete in, and makes sends of
// slowText take a while.
type orderChannel struct {
	fakeChannel
	slowText string
}

func (o *orderChannel) Send(ctx context.Context, chatID, threadID, text, replyTo string) error {
	if text == o.slowText {
		time.Sleep(300 * time.Millisecond)
	}
	return o.fakeChannel.Send(ctx, chatID, threadID, text, replyTo)
}

// TestDispatch_RepliesArriveInTurnOrder proves a slow reply is not overtaken
// by the next turn's faster one in the same session.
func TestDispatch_RepliesArriveInTurnOrder(t *testing.T) {
	runner := &fakeRunner{fn: func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
		return agent.Result{Text: userText}
	}}
	ch := &orderChannel{slowText: "long answer"}
	d := newTestDispatcher(t, runner, ch, time.Minute, 4)

	d.dispatch(inboundTo("ordered", "long answer"))
	d.dispatch(inboundTo("ordered", "short answer"))

	waitCond(t, func() bool { return len(ch.sentSnapshot()) == 2 }, 3*time.Second)
	sent := ch.sentSnapshot()
	assert.Equal(t, "long answer", sent[0].text)
	assert.Equal(t, "short answer", sent[1].text)
}
