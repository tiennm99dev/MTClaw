package gateway

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// drainDeadline bounds how long shutdown waits for in-flight turns to
// finish after the root context is canceled. This is a package constant,
// not a config key: nobody tunes shutdown timeouts, and the schema is
// already large enough.
const drainDeadline = 30 * time.Second

// notifyContext wraps parent in a context canceled by either parent itself
// ending or a SIGINT/SIGTERM arriving, so a turn context derived from the
// result is canceled by both a caller-driven shutdown (tests) and a real OS
// signal (production). On Windows, SIGTERM is never actually delivered by
// the OS - registering it is harmless, and Ctrl-C (os.Interrupt) is what a
// user there actually gets.
func notifyContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// waitForShutdown blocks until ctx is done, then waits on wg (every worker
// goroutine) with drainDeadline, logging whichever outcome actually
// happened. It is the production entry point; drain (below) is the
// deadline-parameterized implementation tests use directly with a short
// deadline instead of waiting out the real 30s.
func waitForShutdown(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger) {
	drain(ctx, wg, log, drainDeadline)
}

// drain waits for ctx to end, then waits on wg up to deadline, returning
// whether every worker finished before the deadline. Split out from
// waitForShutdown so tests can pass a short deadline instead of the real
// production constant.
func drain(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger, deadline time.Duration) bool {
	<-ctx.Done()
	log.Info("gateway: shutdown signal received; draining in-flight turns", "deadline", deadline)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Info("gateway: all workers drained cleanly")
		return true
	case <-time.After(deadline):
		log.Warn("gateway: drain deadline exceeded; some workers may still be running")
		return false
	}
}
