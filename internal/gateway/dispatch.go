// Package gateway wires the long-running mtclaw gateway process: it takes
// inbound messages off one channel, serializes them per chat session while
// running different sessions concurrently, bounds total concurrent turns,
// and drains cleanly on shutdown. See dispatch's own doc comment for the
// worker reap/enqueue race this package's dispatch/runWorker pair is built
// to avoid.
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

// sessionBusyReply is sent once per overflow burst, and the triggering
// message dropped, when a session's own queue is already full: an honest
// "still working" answer instead of silence, which would look
// indistinguishable from the bot being broken.
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
	// control carries a func to run on this worker's own goroutine, between
	// turns - see dispatcher.runOnWorker. Unbuffered: the sender blocks
	// until runWorker actually picks it up, which is how the caller knows
	// fn will not race a turn already in flight.
	control chan func()

	// cancel is the in-flight turn's CancelFunc, non-nil from the moment the
	// worker dequeues a message until its turn returns. It is set in the
	// same dispatcher.mu critical section as the dequeue, so a /stop or /new
	// that runs under the lock either sees the turn or sees the message
	// still queued - never a message already taken but not yet registered.
	cancel context.CancelFunc
	// pendingControl counts runOnWorker callers that have claimed this
	// worker but whose fn the worker has not started yet. Guarded by
	// dispatcher.mu. While it is above zero the worker drops (rather than
	// runs) any message it dequeues, and refuses to reap itself, so fn can
	// neither be preceded by a turn nor sent to a worker that has exited.
	pendingControl int
	// lastReply is closed once the reply goroutine spawned for this
	// worker's most recent turn has finished; the next reply waits on it so
	// replies reach the chat in turn order. Touched only by the worker
	// goroutine.
	lastReply chan struct{}
	// busyNotified is set under dispatcher.mu the first time this worker's
	// queue overflows, and cleared the next time it actually dequeues a
	// message - so a burst of overflowing messages gets exactly one "still
	// working" reply instead of one per dropped message, and a later,
	// separate burst still gets its own notice.
	busyNotified bool
}

// dispatcher owns the worker map, the cancellation registry (folded into
// each worker's cancel field), and the global concurrency semaphore. It has
// no idea a "gateway" or a "channel" concept beyond the small Channel
// interface it depends on, and no idea cron exists - Inbound's
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

	// replyWG tracks every detached reply goroutine runTurn spawns (see
	// reply below), separately from wg: a reply is intentionally not part
	// of wg (so a slow multi-chunk send never looks like a drain-deadline
	// breach), but shutdown must still give it a bounded chance to finish
	// before the process exits - see drainReplies in shutdown.go.
	replyWG sync.WaitGroup

	// rootCtx is the context every turn's own context is derived from, so
	// canceling it (shutdown) cancels every in-flight turn too. It defaults
	// to context.Background() so a dispatcher built directly in tests works
	// without start (below) wrapping it.
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

