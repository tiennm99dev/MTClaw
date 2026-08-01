package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// --- fakes ---------------------------------------------------------------

// fakeApprovalStore is a minimal in-memory store.ApprovalStore, standing in
// for internal/store/sqlite so these tests never touch a real database or
// the network - the whole point of the phase 6 boundary.
type fakeApprovalStore struct {
	mu         sync.Mutex
	rows       map[string]*store.Approval
	failCreate bool // when true, Create always fails - simulates an infra fault
}

func newFakeApprovalStore() *fakeApprovalStore {
	return &fakeApprovalStore{rows: make(map[string]*store.Approval)}
}

func (f *fakeApprovalStore) Create(_ context.Context, a *store.Approval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return errors.New("fake approval store: create failed")
	}
	if a.ID == "" {
		return errors.New("fake approval store: id required")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	if a.State == "" {
		a.State = "pending"
	}
	cp := *a
	f.rows[a.ID] = &cp
	return nil
}

func (f *fakeApprovalStore) Get(_ context.Context, id string) (*store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *row
	return &cp, nil
}

func (f *fakeApprovalStore) SetMessageID(_ context.Context, id, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[id]; ok {
		row.MessageID = messageID
	}
	return nil
}

func (f *fakeApprovalStore) Decide(_ context.Context, id, state, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return store.ErrNotFound
	}
	if row.State != "pending" {
		return store.ErrAlreadyDecided
	}
	row.State = state
	row.DecidedBy = by
	now := time.Now()
	row.DecidedAt = &now
	return nil
}

func (f *fakeApprovalStore) ExpirePending(_ context.Context, now time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, row := range f.rows {
		if row.State == "pending" && row.ExpiresAt.Before(now) {
			row.State = "expired"
			n++
		}
	}
	return n, nil
}

func (f *fakeApprovalStore) state(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[id]; ok {
		return row.State
	}
	return ""
}

// fakeBotAPI implements botAPI without any network call, recording what was
// sent/edited/answered so tests can assert on it. sendFunc, when set,
// overrides SendMessage's default success behavior (used to simulate a
// parse-mode 400 followed by success).
type fakeBotAPI struct {
	mu        sync.Mutex
	sendFunc  func(params *telego.SendMessageParams) (*telego.Message, error)
	nextMsgID int
	sent      []*telego.SendMessageParams
	edited    []*telego.EditMessageTextParams
	answered  []*telego.AnswerCallbackQueryParams
}

var _ botAPI = (*fakeBotAPI)(nil)

func (f *fakeBotAPI) GetMe(context.Context) (*telego.User, error) {
	return &telego.User{ID: 1, Username: "mtclawbot"}, nil
}

func (f *fakeBotAPI) SetMyCommands(context.Context, *telego.SetMyCommandsParams) error { return nil }

func (f *fakeBotAPI) SendMessage(_ context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	f.mu.Lock()
	f.sent = append(f.sent, params)
	f.mu.Unlock()

	if f.sendFunc != nil {
		return f.sendFunc(params)
	}

	f.mu.Lock()
	f.nextMsgID++
	id := f.nextMsgID
	f.mu.Unlock()
	return &telego.Message{MessageID: id, Chat: telego.Chat{ID: params.ChatID.ID}}, nil
}

func (f *fakeBotAPI) EditMessageText(_ context.Context, params *telego.EditMessageTextParams) (*telego.Message, error) {
	f.mu.Lock()
	f.edited = append(f.edited, params)
	f.mu.Unlock()
	return &telego.Message{MessageID: params.MessageID}, nil
}

func (f *fakeBotAPI) AnswerCallbackQuery(_ context.Context, params *telego.AnswerCallbackQueryParams) error {
	f.mu.Lock()
	f.answered = append(f.answered, params)
	f.mu.Unlock()
	return nil
}

func (f *fakeBotAPI) SendChatAction(context.Context, *telego.SendChatActionParams) error { return nil }

