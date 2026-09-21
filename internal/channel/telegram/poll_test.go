package telegram

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeDeps is a minimal Deps for pump-level command tests, recording calls
// instead of touching a real session backend.
type fakeDeps struct {
	mu           sync.Mutex
	resetCalls   int
	cancelCalls  int
	cancelResult bool
}

func (f *fakeDeps) Status(context.Context, string, string) (SessionStatus, error) {
	return SessionStatus{SessionID: "s1", Model: "gpt"}, nil
}

func (f *fakeDeps) Reset(context.Context, string, string) error {
	f.mu.Lock()
	f.resetCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeDeps) Cancel(string, string) bool {
	f.mu.Lock()
	f.cancelCalls++
	f.mu.Unlock()
	return f.cancelResult
}

var _ Deps = (*fakeDeps)(nil)

func testChannel(api *fakeBotAPI, deps Deps) *Channel {
	cfg := &config.Config{Channels: config.ChannelsConfig{Telegram: config.TelegramConfig{AllowFrom: []int64{100}}}}
	return &Channel{
		api:      api,
		cfg:      cfg,
		deps:     deps,
		approver: NewApprover(api, newFakeApprovalStore(), cfg.Channels.Telegram, time.Minute, discardLog()),
		log:      discardLog(),
		username: testBotUsername,
		botID:    testBotID,
	}
}

func privateMsg(id int, text string) *telego.Message {
	return &telego.Message{
		MessageID: id,
		Chat:      telego.Chat{ID: 100, Type: "private"},
		From:      &telego.User{ID: 100},
		Text:      text,
	}
}

// --- command interception ---------------------------------------------------

func TestHandleMessage_KnownCommandIsInterceptedNotForwarded(t *testing.T) {
	api := &fakeBotAPI{}
	deps := &fakeDeps{}
	c := testChannel(api, deps)

	out := make(chan channel.Inbound, 1)
	c.handleMessage(context.Background(), privateMsg(1, "/new"), out)

	select {
	case in := <-out:
		t.Fatalf("a recognized command must not be forwarded to the agent loop, got %+v", in)
	default:
	}

	deps.mu.Lock()
	resetCalls := deps.resetCalls
	deps.mu.Unlock()
	assert.Equal(t, 1, resetCalls, "/new must call Deps.Reset")
	require.NotEmpty(t, api.sent, "/new must reply directly")
}

func TestHandleMessage_StopCommandCallsCancel(t *testing.T) {
	api := &fakeBotAPI{}
	deps := &fakeDeps{cancelResult: true}
	c := testChannel(api, deps)

	out := make(chan channel.Inbound, 1)
	c.handleMessage(context.Background(), privateMsg(1, "/stop"), out)

	select {
	case in := <-out:
		t.Fatalf("/stop must not be forwarded to the agent loop, got %+v", in)
	default:
	}
	deps.mu.Lock()
	cancelCalls := deps.cancelCalls
	deps.mu.Unlock()
	assert.Equal(t, 1, cancelCalls)
	require.NotEmpty(t, api.sent)
	assert.Contains(t, api.lastSent().Text, "cancel")
}

func TestHandleMessage_UnrecognizedTextIsForwarded(t *testing.T) {
	api := &fakeBotAPI{}
	c := testChannel(api, nil)

	out := make(chan channel.Inbound, 1)
	c.handleMessage(context.Background(), privateMsg(1, "what's the weather"), out)

	select {
	case in := <-out:
		assert.Equal(t, "what's the weather", in.Text)
		assert.Equal(t, "telegram", in.Channel)
		assert.Equal(t, "100", in.ChatID)
	case <-time.After(time.Second):
		t.Fatal("ordinary text must be forwarded to the agent loop")
	}
	assert.Empty(t, api.sent, "forwarding must not itself reply")
}

// --- empty-text drop (M2) ---------------------------------------------------

func TestHandleMessage_EmptyTextAfterGating_Dropped(t *testing.T) {
	api := &fakeBotAPI{}
	c := testChannel(api, nil)

	out := make(chan channel.Inbound, 1)
	// A sticker/photo with no caption: Decide accepts it (allowlisted
	// sender, private chat) but Text is empty - nothing for the agent loop
	// to act on.
	c.handleMessage(context.Background(), privateMsg(1, ""), out)

	select {
	case in := <-out:
		t.Fatalf("an accepted message with no usable text must be dropped, got %+v", in)
	default:
	}
	assert.Empty(t, api.sent, "a dropped message must not get a reply either")
}

