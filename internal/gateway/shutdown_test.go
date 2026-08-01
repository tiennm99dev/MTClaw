package gateway

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDrain_CompletesWhenWorkersFinishBeforeDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond) // a short "turn" that finishes fast
	}()

	cancel() // simulate the shutdown signal
	completed := drain(ctx, &wg, testLog(), 500*time.Millisecond)
	assert.True(t, completed, "drain must report success once every worker goroutine returns")
}

func TestDrain_ReturnsAtDeadlineWhenWorkersOutlastIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(500 * time.Millisecond) // longer than the deadline below
	}()

	cancel()
	start := time.Now()
	completed := drain(ctx, &wg, testLog(), 50*time.Millisecond)
	elapsed := time.Since(start)

	assert.False(t, completed, "drain must report the deadline was exceeded")
	assert.Less(t, elapsed, 200*time.Millisecond, "drain must return at the deadline, not wait for the goroutine")
}

func TestDrain_WaitsForCtxBeforeChecking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	done := make(chan struct{})
	go func() {
		drain(ctx, &wg, testLog(), time.Second)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("drain returned before ctx was ever canceled")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain did not return promptly after ctx was canceled")
	}
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}
