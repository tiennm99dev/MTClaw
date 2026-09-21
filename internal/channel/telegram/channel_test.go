package telegram

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBotLogger_ErrorfRedactsTokenFromMessage is the H2 regression test: the
// bot token must never reach a log line, even when it happens to appear
// inside whatever text telego's Errorf formats (a defensive redaction, not a
// case telego's own error strings are known to trigger today).
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

// TestBotLogger_ErrorfLogsAtWarnNotError is the M4 regression test: telego
// calls Errorf for every failed API call, not just polling failures, so
// cases this package already recovers from on its own (a MarkdownV2 400
// that triggers the plain-text fallback, a 429 that gets retried) must not
// each surface as an ERROR line for a request that ultimately succeeded.
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