func TestHandleMessage_CaptionForwardedWhenTextEmpty(t *testing.T) {
	api := &fakeBotAPI{}
	c := testChannel(api, nil)

	msg := privateMsg(1, "")
	msg.Caption = "what is this?"

	out := make(chan channel.Inbound, 1)
	c.handleMessage(context.Background(), msg, out)

	select {
	case in := <-out:
		assert.Equal(t, "what is this?", in.Text)
	case <-time.After(time.Second):
		t.Fatal("a captioned message must be forwarded using its caption as text")
	}
}

// --- callback routing (M5) --------------------------------------------------

// blockingApprovalStore wraps fakeApprovalStore so its Get call blocks on
// unblock, standing in for a slow/rate-limited approval decision.
type blockingApprovalStore struct {
	*fakeApprovalStore
	unblock chan struct{}
}

func (b *blockingApprovalStore) Get(ctx context.Context, id string) (*store.Approval, error) {
	<-b.unblock
	return b.fakeApprovalStore.Get(ctx, id)
}

// TestPumpUpdates_CallbackDoesNotBlockMessagePump is the M5 regression test:
// a slow-to-resolve CallbackQuery must not stall a Message update queued
// right behind it - pumpUpdates must hand the callback to its own goroutine
// rather than processing it inline.
func TestPumpUpdates_CallbackDoesNotBlockMessagePump(t *testing.T) {
	unblock := make(chan struct{})
	approvals := &blockingApprovalStore{fakeApprovalStore: newFakeApprovalStore(), unblock: unblock}
	api := &fakeBotAPI{}
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	c := &Channel{
		api:      api,
		cfg:      &config.Config{Channels: config.ChannelsConfig{Telegram: cfg}},
		approver: NewApprover(api, approvals, cfg, time.Minute, discardLog()),
		log:      discardLog(),
		username: testBotUsername,
		botID:    testBotID,
	}

	updates := make(chan telego.Update, 2)
	updates <- telego.Update{CallbackQuery: &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:stuck",
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}}
	updates <- telego.Update{Message: privateMsg(2, "hello")}
	close(updates)

	out := make(chan channel.Inbound, 1)
	pumpDone := make(chan struct{})
	go func() {
		c.pumpUpdates(context.Background(), updates, out)
		close(pumpDone)
	}()

	select {
	case in := <-out:
		assert.Equal(t, "hello", in.Text, "the message behind a stuck callback must still be forwarded promptly")
	case <-time.After(2 * time.Second):
		t.Fatal("the message update was blocked behind the still-pending callback")
	}

	close(unblock)
	select {
	case <-pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpUpdates never returned after the updates channel closed")
	}
}

// TestPumpUpdates_TracksCallbackGoroutinesForWaiting is the M2 regression
// test: pumpUpdates must track every callback-handling goroutine it spawns
// in c.cbWG, so Start can wait for it to actually finish (inside the
// gateway's shutdown drain window) instead of returning while a callback is
// still touching the store.
func TestPumpUpdates_TracksCallbackGoroutinesForWaiting(t *testing.T) {
	unblock := make(chan struct{})
	approvals := &blockingApprovalStore{fakeApprovalStore: newFakeApprovalStore(), unblock: unblock}
	api := &fakeBotAPI{}
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	c := &Channel{
		api:      api,
		cfg:      &config.Config{Channels: config.ChannelsConfig{Telegram: cfg}},
		approver: NewApprover(api, approvals, cfg, time.Minute, discardLog()),
		log:      discardLog(),
		username: testBotUsername,
		botID:    testBotID,
	}

	updates := make(chan telego.Update, 1)
	updates <- telego.Update{CallbackQuery: &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:stuck",
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}}
	close(updates)

	out := make(chan channel.Inbound, 1)
	c.pumpUpdates(context.Background(), updates, out)

	waitDone := make(chan struct{})
	go func() {
		c.cbWG.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("cbWG.Wait() returned before the still-in-flight callback finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(unblock)
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cbWG.Wait() never returned after the callback finished")
	}
}
