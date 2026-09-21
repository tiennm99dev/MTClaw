// Package telegram implements internal/channel.Channel as a Telegram
// long-polling bot: access gating, message chunking, bot commands, and the
// inline-keyboard tools.Approver. It depends on internal/config,
// internal/store's interfaces, and internal/tools' Approver/Request/
// RedactSecrets - never on internal/agent's loop internals, so this
// package is self-contained and testable without a gateway.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"

	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// longPollTimeoutSeconds is Telegram's getUpdates long-poll timeout: long
// enough to avoid hammering the API with empty polls, short enough to
// notice a shutdown promptly.
const longPollTimeoutSeconds = 30

var _ channel.Channel = (*Channel)(nil)

// Channel is the Telegram implementation of internal/channel.Channel.
type Channel struct {
	api      botAPI
	cfg      *config.Config
	deps     Deps
	approver *Approver
	log      *slog.Logger

	username string
	botID    int64

	// cbWG tracks every callback-handling goroutine pumpUpdates spawns, so
	// Start can wait for them to finish before returning - otherwise a
	// callback still in flight when the gateway's shutdown drain closes the
	// store loses its verdict to "sql: database is closed" with no signal
	// back to the user who tapped the button.
	cbWG sync.WaitGroup
}

// New constructs a Channel from a resolved token (cfg.Channels.Telegram.Token()
// must already be non-empty; Load's secret resolution is what fills it in).
// deps may be nil - the gateway (phase 7) supplies it once a session
// backend and cancel registry exist; until then, bot commands that need it
// degrade to an explanatory reply instead of panicking.
//
// It never passes telego.WithDefaultDebugLogger (that option's own docs warn
// it can log the bot token) and instead wires botLogger: Debugf is a no-op,
// and Errorf routes through log with the token redacted. A discarded logger
// (the prior behavior) meant a poll failure - a revoked token, a sustained
// network failure, a second poller's 409 Conflict - was logged nowhere and
// silently retried forever; this keeps the token out of every log line
// while making that failure visible.
func New(cfg *config.Config, approvals store.ApprovalStore, deps Deps, log *slog.Logger) (*Channel, error) {
	if log == nil {
		log = slog.Default()
	}
	token := cfg.Channels.Telegram.Token()
	if token == "" {
		return nil, fmt.Errorf("telegram: no bot token resolved; set channels.telegram.token_env or token_file")
	}

	bot, err := telego.NewBot(token, telego.WithLogger(newBotLogger(log, token)))
	if err != nil {
		return nil, fmt.Errorf("telegram: construct bot: %w", err)
	}

	ch := &Channel{
		api:  bot,
		cfg:  cfg,
		deps: deps,
		log:  log,
	}
	ch.approver = NewApprover(bot, approvals, cfg.Channels.Telegram, cfg.Tools.Exec.ApprovalTimeout.Std(), log)
	return ch, nil
}

// SendOnce constructs a bot directly from token and sends one message,
// without building a full Channel (no approver, no store involved at all).
// This is what `mtclaw send` (a one-shot outbound message, useful for
// scripts and for verifying a token before running the gateway) uses
// instead of standing up a Channel.
func SendOnce(ctx context.Context, token, chatID, threadID, text string) error {
	if token == "" {
		return fmt.Errorf("telegram: no bot token resolved; set channels.telegram.token_env or token_file")
	}
	bot, err := telego.NewBot(token, telego.WithDiscardLogger())
	if err != nil {
		return fmt.Errorf("telegram: construct bot: %w", err)
	}
	return sendText(ctx, bot, chatID, threadID, text, "")
}

// GetMe constructs a bot directly from token and returns its username,
// without building a full Channel - the same one-shot shape as SendOnce,
// used by `mtclaw doctor`'s Telegram getMe check and `onboard`'s bot-identity
// confirmation (phase 9) instead of standing up a Channel.
func GetMe(ctx context.Context, token string) (username string, err error) {
	if token == "" {
		return "", fmt.Errorf("telegram: no bot token resolved; set channels.telegram.token_env or token_file")
	}
	bot, err := telego.NewBot(token, telego.WithDiscardLogger())
	if err != nil {
		return "", fmt.Errorf("telegram: construct bot: %w", err)
	}
	me, err := bot.GetMe(ctx)
	if err != nil {
		return "", fmt.Errorf("telegram: getMe: %w", err)
	}
	return me.Username, nil
}

