package telegram

import (
	"context"
	"errors"
	"strings"
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
	// sendFunc's own return - and so sawRow firing - races Ask's own
	// SetMessageID call for the same send (both happen after SendMessage
	// returns, on different goroutines with no ordering between them), so
	// the row is not guaranteed to carry message_id yet the instant sawRow
	// is received; poll briefly instead of checking exactly once.
	row := mustGet(t, approvals, id)
	deadline := time.Now().Add(time.Second)
	for row.MessageID == "" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		row = mustGet(t, approvals, id)
	}
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

// TestApprover_FinishExpired_SkipsEditWhenAlreadyDecided is the M3
// regression test: if a callback already decided the approval (Decide
// returns ErrAlreadyDecided), finishExpired must not overwrite the message
// that already shows that outcome with a contradictory "timed out".
func TestApprover_FinishExpired_SkipsEditWhenAlreadyDecided(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	require.NoError(t, approvals.Create(context.Background(), &store.Approval{
		ID: "already1", ChatID: "100", ExpiresAt: time.Now().Add(time.Hour), State: "approved",
	}))
	a := testApprover(api, approvals, config.TelegramConfig{AllowFrom: []int64{100}}, time.Minute)

	a.finishExpired(context.Background(), "already1", "100", 1, "timed out waiting for a response")

	assert.Empty(t, api.edited, "finishExpired must not touch a message whose approval was already decided")
	assert.Equal(t, "approved", approvals.state("already1"))
}

// TestApprover_AwaitDecision_TimerFireRacesBufferedDecision_NeverLosesIt is
// the M3 regression test for the timer/callback race itself: with a
// decision already sitting in wait and an already-elapsed timer, both
// select cases are ready from the start, so Go's runtime picks between them
// at random - run enough times, this reliably exercises both the direct
// <-wait case and the timer case's own drain-before-timeout check. Neither
// path may throw the buffered decision away.
func TestApprover_AwaitDecision_TimerFireRacesBufferedDecision_NeverLosesIt(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	a := testApprover(api, approvals, config.TelegramConfig{AllowFrom: []int64{100}}, time.Minute)

	for i := 0; i < 500; i++ {
		wait := make(chan bool, 1)
		wait <- true
		timer := time.NewTimer(0) // already elapsed: both cases are ready immediately

		approved, err := a.awaitDecision(context.Background(), wait, timer, "no-such-id", "100", 1)
		timer.Stop()

		require.NoError(t, err, "iteration %d: a buffered decision must never be reported as a timeout", i)
		assert.True(t, approved, "iteration %d", i)
	}
}

// TestApprover_AwaitDecision_DecidedBetweenTimerFireAndFinishExpired_HonorsDB
// is the H1 regression test: a callback commits its verdict to the
// approvals row (Decide) noticeably before it pushes to wait - if the timer
// fires inside that exact gap, wait is still empty, but the DB (and the
// message the user already sees, via the callback's own edit) already say
// "approved". awaitDecision must consult the approvals row once it loses
// the finishExpired race and honor that verdict instead of reporting a
// timeout.
func TestApprover_AwaitDecision_DecidedBetweenTimerFireAndFinishExpired_HonorsDB(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	require.NoError(t, approvals.Create(context.Background(), &store.Approval{
		ID: "race1", ChatID: "100", ExpiresAt: time.Now().Add(time.Hour),
	}))
	a := testApprover(api, approvals, config.TelegramConfig{AllowFrom: []int64{100}}, time.Minute)

	// The callback has committed the verdict but not yet reached its own
	// push to wait: wait stays empty.
	require.NoError(t, approvals.Decide(context.Background(), "race1", "approved", "100"))

	wait := make(chan bool, 1)
	timer := time.NewTimer(0) // already elapsed
	defer timer.Stop()

	approved, err := a.awaitDecision(context.Background(), wait, timer, "race1", "100", 1)
	require.NoError(t, err, "a decision already committed to the DB must never be reported as a timeout")
	assert.True(t, approved)
	assert.Equal(t, "approved", approvals.state("race1"), "the winning callback's own verdict must survive, not get overwritten by expiry")
}

// TestApprover_CallbackWithNoWaiter_EditsNoLongerWaiting is the M3
// regression test: a callback for an id nobody in this process is waiting
// on (e.g. a pending row surviving a restart, tapped before the startup
// sweep) must still record the verdict, but the message must say so is no
// longer being waited on rather than falsely implying the gated action will
// run.
func TestApprover_CallbackWithNoWaiter_EditsNoLongerWaiting(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	require.NoError(t, approvals.Create(context.Background(), &store.Approval{
		ID: "orphan1", ChatID: "100", ExpiresAt: time.Now().Add(time.Hour),
	}))
	a := testApprover(api, approvals, config.TelegramConfig{AllowFrom: []int64{100}}, time.Minute)

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:orphan1",
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 5},
	}
	a.HandleCallback(context.Background(), cb)

	require.NotEmpty(t, api.edited)
	last := api.edited[len(api.edited)-1]
	assert.Contains(t, last.Text, "no longer waiting")
	assert.Equal(t, "approved", approvals.state("orphan1"), "Decide still records the verdict; only the message text changes for an orphaned request")
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

// TestSendOne_FallsBackToPlainTextOnTooLongError is the H1 regression test
// for the other 400 wording Telegram uses when MarkdownV2 escaping inflates
// an otherwise in-bounds chunk past 4096 chars: "message is too long" names
// no parsing problem, but must still degrade to plain text instead of
// dropping the reply.
func TestSendOne_FallsBackToPlainTextOnTooLongError(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 400, Description: "Bad Request: message is too long"}
			}
			assert.Empty(t, params.ParseMode, "the retry must drop parse_mode entirely")
			return &telego.Message{MessageID: 1}, nil
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "escaped text", ParseMode: telego.ModeMarkdownV2}
	msg, err := sendOne(context.Background(), api, params, "plain text")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls)
}