func (f *fakeBotAPI) UpdatesViaLongPolling(context.Context, *telego.GetUpdatesParams, ...telego.LongPollingOption) (<-chan telego.Update, error) {
	ch := make(chan telego.Update)
	close(ch)
	return ch, nil
}

func (f *fakeBotAPI) lastSent() *telego.SendMessageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeBotAPI) lastAnswer() *telego.AnswerCallbackQueryParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answered) == 0 {
		return nil
	}
	return f.answered[len(f.answered)-1]
}

// --- helpers ---------------------------------------------------------------

func testApprover(api *fakeBotAPI, approvals store.ApprovalStore, cfg config.TelegramConfig, timeout time.Duration) *Approver {
	return NewApprover(api, approvals, cfg, timeout, nil)
}

// callbackIDFromLastSend extracts the "ok:<id>"/"no:<id>" callback_data
// embedded in the inline keyboard of the last message fakeBotAPI sent.
func callbackIDFromLastSend(t *testing.T, api *fakeBotAPI) string {
	t.Helper()
	params := api.lastSent()
	require.NotNil(t, params)
	kb, ok := params.ReplyMarkup.(*telego.InlineKeyboardMarkup)
	require.True(t, ok)
	require.NotEmpty(t, kb.InlineKeyboard)
	require.NotEmpty(t, kb.InlineKeyboard[0])
	data := kb.InlineKeyboard[0][0].CallbackData
	_, id, ok := parseCallbackData(data)
	require.True(t, ok)
	return id
}

// --- tests -------------------------------------------------------------

func TestApprover_Approve(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "ls"}

	type result struct {
		approved bool
		err      error
	}
	resCh := make(chan result, 1)
	go func() {
		approved, err := a.Ask(context.Background(), req)
		resCh <- result{approved, err}
	}()

	id := waitForCallbackID(t, api)
	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:" + id,
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)

	select {
	case res := <-resCh:
		assert.True(t, res.approved)
		assert.NoError(t, res.err)
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after the callback")
	}

	assert.Equal(t, "approved", approvals.state(id))
	assert.NotEmpty(t, api.edited, "the prompt message must be edited to show the outcome")
	assert.Empty(t, api.lastAnswer().Text)
}

func TestApprover_Deny(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "rm -rf /"}
	resCh := make(chan bool, 1)
	errCh := make(chan error, 1)
	go func() {
		approved, err := a.Ask(context.Background(), req)
		resCh <- approved
		errCh <- err
	}()

	id := waitForCallbackID(t, api)
	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "no:" + id,
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)

	assert.False(t, <-resCh)
	assert.NoError(t, <-errCh)
	assert.Equal(t, "denied", approvals.state(id))
}

// TestApprover_RowExistsBeforePromptIsSent is the M2 regression test: the
// approvals row must already exist by the moment the prompt message is
// actually sent, since a fast tap on the just-delivered buttons can race
// Ask's own return and must never hit "unknown or expired approval". The
// row's message_id must also end up recorded once the send confirms.
func TestApprover_RowExistsBeforePromptIsSent(t *testing.T) {
	approvals := newFakeApprovalStore()
	sawRow := make(chan bool, 1)
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			kb, ok := params.ReplyMarkup.(*telego.InlineKeyboardMarkup)
			require.True(t, ok)
			_, id, ok := parseCallbackData(kb.InlineKeyboard[0][0].CallbackData)
			require.True(t, ok)
			_, err := approvals.Get(context.Background(), id)
			sawRow <- err == nil
			return &telego.Message{MessageID: 1, Chat: telego.Chat{ID: params.ChatID.ID}}, nil
		},
	}
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "ls"}
	resCh := make(chan bool, 1)
	go func() {
		approved, _ := a.Ask(context.Background(), req)
		resCh <- approved
	}()

	select {
	case ok := <-sawRow:
		assert.True(t, ok, "the approvals row must exist by the time the prompt message is actually sent")
	case <-time.After(2 * time.Second):
		t.Fatal("approval prompt was never sent")
	}

	id := waitForCallbackID(t, api)
	row := mustGet(t, approvals, id)
	assert.Equal(t, "1", row.MessageID, "message_id must be recorded on the row after the send confirms")

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:" + id,
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)
	<-resCh
}