// Name identifies this channel for session addressing.
func (c *Channel) Name() string { return "telegram" }

// Approver exposes the Telegram inline-button tools.Approver this channel
// built, for the gateway (phase 7) to wire into the tool registry.
func (c *Channel) Approver() tools.Approver { return c.approver }

// Send delivers text to chatID, chunked and MarkdownV2-formatted with a
// plain-text fallback; see send.go.
func (c *Channel) Send(ctx context.Context, chatID, threadID, text, replyTo string) error {
	return sendText(ctx, c.api, chatID, threadID, text, replyTo)
}

// SendTyping issues one "typing" chat action. Telegram's indicator expires
// after ~5s; a caller running a long turn is expected to call this again
// every ~4s for as long as the turn runs.
func (c *Channel) SendTyping(ctx context.Context, chatID, threadID string) error {
	return sendTyping(ctx, c.api, chatID, threadID)
}

// Start resolves the bot's identity, registers its command menu, expires
// any approvals left pending by a prior process, then pumps long-polled
// updates until ctx is done.
func (c *Channel) Start(ctx context.Context, out chan<- channel.Inbound) error {
	me, err := c.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram: getMe: %w", err)
	}
	c.username = me.Username
	c.botID = me.ID
	c.log.Info("telegram: bot ready", "username", c.username)

	if err := registerCommands(ctx, c.api); err != nil {
		c.log.Error("telegram: register bot commands failed", "error", err)
	}
	if err := c.approver.ExpirePending(ctx); err != nil {
		c.log.Error("telegram: expire pending approvals at startup failed", "error", err)
	}

	updates, err := c.api.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{Timeout: longPollTimeoutSeconds},
		telego.WithLongPollingUpdateInterval(0),
		telego.WithLongPollingRetryTimeout(8*time.Second),
		telego.WithLongPollingBuffer(100),
	)
	if err != nil {
		return fmt.Errorf("telegram: start long polling: %w", err)
	}

	c.pumpUpdates(ctx, updates, out)
	// Closing ctx is what closes the updates channel (telego's documented
	// shutdown hook), so a clean range-loop exit here means ctx ended. Wait
	// for every callback goroutine pumpUpdates spawned to finish inside the
	// same drain window the gateway already gives pumpUpdates itself,
	// before the caller can go on to close the store.
	c.cbWG.Wait()
	return ctx.Err()
}

// botLogger adapts telego's Logger interface (Debugf/Errorf) to log/slog.
// Debugf is a no-op - telego's own docs warn its debug output can include
// the bot token, and there is nothing operationally useful in it this
// package needs. Errorf is what makes a poll failure visible at all: telego
// logs every getUpdates error (and its own "retrying in Ns..." line) through
// this interface, and with WithDiscardLogger that output went nowhere,
// leaving a revoked token or a sustained network failure silently retried
// forever with zero log output. Errorf routes to slog's Warn level, not
// Error: telego calls it for every failed API call, not just polling, so
// cases this package already recovers from on its own (a MarkdownV2 400
// that triggers the plain-text fallback, a 429 that gets retried,
// editMessageText's harmless "message is not modified") would otherwise
// each log an ERROR line for a request that ultimately succeeded.
type botLogger struct {
	log   *slog.Logger
	token string
}

// newBotLogger builds a botLogger. token, when non-empty, is redacted from
// every Errorf message before it reaches log - telego's error strings for a
// getUpdates failure do not normally include the token, but this is
// defensive: a future telego release, or a transport error wrapping the
// request URL, could include it.
func newBotLogger(log *slog.Logger, token string) *botLogger {
	return &botLogger{log: log, token: token}
}

func (l *botLogger) Debugf(format string, args ...any) {}

func (l *botLogger) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if l.token != "" {
		msg = strings.ReplaceAll(msg, l.token, "<redacted>")
	}
	l.log.Warn("telegram: " + msg)
}

var _ telego.Logger = (*botLogger)(nil)
