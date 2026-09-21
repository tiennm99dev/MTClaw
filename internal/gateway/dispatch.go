// Package gateway wires the long-running mtclaw gateway process: it takes
// inbound messages off one channel, serializes them per chat session while
// running different sessions concurrently, bounds total concurrent turns,
// and drains cleanly on shutdown. See
// plans/260731-2219-mtclaw-core-system/phase-07-gateway-orchestration.md for
// the full design rationale, in particular the worker reap/enqueue race
// this package's dispatch/runWorker pair is built to avoid.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store"
)

// idleSessionTimeout is how long a session worker waits for a new message
// before reaping itself. A public-ish bot accumulates chats forever
// otherwise; 5 minutes is generous enough that a normal back-and-forth
// never triggers a respawn mid-conversation.
const idleSessionTimeout = 5 * time.Minute

// globalConcurrency caps turns running at once across the whole process,
// independent of how many sessions are active. This is a cost and
// blast-radius control (token spend, concurrent exec children), not a
// performance knob.
const globalConcurrency = 4

// sessionBusyReply is sent once, and the triggering message dropped, when a
// session's own queue is already full: an honest "still working" answer
// instead of silence, which would look indistinguishable from the bot being
// broken.
const sessionBusyReply = "still working on the previous message, try again shortly"

// turnErrorReply is sent when a turn ends with an error that produced no
// usable text at all, so the user is not left with silence.
const turnErrorReply = "sorry, something went wrong handling that message."

// errSessionQueueFull is the error OnDone receives when a message is
// dropped because its session's own queue is already full - the
// dispatcher's own overflow policy, not an infrastructure fault.
var errSessionQueueFull = errors.New("gateway: session queue full")