// TestApprover_CreateFailure_NoPromptSentFailClosed is the M2 regression
// test for the other half of the ordering fix: when Create fails, Ask must
// return a fail-closed deny without ever sending a prompt - the old order
// could send live-looking buttons that nothing would ever be able to
// resolve.
func TestApprover_CreateFailure_NoPromptSentFailClosed(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	approvals.failCreate = true
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "rm -rf /"}
	approved, err := a.Ask(context.Background(), req)

	assert.False(t, approved)
	assert.Error(t, err)
	assert.Nil(t, api.lastSent(), "a Create failure must never send a prompt with live-looking buttons")
}

func TestApprover_DoubleTapIsANoOp(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	require.NoError(t, approvals.Create(context.Background(), &store.Approval{
		ID: "dup1", ChatID: "100", ExpiresAt: time.Now().Add(time.Hour),
	}))

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:dup1",
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 7},
	}
	a.HandleCallback(context.Background(), cb)
	assert.Equal(t, "approved", approvals.state("dup1"))
	firstDecidedBy := mustGet(t, approvals, "dup1").DecidedBy

	// A second tap (e.g. a denial after the approval already landed) must
	// change nothing.
	cb2 := &telego.CallbackQuery{
		ID:      "cbq2",
		From:    telego.User{ID: 100},
		Data:    "no:dup1",
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 7},
	}
	a.HandleCallback(context.Background(), cb2)
	assert.Equal(t, "approved", approvals.state("dup1"))
	assert.Equal(t, firstDecidedBy, mustGet(t, approvals, "dup1").DecidedBy)

	last := api.lastAnswer()
	require.NotNil(t, last)
	assert.Contains(t, last.Text, "already decided")
}

func TestApprover_Timeout(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, 30*time.Millisecond)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "ls"}

	start := time.Now()
	approved, err := a.Ask(context.Background(), req)
	elapsed := time.Since(start)

	assert.False(t, approved)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, time.Second)

	id := callbackIDFromLastSend(t, api)
	assert.Equal(t, "expired", approvals.state(id))
	assert.NotEmpty(t, api.edited)
}

func TestApprover_ContextCancelWhilePendingReturnsPromptly(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	// A long timeout: only ctx cancellation should end this wait, proving
	// the ctx.Done() select arm - not the timer - is what fires.
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "ls"}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	approved, err := a.Ask(ctx, req)
	elapsed := time.Since(start)

	assert.False(t, approved)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, time.Second, "ctx cancellation must not wait for the full approval_timeout")

	id := callbackIDFromLastSend(t, api)
	assert.Equal(t, "expired", approvals.state(id))
}

func TestApprover_CallbackFromNonAllowlistedUserIsRejected(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	require.NoError(t, approvals.Create(context.Background(), &store.Approval{
		ID: "auth1", ChatID: "100", ExpiresAt: time.Now().Add(time.Hour),
	}))

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 999}, // not in AllowFrom
		Data:    "ok:auth1",
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)

	assert.Equal(t, "pending", approvals.state("auth1"))
	assert.Contains(t, api.lastAnswer().Text, "not allowed")
}

func TestApprover_CallbackFromDifferentChatIsRejected(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	require.NoError(t, approvals.Create(context.Background(), &store.Approval{
		ID: "chat1", ChatID: "100", ExpiresAt: time.Now().Add(time.Hour),
	}))

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:chat1",
		Message: &telego.Message{Chat: telego.Chat{ID: 200, Type: "private"}, MessageID: 1}, // different chat
	}
	a.HandleCallback(context.Background(), cb)

	assert.Equal(t, "pending", approvals.state("chat1"))
	assert.Contains(t, api.lastAnswer().Text, "different chat")
}