func TestSendOne_RetriesOnceOn5xxThenSucceeds(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 500, Description: "Internal Server Error"}
			}
			return &telego.Message{MessageID: 1}, nil
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	start := time.Now()
	msg, err := sendOne(context.Background(), api, params, "hi")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls, "a 5xx gets exactly one bounded retry")
	assert.GreaterOrEqual(t, time.Since(start), transientRetryDelay)
}

// TestSendOne_NetworkErrorIsNotRetried is the H2 regression test: unlike a
// 5xx (which Telegram never accepted), a raw network error can surface
// after Telegram already delivered the message, and Telegram has no
// idempotency key - retrying risks a user-visible duplicate send (or a
// second, unresolved approval prompt), so sendOne must return the error
// immediately instead of retrying it.
func TestSendOne_NetworkErrorIsNotRetried(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			return nil, errors.New("connection reset by peer")
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	_, err := sendOne(context.Background(), api, params, "hi")
	require.Error(t, err)
	assert.Equal(t, 1, calls, "a non-API network error must not be retried - Telegram may have already delivered the message")
}

func TestSendOne_GivesUpAfterOneTransientRetry(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			return nil, &ta.Error{ErrorCode: 503, Description: "Service Unavailable"}
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	_, err := sendOne(context.Background(), api, params, "hi")
	require.Error(t, err)
	assert.Equal(t, 2, calls, "exactly one retry, not a retry storm against a persistently failing backend")
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

// TestSendText_EscapedChunksNeverExceedLimit is the H1 regression test: text
// dense in MarkdownV2-special characters (every rune escapes to two bytes)
// must still produce only chunks whose *escaped* length fits Telegram's
// 4096-char ceiling, not just its pre-escape length.
func TestSendText_EscapedChunksNeverExceedLimit(t *testing.T) {
	var b strings.Builder
	for b.Len() < 20_000 {
		b.WriteString("a.b-c!d(e)f_g*h~i>j#k+l=m|n{o}p.")
	}
	text := b.String()

	api := &fakeBotAPI{}
	require.NoError(t, sendText(context.Background(), api, "100", "", text, ""))

	require.NotEmpty(t, api.sent)
	for i, params := range api.sent {
		assert.LessOrEqualf(t, len(params.Text), DefaultChunkLimit, "chunk %d escaped to %d bytes, over the limit", i, len(params.Text))
	}

	// Every chunk actually sent must reassemble (once unescaped) back into
	// the original text, so the re-split did not drop or reorder content.
	var reassembled strings.Builder
	for _, params := range api.sent {
		reassembled.WriteString(unescapeMDV2ForTest(t, params.Text))
	}
	assert.Equal(t, text, reassembled.String())
}

func TestEscapeChunk_ReSplitsWhenEscapingInflatesPastLimit(t *testing.T) {
	// Every character escapes to two bytes, so a chunk exactly at
	// DefaultChunkLimit pre-escape length inflates to 2x that - over the
	// limit - and must come back as more than one part, each fitting.
	chunk := strings.Repeat(".", DefaultChunkLimit)
	parts := escapeChunk(chunk, DefaultChunkLimit)

	require.Greater(t, len(parts), 1)
	var plainTotal strings.Builder
	for _, p := range parts {
		assert.LessOrEqual(t, len(p.escaped), DefaultChunkLimit)
		assert.Equal(t, EscapeMarkdownV2(p.plain), p.escaped)
		plainTotal.WriteString(p.plain)
	}
	assert.Equal(t, chunk, plainTotal.String())
}

// TestEscapeChunk_FenceInfoStringLongerThanLimit_TerminatesQuickly is the
// C1 regression test: a fence whose opening line's info string alone
// exceeds the limit makes splitFence's own overhead clamp every piece back
// to (near) the full input, so naive re-split-and-recurse never terminates.
// escapeChunk must still return promptly, and every emitted chunk must
// still fit the limit once escaped.
func TestEscapeChunk_FenceInfoStringLongerThanLimit_TerminatesQuickly(t *testing.T) {
	text := "```" + strings.Repeat("A", 5000) + "\nxy\n```"

	done := make(chan []escapedPart, 1)
	go func() {
		var parts []escapedPart
		for _, chunk := range Split(text, DefaultChunkLimit) {
			parts = append(parts, escapeChunk(chunk, DefaultChunkLimit)...)
		}
		done <- parts
	}()

	select {
	case parts := <-done:
		require.NotEmpty(t, parts)
		for i, p := range parts {
			assert.LessOrEqualf(t, len(p.escaped), DefaultChunkLimit, "part %d escaped to %d bytes, over the limit", i, len(p.escaped))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("escapeChunk did not terminate on a fence whose info string alone exceeds the limit")
	}
}

func TestEscapeChunk_FitsWithoutSplittingWhenAlreadyUnderLimit(t *testing.T) {
	chunk := "just plain prose with no special characters at all"
	parts := escapeChunk(chunk, DefaultChunkLimit)
	require.Len(t, parts, 1)
	assert.Equal(t, chunk, parts[0].plain)
	assert.Equal(t, chunk, parts[0].escaped)
}

// unescapeMDV2ForTest strips the backslash MarkdownV2 escaping inserts
// before every special character, so a sent chunk's escaped text can be
// compared back against the original plain input.
func unescapeMDV2ForTest(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
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