// turnRunner is the minimal surface the dispatcher needs from an agent
// loop. Defining it locally (rather than depending on *agent.Loop directly)
// lets tests substitute a function-backed fake with no provider, store
// wiring, or tool registry at all. *agent.Loop satisfies it.
type turnRunner interface {
	Run(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result
}

// Channel is the narrow surface the dispatcher and its progress reporter
// need from a chat channel: deliver a turn's reply, and refresh a typing
// indicator while a turn runs. internal/channel/telegram.Channel satisfies
// it; tests substitute a fake with no network.
type Channel interface {
	Send(ctx context.Context, chatID, threadID, text, replyTo string) error
	SendTyping(ctx context.Context, chatID, threadID string) error
}

// worker owns one session's serialized turn queue. Exactly one worker
// goroutine exists per active session key at a time; runWorker is that
// goroutine's body.
type worker struct {
	key   string
	queue chan channel.Inbound

	// closing is set, under dispatcher.mu, by the worker itself right
	// before it deletes itself from dispatcher.workers and returns. The
	// dispatcher checks it (also under dispatcher.mu) to tell an about-to-
	// exit worker apart from a live one - see dispatch's doc comment for
	// why this must be one mutex, not two independent decisions.
	closing bool
	// cancel is the in-flight turn's CancelFunc, non-nil only while a turn
	// is actually running. /stop reads and calls it under dispatcher.mu.
	cancel context.CancelFunc
}

// dispatcher owns the worker map, the cancellation registry (folded into
// each worker's cancel field), and the global concurrency semaphore. It has
// no idea a "gateway" or a "channel" concept beyond the small Channel
// interface it depends on, and no idea cron (phase 8) exists - Inbound's
// DeliverTo/Timeout/OnDone fields are honored generically.
type dispatcher struct {
	store   store.Store
	channel Channel
	runner  turnRunner
	log     *slog.Logger

	idleTimeout time.Duration
	sem         chan struct{} // buffered counting semaphore, capacity = concurrency

	mu      sync.Mutex
	workers map[string]*worker
	// closed is set true, under mu, by closeForShutdown once rootCtx ends.
	// dispatch checks it in the same critical section it spawns a worker
	// from, so a message arriving after every worker has already exited and
	// drain's wg.Wait() has begun unblocking can never spawn a fresh worker
	// and race wg.Add against that already-unblocking Wait - a race
	// sync.WaitGroup explicitly forbids (see dispatch's own doc comment).
	closed bool

	wg sync.WaitGroup // tracks every worker goroutine, for shutdown drain

	// rootCtx is the context every turn's own context is derived from,
	// so canceling it (shutdown) cancels every in-flight turn too. It
	// defaults to context.Background() so a dispatcher built directly in
	// tests works without a Gateway.Run wrapping it.
	rootCtx context.Context
}

// newDispatcher builds a dispatcher. idleTimeout <= 0 defaults to
// idleSessionTimeout; concurrency <= 0 defaults to globalConcurrency. ch and
// runner may be nil at construction and assigned afterward (Gateway.New
// needs a dispatcher to exist, for its cancel registry, before the channel
// and the agent loop it will hold are themselves built).
func newDispatcher(st store.Store, ch Channel, runner turnRunner, log *slog.Logger, idleTimeout time.Duration, concurrency int) *dispatcher {
	if log == nil {
		log = slog.Default()
	}
	if idleTimeout <= 0 {
		idleTimeout = idleSessionTimeout
	}
	if concurrency <= 0 {
		concurrency = globalConcurrency
	}
	return &dispatcher{
		store:       st,
		channel:     ch,
		runner:      runner,
		log:         log,
		idleTimeout: idleTimeout,
		sem:         make(chan struct{}, concurrency),
		workers:     make(map[string]*worker),
		rootCtx:     context.Background(),
	}
}

// sessionKey identifies the one worker/queue an inbound message serializes
// against: two concurrent turns in the same chat interleave tool side
// effects and produce conflicting history writes, so channel+chat+thread is
// the unit of serialization; different chats have no shared state and run
// freely in parallel.
func sessionKey(channelName, chatID, threadID string) string {
	return channelName + ":" + chatID + ":" + threadID
}

// pump reads every message off in and dispatches it until in closes or ctx
// is done. It is the dispatcher's only reader of the global inbound queue.
func (d *dispatcher) pump(ctx context.Context, in <-chan channel.Inbound) {
	for {
		select {
		case msg, ok := <-in:
			if !ok {
				return
			}
			d.dispatch(msg)
		case <-ctx.Done():
			return
		}
	}
}

// dispatch routes one inbound message to its session's worker, spawning one
// if none exists (or the existing one is mid-exit).
//
// The reap/enqueue race this method and runWorker are built to avoid: a
// naive dispatcher would take the lock, look up the worker, release the
// lock, then send to worker.queue. A worker whose idle timer fires in that
// gap exits and the message lands in a channel with no receiver - silent
// message loss indistinguishable from the bot being broken. The fix is that
// the worker's decision to exit and this method's enqueue are serialized by
// the *same* mutex: dispatch holds d.mu across both the lookup and the
// non-blocking send (safe - a buffered non-blocking send never blocks), and
// runWorker's idle-fire branch takes d.mu, re-checks its queue is empty,
// sets closing, deletes itself from the map, and only then releases and
// returns. Whichever side gets the lock first wins outright: if dispatch
// wins, the message is enqueued to a worker that has not yet decided to
// exit (its own subsequent re-check under the lock will see the queue is
// non-empty and abort the reap); if the worker wins, it is already gone from
// the map by the time dispatch gets the lock, so `closing`/`!ok` is treated
// as a miss and a fresh worker is spawned instead.
func (d *dispatcher) dispatch(in channel.Inbound) {
	in = wrapOnDone(in)
	if err := d.rootCtx.Err(); err != nil {
		// Fast path: shutdown already canceled rootCtx by the time this
		// unsynchronized read happened, so there is nothing to gain by
		// spawning a worker just to drop it - fail the message now. This
		// check alone is not the correctness guarantee against spawning
		// after drain's wg.Wait() has begun unblocking (it races
		// closeForShutdown); the d.closed check below, taken under the same
		// lock as the spawn decision, is.
		callOnDone(in, err)
		return
	}
	key := sessionKey(in.Channel, in.ChatID, in.ThreadID)

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		callOnDone(in, d.rootCtx.Err())
		return
	}
	w, ok := d.workers[key]
	if !ok || w.closing {
		w = d.spawnWorkerLocked(key)
	}
	sent := trySend(w.queue, in)
	d.mu.Unlock()

	if !sent {
		callOnDone(in, errSessionQueueFull)
		d.replySessionBusy(in)
	}
}

