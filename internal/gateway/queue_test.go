package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/channel"
)

// syncBuffer is a mutex-guarded bytes.Buffer, since relayInbound logs from
// its own goroutine while the test concurrently reads what was logged so
// far - a plain bytes.Buffer would race under -race here.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTrySend_SucceedsWithCapacityRemaining(t *testing.T) {
	ch := make(chan channel.Inbound, 1)
	assert.True(t, trySend(ch, channel.Inbound{Text: "a"}))
}

func TestTrySend_FailsWhenFull(t *testing.T) {
	ch := make(chan channel.Inbound, 1)
	require.True(t, trySend(ch, channel.Inbound{Text: "a"}))
	assert.False(t, trySend(ch, channel.Inbound{Text: "b"}), "a full channel must report a miss, not block")
}

func TestRelayInbound_DropsAndWarnsWhenGlobalQueueFull(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	in := make(chan channel.Inbound)
	out := make(chan channel.Inbound, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go relayInbound(ctx, in, out, log)

	in <- channel.Inbound{Text: "1"} // fills out's one slot
	in <- channel.Inbound{Text: "2"} // out is now full: must warn+drop, not block

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "queue full") {
		time.Sleep(2 * time.Millisecond)
	}
	assert.Contains(t, buf.String(), "queue full")

	select {
	case got := <-out:
		assert.Equal(t, "1", got.Text, "the message accepted before overflow must not be lost")
	default:
		t.Fatal("expected the first message to still be queued")
	}
}

func TestRelayInbound_StopsOnContextDone(t *testing.T) {
	in := make(chan channel.Inbound)
	out := make(chan channel.Inbound, 1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		relayInbound(ctx, in, out, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relayInbound did not stop after ctx was canceled")
	}
}
