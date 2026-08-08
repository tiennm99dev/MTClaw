package gateway

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"
	// This package's own gateway.go already carries the blank import that
	// registers "sqlite" with store.Open's driver registry, so this test
	// file - package gateway, not gateway_test - does not need its own.
)

// newTestStore opens a fresh sqlite-backed store.Store at a temp path - the
// same real implementation production code uses, matching the convention
// used across the repo's other test suites (see internal/agent/loop_test.go)
// so session Ensure/CountBySession behave exactly as they do in production.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "gateway-test.db")
	st, err := store.Open(ctx, config.StorageConfig{Driver: "sqlite", DSN: dbPath}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newTestDispatcher builds a dispatcher against a real temp store, with
// rootCtx defaulted to context.Background() so dispatch() works without a
// Gateway.Run wrapping it.
func newTestDispatcher(t *testing.T, runner turnRunner, ch Channel, idleTimeout time.Duration, concurrency int) *dispatcher {
	t.Helper()
	st := newTestStore(t)
	return newDispatcher(st, ch, runner, slog.New(slog.NewTextHandler(discardWriter{}, nil)), idleTimeout, concurrency)
}

// discardWriter is a zero-alloc io.Writer sink for test loggers that should
// not spam `go test -v` output.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fakeRunner is a minimal, test-only turnRunner: fn decides the Result (and
// can observe ctx, sessionID, userText, messageID) per call.
type fakeRunner struct {
	fn func(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result
}

func (f *fakeRunner) Run(ctx context.Context, sessionID, userText, messageID string, onProgress agent.Progress) agent.Result {
	return f.fn(ctx, sessionID, userText, messageID, onProgress)
}

// sentMsg records one fakeChannel.Send call.
type sentMsg struct {
	chatID, threadID, text, replyTo string
}

// fakeChannel implements gateway.Channel without any network, recording
// every Send/SendTyping call so tests can assert on them.
type fakeChannel struct {
	mu          sync.Mutex
	sent        []sentMsg
	typingCalls int
}

func (f *fakeChannel) Send(_ context.Context, chatID, threadID, text, replyTo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{chatID, threadID, text, replyTo})
	return nil
}

func (f *fakeChannel) SendTyping(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typingCalls++
	return nil
}

func (f *fakeChannel) sentSnapshot() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMsg, len(f.sent))
	copy(out, f.sent)
	return out
}

var _ Channel = (*fakeChannel)(nil)

// waitCond polls cond until it reports true or timeout elapses, failing the
// test in the latter case. Used throughout instead of a fixed sleep because
// the dispatcher's own concurrency means a fixed sleep is either flaky
// (too short) or slow (too long) depending on the machine.
func waitCond(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition was never satisfied within the timeout")
	}
}

// inboundTo builds a minimal channel.Inbound for one chat, defaulting
// Channel to "telegram" since that is the gateway's only real channel.
func inboundTo(chatID, text string) channel.Inbound {
	return channel.Inbound{Channel: "telegram", ChatID: chatID, Text: text}
}