// closeForShutdown marks the dispatcher closed under d.mu. Callers must
// invoke this once, as soon as rootCtx ends and before waiting on d.wg (see
// Gateway.Run), so dispatch's spawn decision can observe it in time to
// close the wg.Add/wg.Wait race described on the closed field itself.
func (d *dispatcher) closeForShutdown() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
}

// wrapOnDone returns a copy of in whose OnDone callback, if set, is guarded
// to fire at most once no matter which terminal path of this message ends
// up invoking it. One Inbound value can reach a terminal state several
// different ways - the session-queue-full drop right below, an Ensure
// failure or a shutdown mid-wait in runTurn, a shutdown mid-queue in
// runWorker, or ordinary completion - and every one of those needs to be
// able to call OnDone without knowing whether another path already did.
func wrapOnDone(in channel.Inbound) channel.Inbound {
	if in.OnDone == nil {
		return in
	}
	var once sync.Once
	orig := in.OnDone
	in.OnDone = func(err error) {
		once.Do(func() { orig(err) })
	}
	return in
}

// callOnDone invokes in.OnDone, if set, with err. Safe to call from more
// than one terminal path for the same message: wrapOnDone (applied once, in
// dispatch) guarantees only the first call actually reaches the caller's
// callback.
func callOnDone(in channel.Inbound, err error) {
	if in.OnDone != nil {
		in.OnDone(err)
	}
}

// spawnWorkerLocked creates and starts a new worker for key. Callers must
// already hold d.mu.
func (d *dispatcher) spawnWorkerLocked(key string) *worker {
	w := &worker{key: key, queue: make(chan channel.Inbound, sessionQueueSize)}
	d.workers[key] = w
	d.wg.Add(1)
	go d.runWorker(w)
	return w
}

// runWorker is one session worker's whole lifetime: pull a message, run its
// turn, repeat; reap itself after idleTimeout with nothing queued.
func (d *dispatcher) runWorker(w *worker) {
	defer d.wg.Done()

	timer := time.NewTimer(d.idleTimeout)
	defer timer.Stop()

	for {
		select {
		case in, ok := <-w.queue:
			if !ok {
				return
			}
			drainTimer(timer)
			d.runTurn(w, in)
			timer.Reset(d.idleTimeout)

		case <-timer.C:
			d.mu.Lock()
			if len(w.queue) > 0 {
				// A message landed between the timer firing and this
				// goroutine winning the lock (select can pick a fired timer
				// over a ready channel receive even when both are ready).
				// Do not reap; go back around and let the message case run.
				d.mu.Unlock()
				timer.Reset(d.idleTimeout)
				continue
			}
			w.closing = true
			delete(d.workers, w.key)
			d.mu.Unlock()
			return

		case <-d.rootCtx.Done():
			// Mirror the idle-reap branch's bookkeeping: mark closing and
			// remove this worker from the map under d.mu before draining,
			// so a dispatch racing this exit sees `closing`/`!ok` (a miss)
			// and spawns a fresh worker instead of enqueuing into a queue
			// nothing will ever drain. The separate wg.Add/wg.Wait race -
			// dispatch spawning a brand new worker after every existing one
			// has already exited and drain's wg.Wait() has begun unblocking
			// - is closed by dispatch's own d.closed check under d.mu, not
			// by this branch; see closeForShutdown.
			d.mu.Lock()
			w.closing = true
			delete(d.workers, w.key)
			d.mu.Unlock()
			d.drainQueueOnDone(w)
			return
		}
	}
}

