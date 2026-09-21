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

// waitForShutdown blocks until ctx is done, then waits on wg (every worker
// goroutine) with drainDeadline, logging whichever outcome actually
// happened, and reports whether every worker finished before the deadline -
// the caller uses this to decide whether closing the store is safe (see
// gateway.go's Run). It is the production entry point; drain (below) is the
// deadline-parameterized implementation tests use directly with a short
// deadline instead of waiting out the real 30s.
func waitForShutdown(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger) bool {
	return drain(ctx, wg, log, drainDeadline)
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
		log.Error("gateway: drain deadline exceeded; some workers may still be running")
		return false
	}
}
