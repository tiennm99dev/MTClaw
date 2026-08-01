package telegram

import (
	"context"
	"strconv"

	"github.com/mymmrac/telego"

	"github.com/tiennm99/MTClaw/internal/channel"
)

// pumpUpdates dispatches every update from updates until the channel closes
// (which happens when ctx is done - telego's UpdatesViaLongPolling
// contract): CallbackQuery updates go to the approver, Message updates go
// through gating and either get handled here (a recognized bot command) or
// get forwarded to out.
func (c *Channel) pumpUpdates(ctx context.Context, updates <-chan telego.Update, out chan<- channel.Inbound) {
	for update := range updates {
		if update.CallbackQuery != nil {
			c.approver.HandleCallback(ctx, update.CallbackQuery)
		}
		if update.Message != nil {
			c.handleMessage(ctx, update.Message, out)
		}
	}
}

// handleMessage gates one inbound Telegram message, intercepts recognized
// bot commands itself (replying directly rather than forwarding them to the
// agent loop), and forwards anything else to out as an accepted Inbound.
func (c *Channel) handleMessage(ctx context.Context, msg *telego.Message, out chan<- channel.Inbound) {
	accept, cleanText, reason := Decide(c.cfg.Channels.Telegram, c.username, c.botID, msg)
	if !accept {
		c.log.Debug("telegram: rejected inbound message", "reason", reason)
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
