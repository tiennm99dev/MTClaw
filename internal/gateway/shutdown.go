package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// drainDeadline bounds how long shutdown waits for in-flight turns to
// finish after the root context is canceled. This is a package constant,
// not a config key: nobody tunes shutdown timeouts, and the schema is
// already large enough.
const drainDeadline = 30 * time.Second

// replyDrainDeadline bounds how long shutdown additionally waits, after
// every worker has already drained, for a detached reply goroutine (see
// dispatcher.reply) to finish delivering a turn's result. It is separate
// from drainDeadline and never counted as a drain breach: reply delivery
// touches only the channel, never the store, so it cannot make closing the
// store unsafe the way a still-writing worker would.
const replyDrainDeadline = 30 * time.Second

// waitDeadline blocks until wg's counter reaches zero or deadline elapses,
// whichever happens first, and reports which one it was.
func waitDeadline(wg *sync.WaitGroup, deadline time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(deadline):
		return false
	}
}

// drain waits for ctx to end, then waits on wg (every worker goroutine) up
// to deadline, logging whichever outcome actually happened, and reports
// whether every worker finished before the deadline - Run (see gateway.go)
// uses this, with drainDeadline, to decide whether closing the store is
// safe. deadline is a parameter (rather than always drainDeadline) so
// tests can pass a short one instead of waiting out the real 30s.
func drain(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger, deadline time.Duration) bool {
	<-ctx.Done()
	log.Info("gateway: shutdown signal received; draining in-flight turns", "deadline", deadline)

	if waitDeadline(wg, deadline) {
		log.Info("gateway: all workers drained cleanly")
		return true
	}
	log.Error("gateway: drain deadline exceeded; some workers may still be running")
	return false
}

// drainReplies waits, with its own bounded deadline, for every detached
// reply goroutine dispatcher.runTurn spawned (see dispatcher.reply) to
// finish sending - so a reply produced right at shutdown gets a real
// chance to go out instead of being cut off by the process exiting the
// instant workers themselves have drained. Call this only after drain has
// already returned: reply goroutines are spawned from runTurn, which drain
// itself waits on, so waiting for replies first could still miss one
// started right at the end of drain's own wait.
func drainReplies(wg *sync.WaitGroup, log *slog.Logger, deadline time.Duration) {
	if waitDeadline(wg, deadline) {
		log.Info("gateway: all detached replies sent")
		return
	}
	log.Error("gateway: reply drain deadline exceeded; a turn's reply may not have been fully sent", "deadline", deadline)
}