// start gives the dispatcher's shutdown transition a single entry point:
// it sets rootCtx and arranges for closeForShutdown to run exactly once
// rootCtx ends, so a caller (Gateway.Run) never has to hand-write that
// goroutine itself.
func (d *dispatcher) start(ctx context.Context) {
	d.rootCtx = ctx
	context.AfterFunc(ctx, d.closeForShutdown)
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
// is done. It is the dispatcher's only reader of the inbound queue.
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
// deletes itself from the map, and only then releases and returns. Whichever side gets the lock first wins outright: if dispatch
// wins, the message is enqueued to a worker that has not yet decided to
// exit (its own subsequent re-check under the lock will see the queue is
// non-empty and abort the reap); if the worker wins, it is already gone from
// the map by the time dispatch gets the lock, so `!ok` is treated as a miss
// and a fresh worker is spawned instead.
func (d *dispatcher) dispatch(in channel.Inbound) {
	in = wrapOnDone(in)
	key := sessionKey(in.Channel, in.ChatID, in.ThreadID)

	d.mu.Lock()
	if d.closed || d.rootCtx.Err() != nil {
		d.mu.Unlock()
		callOnDone(in, d.rootCtx.Err())
		return
	}
	w, ok := d.workers[key]
	if !ok {
		w = d.spawnWorkerLocked(key)
	}
	sent := trySend(w.queue, in)
	notify := false
	if !sent && !w.busyNotified {
		w.busyNotified = true
		notify = true
	}
	d.mu.Unlock()

	if !sent {
		callOnDone(in, errSessionQueueFull)
		if notify {
			d.replySessionBusyAsync(in)
		}
	}
}

// closeForShutdown marks the dispatcher closed under d.mu. Called
// automatically once rootCtx ends (see start); dispatch's spawn decision
// checks this under the same lock so a message arriving after shutdown has
// begun can never spawn a worker doomed to be dropped.
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
	w := &worker{key: key, queue: make(chan channel.Inbound, sessionQueueSize), control: make(chan func())}
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
		// A pending control func (runOnWorker - today, only /new's Reset)
		// must win over an already-queued message: Go's select picks
		// pseudo-randomly among every ready case below, and a queued
		// message winning that pick would run a whole turn - including a
		// full approval wait - against history /new is about to delete,
		// while also blocking the update pump's synchronous Reset call for
		// as long as that turn takes. Checking control first, non-blocking,
		// makes that pick deterministic instead of a coin flip.
		select {
		case fn := <-w.control:
			fn()
			continue
		default:
		}

		select {
		case in, ok := <-w.queue:
			if !ok {
				return
			}
			// Dequeue and cancel registration are one critical section: a
			// /stop or /new taking d.mu after this point sees the turn's
			// cancel func, and one taking it before either cancelled the
			// worker's previous turn or set pendingControl, in which case
			// this message is dropped instead of run.
			d.mu.Lock()
			w.busyNotified = false
			var dropErr error
			var turnCtx context.Context
			var cancel context.CancelFunc
			switch {
			case d.rootCtx.Err() != nil:
				// select can pick a ready queue receive over a ready
				// rootCtx.Done() case at random (Go's own select semantics),
				// so a message already queued when shutdown begins can reach
				// here even though the gateway is already shutting down.
				// Running it would only produce a canceled-context error out
				// of Ensure; report the same terminal signal
				// drainQueueOnDone gives a never-dequeued message, with no
				// log noise for what is normal shutdown behavior.
				dropErr = d.rootCtx.Err()
			case w.pendingControl > 0:
				// A /new is waiting for this worker: this message would run
				// against history it is about to delete.
				dropErr = context.Canceled
			default:
				turnCtx, cancel = d.newTurnContext(in)
				w.cancel = cancel
			}
			d.mu.Unlock()
			if dropErr != nil {
				callOnDone(in, dropErr)
				continue
			}

			drainTimer(timer)
			d.runTurn(w, in, turnCtx, cancel)
			timer.Reset(d.idleTimeout)

		case fn := <-w.control:
			fn()

		case <-timer.C:
			d.mu.Lock()
			if len(w.queue) > 0 || w.pendingControl > 0 {
				// A message landed (or a runOnWorker caller claimed this
				// worker) between the timer firing and this goroutine
				// winning the lock (select can pick a fired timer over a
				// ready channel receive even when both are ready). Do not
				// reap; go back around and let that case run.
				d.mu.Unlock()
				timer.Reset(d.idleTimeout)
				continue
			}
			delete(d.workers, w.key)
			d.mu.Unlock()
			return

		case <-d.rootCtx.Done():
			// Mirror the idle-reap branch's bookkeeping: remove this worker
			// from the map under d.mu before draining, so a dispatch racing
			// this exit sees `!ok` (a miss) and spawns a fresh worker
			// instead of enqueuing into a queue
			// nothing will ever drain. The separate wg.Add/wg.Wait race -
			// dispatch spawning a brand new worker after every existing one
			// has already exited and drain's wg.Wait() has begun unblocking
			// - is closed by dispatch's own d.closed check under d.mu, not
			// by this branch; see closeForShutdown.
			d.mu.Lock()
			delete(d.workers, w.key)
			d.mu.Unlock()
			d.drainQueueOnDone(w, d.rootCtx.Err())
			return
		}
	}
}