// drainQueueOnDone fires OnDone (if set) for every message still sitting in
// w.queue when the worker exits at shutdown without ever processing them -
// otherwise those Inbounds vanish silently with their caller's OnDone never
// invoked, which for a cron job means its overlap-guard flag never clears.
func (d *dispatcher) drainQueueOnDone(w *worker) {
	for {
		select {
		case in := <-w.queue:
			callOnDone(in, d.rootCtx.Err())
		default:
			return
		}
	}
}

// drainTimer stops t and drains any value already sent, so a subsequent
// Reset starts clean regardless of whether t had already fired.
func drainTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// cancelSession cancels key's in-flight turn, if any, reporting whether
// there actually was one running - the /stop command's reply depends on
// this distinction ("cancelling the current turn" vs "nothing running").
func (d *dispatcher) cancelSession(key string) bool {
	d.mu.Lock()
	w, ok := d.workers[key]
	var cancel context.CancelFunc
	if ok {
		cancel = w.cancel
	}
	d.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// runTurn acquires the global concurrency slot, runs one turn end to end,
// and delivers the reply. It never returns an error itself: every failure
// mode (session lookup, the turn, the send) is logged and, where it affects
// the user, turned into a chat reply instead.
func (d *dispatcher) runTurn(w *worker, in channel.Inbound) {
	select {
	case d.sem <- struct{}{}:
	case <-d.rootCtx.Done():
		callOnDone(in, d.rootCtx.Err())
		return
	}
	defer func() { <-d.sem }()

	// Bounded, not context.Background(): this runs after the semaphore is
	// already taken and before any cancelable turn context exists, so an
	// unbounded call here would pin a global concurrency slot indefinitely
	// (and ignore /stop) if a write ever blocks - a manual `mtclaw cron
	// run`, a backup holding the database's write lock.
	ensureCtx, ensureCancel := context.WithTimeout(d.rootCtx, 10*time.Second)
	sess, err := d.store.Sessions().Ensure(ensureCtx, in.Channel, in.ChatID, in.ThreadID)
	ensureCancel()
	if err != nil {
		d.log.Error("gateway: ensure session failed", "channel", in.Channel, "chat_id", in.ChatID, "error", err)
		callOnDone(in, fmt.Errorf("gateway: ensure session: %w", err))
		return
	}

	var turnCtx context.Context
	var cancel context.CancelFunc
	if in.Timeout > 0 {
		turnCtx, cancel = context.WithTimeout(d.rootCtx, in.Timeout)
	} else {
		turnCtx, cancel = context.WithCancel(d.rootCtx)
	}

	d.mu.Lock()
	w.cancel = cancel
	d.mu.Unlock()

	deliverChatID, deliverThreadID := in.ChatID, in.ThreadID
	if in.DeliverTo.ChatID != "" {
		deliverChatID, deliverThreadID = in.DeliverTo.ChatID, in.DeliverTo.ThreadID
	}

	var onProgress agent.Progress
	var progress *progressReporter
	if d.channel != nil {
		progress = newProgressReporter(turnCtx, d.channel, deliverChatID, deliverThreadID, d.log)
		progress.start()
		onProgress = progress.onEvent
	}

	result := d.runner.Run(turnCtx, sess.ID, in.Text, in.MessageID, onProgress)

	if progress != nil {
		progress.stop()
	}
	cancel()

	d.mu.Lock()
	w.cancel = nil
	d.mu.Unlock()

	callOnDone(in, result.Err)

	d.reply(in, deliverChatID, deliverThreadID, result)
}

// reply delivers a turn's result, applying the same NoReply/error rules
// regardless of whether the turn's origin (in) and its delivery target
// (chatID/threadID, possibly overridden by in.DeliverTo) are the same chat.
func (d *dispatcher) reply(in channel.Inbound, chatID, threadID string, result agent.Result) {
	if d.channel == nil || result.NoReply {
		return
	}

	text := result.Text
	if result.Err != nil {
		var perr *provider.Error
		if errors.As(result.Err, &perr) && perr.Kind == provider.ErrCanceled {
			if in.Channel == "cron" && in.Timeout > 0 && errors.Is(result.Err, context.DeadlineExceeded) {
				// Unlike an interactive /stop or a shutdown drain - both
				// already explained by whoever triggered the cancellation -
				// nobody is watching a cron fire: its own job.timeout
				// elapsing must still tell the deliver target something
				// happened, not go silent with only `mtclaw cron runs`
				// ever showing the failure.
				text = fmt.Sprintf("scheduled job timed out after %s", in.Timeout)
			} else {
				return
			}
		} else {
			d.log.Error("gateway: turn completed with an error", "channel", in.Channel, "chat_id", in.ChatID, "error", result.Err)
			if text == "" {
				text = turnErrorReply
			}
		}
	}
	if text == "" {
		return
	}

	replyTo := ""
	if chatID == in.ChatID && threadID == in.ThreadID {
		replyTo = in.MessageID
	}

	// Deliver on a context detached from rootCtx (which may already be
	// canceled if this turn finished during shutdown drain) but still
	// bounded, so a reply produced right at shutdown gets a real chance to
	// go out instead of failing instantly on an already-done context. The
	// budget scales with how many chunks the channel is likely to split
	// text into: a fixed one-chunk budget truncates a long multi-chunk
	// answer mid-send once inter-chunk delays and any 429 wait are added up.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(d.rootCtx), replyTimeout(text))
	defer cancel()
	if err := d.channel.Send(sendCtx, chatID, threadID, text, replyTo); err != nil {
		d.log.Error("gateway: send turn reply failed", "chat_id", chatID, "error", err)
	}
}

// replyChunkLimit mirrors telegram.DefaultChunkLimit purely to size
// replyTimeout's chunk-count estimate; the dispatcher must not import the
// concrete telegram package (tests substitute fakeChannel, which has no
// concept of chunking), so this is a deliberately duplicated constant, not a
// shared one.
const replyChunkLimit = 4096

// baseReplyTimeout covers one chunk's send plus headroom for one 429 wait or
// transient retry; perChunkReplyTimeout is added per additional chunk, for
// its own send plus inter-chunk delay plus the same headroom.
const (
	baseReplyTimeout     = 10 * time.Second
	perChunkReplyTimeout = 5 * time.Second
)

// replyTimeout scales the reply budget with how many chunks text is likely
// to be split into.
func replyTimeout(text string) time.Duration {
	chunks := len(text)/replyChunkLimit + 1
	return baseReplyTimeout + time.Duration(chunks-1)*perChunkReplyTimeout
}

// replySessionBusy sends the one-time overflow reply when a session's queue
// is already full, on a short bounded context so a slow or dead channel
// cannot stall the dispatcher itself.
func (d *dispatcher) replySessionBusy(in channel.Inbound) {
	if d.channel == nil || in.Channel == "cron" {
		// A cron inbound's ChatID is a synthetic job key ("job:<name>"), not
		// a real chat - there is nothing to send to; callOnDone above is the
		// only signal a dropped cron fire needs to give the scheduler.
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.rootCtx), 10*time.Second)
	defer cancel()
	if err := d.channel.Send(ctx, in.ChatID, in.ThreadID, sessionBusyReply, in.MessageID); err != nil {
		d.log.Error("gateway: send session-busy reply failed", "chat_id", in.ChatID, "error", err)
	}
}
