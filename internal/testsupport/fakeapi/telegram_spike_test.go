package fakeapi

import (
	"context"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpike_TelegoTalksToHTTPTestServer is the permanent regression test of
// the transport assumption this whole harness is built on: that telego's
// default (fasthttp-based) caller can be pointed at an httptest.Server via
// telego.WithAPIServer and successfully call getMe, long-poll for an
// injected update, and send a message - on Windows, under -race. A throwaway
// spike already proved this empirically before this phase's implementation
// began (see plan.md's "Resolutions" section); this test is that spike kept
// on file so a future telego upgrade that breaks the assumption fails loudly
// here instead of silently invalidating every e2e test built on top of it.
func TestSpike_TelegoTalksToHTTPTestServer(t *testing.T) {
	fake := NewTelegram()
	t.Cleanup(fake.Close)

	bot, err := telego.NewBot(fake.Token(), telego.WithDiscardLogger(), telego.WithAPIServer(fake.URL()))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// getMe: proves a plain, non-long-polling call round-trips at all.
	me, err := bot.GetMe(ctx)
	require.NoError(t, err, "getMe against the fake server must succeed")
	assert.Equal(t, telegramFakeBotUsername, me.Username)

	// Long polling: an injected update must actually arrive on the channel
	// telego hands back, proving the blocking-getUpdates behavior (see
	// telegramPollWait) does not stall or corrupt delivery.
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	updates, err := bot.UpdatesViaLongPolling(pollCtx, &telego.GetUpdatesParams{Timeout: 1},
		telego.WithLongPollingUpdateInterval(0),
	)
	require.NoError(t, err)

	fake.PushMessage(42, 7, "hello")

	select {
	case u := <-updates:
		require.NotNil(t, u.Message)
		assert.Equal(t, "hello", u.Message.Text)
		assert.EqualValues(t, 42, u.Message.Chat.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("no update arrived within 5s of PushMessage")
	}
	pollCancel()

	// sendMessage: the fake must observe the exact body shape telego sends,
	// not just accept the call - this is what SentTexts/Calls' callers
	// depend on in every gateway/cli e2e test.
	sent, err := bot.SendMessage(ctx, &telego.SendMessageParams{ChatID: telego.ChatID{ID: 42}, Text: "pong"})
	require.NoError(t, err)
	assert.NotZero(t, sent.MessageID)

	found := false
	for _, c := range fake.Calls() {
		if c.Method != "sendMessage" {
			continue
		}
		found = true
		assert.EqualValues(t, 42, ChatIDFromBody(c.Body))
		assert.Equal(t, "pong", c.Body["text"])
	}
	require.True(t, found, "the fake must have recorded the sendMessage call")
}

// TestSpike_GetUpdatesDoesNotBusySpin is R6's confirmation, kept as a
// standing bound rather than a one-off measurement: with nothing queued,
// telego's long-poll loop (UpdateInterval: 0, matching
// internal/channel/telegram/channel.go's production settings) must not
// issue more than a small, fixed number of getUpdates calls while idle.
// Without telegramPollWait's blocking wait, the spike this test replaces
// measured four calls in ~400ms against a non-blocking fake; this test runs
// five times that long and asserts an order-of-magnitude tighter bound,
// which only holds if the wait is actually blocking for close to its full
// duration on most iterations.
func TestSpike_GetUpdatesDoesNotBusySpin(t *testing.T) {
	fake := NewTelegram()
	t.Cleanup(fake.Close)

	bot, err := telego.NewBot(fake.Token(), telego.WithDiscardLogger(), telego.WithAPIServer(fake.URL()))
	require.NoError(t, err)

	const idle = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), idle+2*time.Second)
	defer cancel()

	updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{Timeout: 1},
		telego.WithLongPollingUpdateInterval(0),
	)
	require.NoError(t, err)

	deadline := time.After(idle)
loop:
	for {
		select {
		case <-updates:
			t.Fatal("no update was ever pushed; the update channel must stay empty for this test")
		case <-deadline:
			break loop
		}
	}
	cancel()
	// Drain so doLongPolling's goroutine exits cleanly before the test ends.
	for range updates {
	}

	count := 0
	for _, c := range fake.Calls() {
		if c.Method == "getUpdates" {
			count++
		}
	}
	// idle (2s) / telegramPollWait (250ms) = 8 calls if the wait blocks for
	// its full duration every time; 20 leaves generous headroom for
	// scheduling jitter while still failing hard on a regression back to a
	// non-blocking (or much-too-short) wait.
	assert.Less(t, count, 20, "idle long polling issued %d getUpdates calls in %s; the blocking wait in getUpdates must not have engaged", count, idle)
}
