package telegram

import (
	"context"
	"time"

	"github.com/mymmrac/telego"
)

// Sender is one distinct message sender observed during a CaptureSenders
// window.
type Sender struct {
	UserID   int64
	Username string
}

// CaptureSenders long-polls token's bot for the full window and returns
// every distinct sender who messaged it, in first-seen order. It is
// `onboard`'s (phase 9) Telegram user-ID capture step: whoever messages the
// bot during this window is a candidate owner of channels.telegram.allow_from,
// so this collects every sender across the whole window rather than
// returning on the first message - a second, later sender is exactly the
// case onboard needs to surface, not race past. The caller is responsible
// for getting explicit on-screen confirmation before writing anything to
// config; CaptureSenders itself never touches disk.
func CaptureSenders(ctx context.Context, token string, window time.Duration) ([]Sender, error) {
	bot, err := telego.NewBot(token, telego.WithDiscardLogger())
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	updates, err := bot.UpdatesViaLongPolling(cctx, &telego.GetUpdatesParams{Timeout: longPollTimeoutSeconds},
		telego.WithLongPollingUpdateInterval(0),
	)
	if err != nil {
		return nil, err
	}

	var order []int64
	seen := make(map[int64]Sender)
	for update := range updates {
		if update.Message == nil || update.Message.From == nil {
			continue
		}
		id := update.Message.From.ID
		if _, ok := seen[id]; !ok {
			order = append(order, id)
		}
		seen[id] = Sender{UserID: id, Username: update.Message.From.Username}
	}

	result := make([]Sender, 0, len(order))
	for _, id := range order {
		result = append(result, seen[id])
	}
	return result, nil
}
