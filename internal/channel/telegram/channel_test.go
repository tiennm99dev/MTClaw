package telegram

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/config"
)

// TestBotLogger_ErrorfRedactsTokenFromMessage proves the bot token never
// reaches a log line, even when it happens to appear inside whatever text
// telego's Errorf formats (a defensive redaction, not a case telego's own
// error strings are known to trigger today).
func TestBotLogger_ErrorfRedactsTokenFromMessage(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	l := newBotLogger(log, "secret-token-123")

	l.Errorf("Getting updates: unauthorized (token secret-token-123 rejected)")

	out := buf.String()
	assert.NotContains(t, out, "secret-token-123", "the bot token must never reach a log line")
	assert.Contains(t, out, "<redacted>")
	assert.Contains(t, out, "Getting updates")
}

// TestBotLogger_ErrorfLogsWithoutToken proves a getUpdates failure - the
// exact scenario WithDiscardLogger previously swallowed entirely - actually
// reaches the logger now.
func TestBotLogger_ErrorfLogsWithoutToken(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	l := newBotLogger(log, "tok")

	l.Errorf("Getting updates: %s", "409 Conflict")

	assert.Contains(t, buf.String(), "409 Conflict")
}

// TestBotLogger_ErrorfLogsAtWarnNotError proves telego calling Errorf for
// every failed API call, not just polling failures, does not surface as an
// ERROR line for a request this package already recovers from on its own
// (an HTML parse-mode 400 that triggers the plain-text fallback, a 429
// that gets retried).
func TestBotLogger_ErrorfLogsAtWarnNotError(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	l := newBotLogger(log, "tok")

	l.Errorf("Execution error sendMessage: %s", "Bad Request: can't parse entities")

	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.NotContains(t, out, "level=ERROR")
}

// TestBotLogger_DebugfIsANoOp proves Debugf never reaches the logger at all
// - telego's own docs warn debug output can include the bot token.
func TestBotLogger_DebugfIsANoOp(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	l := newBotLogger(log, "tok")

	l.Debugf("some debug line with tok in it")

	assert.Empty(t, buf.String())
}

// TestChannelStart_ReturnsPromptlyOnCancelWhileLongPollIsHeld proves a
// shutdown does not wait out an in-flight getUpdates: the server holds the
// poll for up to 20s, and Start must return within a second of cancel.
func TestChannelStart_ReturnsPromptlyOnCancelWhileLongPollIsHeld(t *testing.T) {
	var polling atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			polling.Store(true)
			select {
			case <-r.Context().Done():
			case <-time.After(20 * time.Second):
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"b","username":"b_bot"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(srv.Close)

	bot, err := newBot(testBotToken, srv.URL, telego.WithDiscardLogger())
	require.NoError(t, err)
	log := discardLog()
	ch := &Channel{
		api:      bot,
		cfg:      &config.Config{},
		log:      log,
		approver: newApprover(bot, newFakeApprovalStore(), config.TelegramConfig{}, time.Second, log),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ch.Start(ctx, make(chan channel.Inbound, 1)) }()

	require.Eventually(t, polling.Load, 5*time.Second, 5*time.Millisecond, "the long poll never started")
	start := time.Now()
	cancel()
	select {
	case <-done:
		assert.Less(t, time.Since(start), time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancel while getUpdates was in flight")
	}
}