// drainQueueOnDone fires OnDone (if set, with err) for every message still
// sitting in w.queue, without ever processing them - otherwise those
// Inbounds vanish silently with their caller's OnDone never invoked, which
// for a cron job means its overlap-guard flag never clears. Used both when
// a worker exits at shutdown (err is d.rootCtx.Err()) and by runOnWorker
// (err is context.Canceled), which drops anything queued behind the turn
// it just canceled before running its own fn - a queued message the worker
// had not started yet is otherwise left to run against history /new is
// about to delete.
func (d *dispatcher) drainQueueOnDone(w *worker, err error) {
	for {
		select {
		case in := <-w.queue:
			callOnDone(in, err)
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

// runOnWorker cancels key's in-flight turn, if any, drops any message
// already queued behind it, then runs fn on key's own worker goroutine and
// blocks until fn returns (or ctx ends) - spawning a worker for key if none
// exists yet. Because fn runs on the same goroutine runTurn does, and only
// after any turn already in flight has actually returned (runWorker's
// select loop always checks its control case before its message case - see
// runWorker), fn is guaranteed to run strictly after that turn's own
// end-of-turn history flush, and before any message that had been waiting
// behind it - not racing either one. /new uses this to delete a session's
// history only once a turn cancellation has actually taken effect, and
// without a queued message it dropped in the meantime going on to run
// against the history it just deleted.
func (d *dispatcher) runOnWorker(ctx context.Context, key string, fn func()) error {
	d.mu.Lock()
	if d.closed || d.rootCtx.Err() != nil {
		d.mu.Unlock()
		return d.rootCtx.Err()
	}
	w, ok := d.workers[key]
	if !ok {
		w = d.spawnWorkerLocked(key)
	}
	// Claim the worker and cancel its turn in the same critical section as
	// the lookup: the worker cannot reap itself, and cannot dequeue a
	// message it would run, between here and picking fn up.
	w.pendingControl++
	if w.cancel != nil {
		w.cancel()
	}
	d.mu.Unlock()

	// release drops the claim exactly once, whether the worker picked fn up
	// or this call gave up first.
	var released bool
	release := func() {
		d.mu.Lock()
		if !released {
			released = true
			w.pendingControl--
		}
		d.mu.Unlock()
	}
	defer release()

	done := make(chan struct{})
	wrapped := func() {
		defer close(done)
		// The worker is busy running fn from here on, so it can dequeue
		// nothing else until fn returns.
		release()
		d.drainQueueOnDone(w, context.Canceled)
		fn()
	}
	select {
	case w.control <- wrapped:
	case <-ctx.Done():
		return ctx.Err()
	case <-d.rootCtx.Done():
		return d.rootCtx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newTurnContext derives a turn's context from rootCtx, bounded by
// in.Timeout when set.
func (d *dispatcher) newTurnContext(in channel.Inbound) (context.Context, context.CancelFunc) {
	if in.Timeout > 0 {
		return context.WithTimeout(d.rootCtx, in.Timeout)
	}
	return context.WithCancel(d.rootCtx)
}

// runTurn runs one turn end to end and delivers the reply. It never returns
// an error itself: every failure mode (session lookup, the turn, the send)
// is logged and, where it affects the user, turned into a chat reply
// instead.
//
// turnCtx and cancel come from newTurnContext, and w.cancel was already set
// to cancel by runWorker in the same critical section that dequeued in - so
// /stop finds the turn even while it is still queued behind the global
// concurrency cap below.
func (d *dispatcher) runTurn(w *worker, in channel.Inbound, turnCtx context.Context, cancel context.CancelFunc) {
	defer func() {
		cancel()
		d.mu.Lock()
		w.cancel = nil
		d.mu.Unlock()
	}()

	select {
	case d.sem <- struct{}{}:
	case <-turnCtx.Done():
		callOnDone(in, turnCtx.Err())
		return
	}
	defer func() { <-d.sem }()

	// Bounded, not context.Background() or d.rootCtx directly: an unbounded
	// call here would pin a global concurrency slot indefinitely (and
	// ignore /stop, since turnCtx already covers that) if a write ever
	// blocks - a manual `mtclaw cron run`, a backup holding the database's
	// write lock.
	ensureCtx, ensureCancel := context.WithTimeout(turnCtx, 10*time.Second)
	sess, err := d.store.Sessions().Ensure(ensureCtx, in.Channel, in.ChatID, in.ThreadID)
	ensureCancel()
	if err != nil {
		d.log.Error("gateway: ensure session failed", "channel", in.Channel, "chat_id", in.ChatID, "error", err)
		callOnDone(in, fmt.Errorf("gateway: ensure session: %w", err))
		return
	}

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

	callOnDone(in, result.Err)

	// Detached from this call (and so from d.wg, which drain waits on):
	// delivery touches no store, only the channel, so a slow multi-chunk
	// send must never make an otherwise-clean shutdown look like a
	// drain-deadline breach just because Telegram is still receiving the
	// last chunk of a long reply. Tracked on d.replyWG instead, which
	// drainReplies gives its own bounded wait after wg itself has drained -
	// otherwise a reply produced right at shutdown is dropped the instant
	// Run returns, not merely delayed past the drain deadline.
	//
	// Chained per worker instead: reply N+1 waits for reply N to finish, so
	// a long multi-chunk answer is never overtaken by the next turn's
	// shorter one.
	prev := w.lastReply
	done := make(chan struct{})
	w.lastReply = done
	d.replyWG.Add(1)
	go d.reply(prev, done, in, deliverChatID, deliverThreadID, result)
}

// reply delivers a turn's result, applying the same NoReply/error rules
// regardless of whether the turn's origin (in) and its delivery target
// (chatID/threadID, possibly overridden by in.DeliverTo) are the same chat.
//
// It first waits for prev (the session's previous reply, nil for the first)
// and closes done when finished, whichever way it returns. The wait is
// unconditional because every reply is itself bounded by its own send
// timeout, so the chain always unwinds.
func (d *dispatcher) reply(prev <-chan struct{}, done chan<- struct{}, in channel.Inbound, chatID, threadID string, result agent.Result) {
	defer d.replyWG.Done()
	defer close(done)
	if prev != nil {
		<-prev
	}
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

// replySessionBusyAsync sends the overflow-burst reply on its own goroutine
// (not the pump goroutine dispatch runs on, and not tracked by d.wg) so a
// slow or dead channel cannot stall dispatch itself; dispatch's own
// busyNotified bookkeeping is what keeps this to one send per overflow
// burst rather than one per dropped message.
func (d *dispatcher) replySessionBusyAsync(in channel.Inbound) {
	if d.channel == nil || in.Channel == "cron" {
		// A cron inbound's ChatID is a synthetic job key ("job:<name>"), not
		// a real chat - there is nothing to send to; callOnDone above is the
		// only signal a dropped cron fire needs to give the scheduler.
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(d.rootCtx), 10*time.Second)
		defer cancel()
		if err := d.channel.Send(ctx, in.ChatID, in.ThreadID, sessionBusyReply, in.MessageID); err != nil {
			d.log.Error("gateway: send session-busy reply failed", "chat_id", in.ChatID, "error", err)
		}
	}()
}