func TestApprover_CallbackForUnknownID(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:does-not-exist",
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)

	assert.Contains(t, api.lastAnswer().Text, "unknown")
}

func TestApprover_ThreadRoutingCarriesMessageThreadID(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", ThreadID: "42", Tool: "exec", Command: "ls"}
	resCh := make(chan bool, 1)
	go func() {
		approved, _ := a.Ask(context.Background(), req)
		resCh <- approved
	}()

	id := waitForCallbackID(t, api)
	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, 42, params.MessageThreadID, "an approval prompt in a forum topic must carry message_thread_id")

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:" + id,
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)
	<-resCh
}

func TestApprover_MessageIDQuotesTheTriggeringMessage(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "ls", MessageID: "777"}
	resCh := make(chan bool, 1)
	go func() {
		approved, _ := a.Ask(context.Background(), req)
		resCh <- approved
	}()

	id := waitForCallbackID(t, api)
	params := api.lastSent()
	require.NotNil(t, params)
	require.NotNil(t, params.ReplyParameters, "an approval prompt with a known triggering message must quote it")
	assert.Equal(t, 777, params.ReplyParameters.MessageID)

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:" + id,
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)
	<-resCh
}

func TestApprover_NoMessageIDLeavesPromptUnanchored(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	a := testApprover(api, approvals, cfg, time.Minute)

	req := tools.Request{SessionID: "s1", Channel: "telegram", ChatID: "100", Tool: "exec", Command: "ls"}
	resCh := make(chan bool, 1)
	go func() {
		approved, _ := a.Ask(context.Background(), req)
		resCh <- approved
	}()

	id := waitForCallbackID(t, api)
	params := api.lastSent()
	require.NotNil(t, params)
	assert.Nil(t, params.ReplyParameters)

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:" + id,
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)
	<-resCh
}

// --- send fallback ---------------------------------------------------------

func TestSendOne_FallsBackToPlainTextOnParseError(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 400, Description: "Bad Request: can't parse entities"}
			}
			assert.Empty(t, params.ParseMode, "the retry must drop parse_mode entirely")
			return &telego.Message{MessageID: 1}, nil
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "bad *markdown", ParseMode: telego.ModeMarkdownV2}
	msg, err := sendOne(context.Background(), api, params, "bad *markdown")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls, "exactly one retry: the message is delivered exactly once")
}

// --- Channel.Send thread routing -------------------------------------------

func TestSendText_CarriesMessageThreadID(t *testing.T) {
	api := &fakeBotAPI{}
	err := sendText(context.Background(), api, "100", "42", "hello", "")
	require.NoError(t, err)

	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, 42, params.MessageThreadID)
}

func TestSendText_EmptyThreadIDTargetsGeneralTimeline(t *testing.T) {
	api := &fakeBotAPI{}
	err := sendText(context.Background(), api, "100", "", "hello", "")
	require.NoError(t, err)

	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, 0, params.MessageThreadID)
}

// --- test helpers ------------------------------------------------------

// waitForCallbackID polls fakeBotAPI until Ask has sent its prompt, then
// extracts the approval id from its inline keyboard - Ask runs on its own
// goroutine in these tests, so the prompt is not necessarily sent the
// instant Ask is called.
func waitForCallbackID(t *testing.T, api *fakeBotAPI) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if api.lastSent() != nil {
			return callbackIDFromLastSend(t, api)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("approval prompt was never sent")
	return ""
}

func mustGet(t *testing.T, approvals *fakeApprovalStore, id string) *store.Approval {
	t.Helper()
	row, err := approvals.Get(context.Background(), id)
	require.NoError(t, err)
	return row
}
