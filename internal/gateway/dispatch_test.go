package gateway

import (
	"context"
	"errors"
	"strconv"
	"sync"
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

// TestDispatch_SessionQueueFull_OnDoneCalledWithError is an H2 regression
// test: a message dropped because its session queue is already full must
// still invoke OnDone (with a non-nil error), not just skip it silently -
// for cron.Scheduler, a skipped OnDone means the job's overlap-guard flag
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

// TestDispatch_SessionQueueFull_CronInbound_NoSendAttempted is the bonus H2
// finding: a dropped cron Inbound's ChatID is a synthetic job key
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

// TestDispatch_EnsureSessionFailure_OnDoneCalledWithError is an H2
// regression test for the second drop path: Sessions().Ensure failing in
// runTurn must still invoke OnDone, not just log and return.
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

// TestDispatch_WorkerExitWithQueuedItem_OnDoneCalledForEachQueued is an H2
// regression test for the fourth drop path: a worker exiting at shutdown
// with messages still sitting in its queue must still fire OnDone for every
// one of them via drainQueueOnDone, the helper runWorker's rootCtx.Done()
// branch now calls.
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

	d.drainQueueOnDone(w)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotErrs, 3, "every message still queued when the worker exits must still get its OnDone call")
	for _, err := range gotErrs {
		assert.ErrorIs(t, err, context.Canceled)
	}
}

// TestDispatch_AfterRootCtxCanceled_OnDoneFiresImmediately is the H3
// regression test for dispatch's own early-return guard: a message
// dispatched after rootCtx is already canceled must get its OnDone call
// right away, without ever being hidden behind a spawned worker's queue.
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

// TestDispatch_RunWorker_RootCtxDoneRemovesItselfFromMap is the H3
// regression test for runWorker's rootCtx.Done() branch: a live worker
// exiting because the gateway is shutting down must remove itself from
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

// TestDispatch_ReapEnqueueRace_NoMessageIsEverDropped is the test the phase
// plan calls out by name: with the idle timeout set to ~1ms, the worker's
// decision to reap and the dispatcher's decision to enqueue race
// constantly. Every dispatched message must still eventually be processed -
// a count, not a smoke test. Run with -race (see the phase's "go test -race
// ./internal/gateway/" requirement); the shared mutex discipline in
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

// --- DeliverTo / OnDone / Timeout (phase 8's cron plumbing) -----------------
//
// Inbound.DeliverTo/Timeout/OnDone were added in phase 7 and the dispatcher
// already honors them generically - it has no idea cron exists. These
// tests pin that behavior down directly, since phase 8 (internal/cron) is
// the first real caller and depends on it being correct.

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
		DeliverTo: channel.DeliverTarget{Channel: "telegram", ChatID: "999888"},
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
		DeliverTo: channel.DeliverTarget{Channel: "telegram", ChatID: "999888"},
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

// TestDispatch_CronJobTimeout_DeliversTimeoutNotice is the M8 regression
// test: unlike an interactive /stop or a shutdown drain (both already
// explained to the user by whoever triggered them), nobody is watching a
// cron fire - its own job.timeout elapsing must still tell the deliver
// target something happened, not go silent.
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
		DeliverTo: channel.DeliverTarget{Channel: "telegram", ChatID: "999888"},
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
		DeliverTo: channel.DeliverTarget{Channel: "telegram", ChatID: "999888"},
	}
	d.dispatch(in)
	<-started
	cancel()

	// Give runTurn a moment to finish and (not) reply.
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, ch.sentSnapshot(), "a shutdown/interactive cancellation must stay silent, not just a job timeout")
}
