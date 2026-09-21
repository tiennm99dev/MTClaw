package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"

	"github.com/tiennm99/MTClaw/internal/channel"
)

// pumpUpdates dispatches every update from updates until the channel closes
// (which happens when ctx is done - telego's UpdatesViaLongPolling
// contract). CallbackQuery updates go to the approver on their own
// goroutine, tracked in c.cbWG so Start can wait for them before returning -
// the approver is mutex-safe and the store serializes its own writes, so
// this cannot corrupt anything, and it stops a slow approval decision (or a
// rate-limited edit) from stalling the pump. Message updates stay on this
// goroutine: they either get handled here (a recognized bot command) or get
// forwarded to out, and out's own backpressure semantics depend on that
// blocking send happening on the pump, not a spawned one.
func (c *Channel) pumpUpdates(ctx context.Context, updates <-chan telego.Update, out chan<- channel.Inbound) {
	for update := range updates {
		if update.CallbackQuery != nil {
			cb := update.CallbackQuery
			c.cbWG.Add(1)
			go func() {
				defer c.cbWG.Done()
				c.approver.HandleCallback(ctx, cb)
			}()
		}
		if update.Message != nil {
			c.handleMessage(ctx, update.Message, out)
		}
	}
}

// handleMessage gates one inbound Telegram message, drops anything that
// gated through with no usable text (a sticker, a photo with no caption, a
// group service message - Decide already ruled out an access-denied
// rejection, there is just nothing here for the agent loop to act on),
// intercepts recognized bot commands itself (replying directly rather than
// forwarding them to the agent loop), and forwards anything else to out as
// an accepted Inbound.
func (c *Channel) handleMessage(ctx context.Context, msg *telego.Message, out chan<- channel.Inbound) {
	accept, cleanText, reason := Decide(c.cfg.Channels.Telegram, c.username, c.botID, msg)
	if !accept {
		c.log.Debug("telegram: rejected inbound message", "reason", reason)
		return
	}
	if strings.TrimSpace(cleanText) == "" {
		c.log.Debug("telegram: dropping accepted message with no usable text")
		return
	}

	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	threadID := ""
	if msg.MessageThreadID != 0 {
		threadID = strconv.Itoa(msg.MessageThreadID)
	}
	var fromID int64
	if msg.From != nil {
		fromID = msg.From.ID
	}

	if cmd := commandName(cleanText); cmd != "" {
		if _, known := commandNames[cmd]; known {
			reply := handleCommand(ctx, c.cfg, c.deps, cmd, chatID, threadID, fromID)
			if reply != "" {
				if err := c.Send(ctx, chatID, threadID, reply, ""); err != nil {
					c.log.Error("telegram: send command reply failed", "command", cmd, "error", err)
				}
			}
			return
		}
	}

	inbound := channel.Inbound{
		Channel:   c.Name(),
		ChatID:    chatID,
		ThreadID:  threadID,
		UserID:    strconv.FormatInt(fromID, 10),
		Text:      cleanText,
		MessageID: strconv.Itoa(msg.MessageID),
	}
	select {
	case out <- inbound:
	case <-ctx.Done():
	}
}
