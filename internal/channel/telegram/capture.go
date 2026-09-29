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

// captureWindowGrace absorbs ordinary clock skew between this host and
// Telegram's servers when comparing a message's second-resolution Date
// against windowStart's cutoff - see captureWindowStart. A few seconds is
// negligible next to the 24h of stale updates windowStart already exists to
// filter out, so it cannot meaningfully reopen that gap.
const captureWindowGrace = 3 * time.Second

// captureWindowStart derives CaptureSenders' window cutoff from now:
// truncated to whole seconds and backdated by captureWindowGrace.
// update.Message.Date (compared against this cutoff in CaptureSenders) is a
// Telegram server-side Unix timestamp with whole-second resolution, while
// now carries sub-second precision - comparing them directly would drop a
// real message sent in the same second this call started (Date truncates
// down, so it can read as "before" a windowStart a few hundred
// milliseconds into that same second) or during any local-to-Telegram
// clock skew. Truncating removes the first source of loss; the grace
// period absorbs the second - onboard only needs "no false negative", not
// a tight cutoff, since a stray sender from a few seconds before this call
// is still an honest candidate for the user to review and reject on
// screen.
func captureWindowStart(now time.Time) time.Time {
	return now.Truncate(time.Second).Add(-captureWindowGrace)
}

// CaptureSenders long-polls token's bot for the full window and returns
// every distinct sender who messaged it during that window, in first-seen
// order. It is `mtclaw onboard`'s Telegram user-ID capture step: whoever
// messages the bot during this window is a candidate owner of
// channels.telegram.allow_from, so this collects every sender across the
// whole window rather than returning on the first message - a second,
// later sender is exactly the case onboard needs to surface, not race
// past. The caller is responsible for getting explicit on-screen
// confirmation before writing anything to config; CaptureSenders itself
// never touches disk. apiBaseURL is normally cfg.Channels.Telegram.APIBaseURL;
// onboard has no config to read one from yet, so it always passes "".
func CaptureSenders(ctx context.Context, token, apiBaseURL string, window time.Duration) ([]Sender, error) {
	bot, err := oneShotBot(token, apiBaseURL)
	if err != nil {
		return nil, err
	}

	// getUpdates' first response can include up to 24h of updates the bot
	// received before this call ever started (anyone who messaged it
	// earlier, for an unrelated reason). Recording that sender as a
	// candidate owner - a stranger who messaged the bot last week - would
	// be misleading; windowStart is the cutoff a message must be at or
	// after to count as having arrived during this capture window. See
	// captureWindowStart for why it is not simply time.Now().
	windowStart := captureWindowStart(time.Now())

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
		if time.Unix(update.Message.Date, 0).Before(windowStart) {
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
