package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// --- fakes ---------------------------------------------------------------

// fakeApprovalStore is a minimal in-memory store.ApprovalStore, standing in
// for internal/store/sqlite so these tests never touch a real database or
// the network - the whole point of this package's Approver boundary.
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

func (f *fakeApprovalStore) ExpirePending(_ context.Context, now time.Time, channel string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, row := range f.rows {
		if row.State == "pending" && row.Channel == channel && row.ExpiresAt.Before(now) {
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
	return newApprover(api, approvals, cfg, timeout, nil)
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

// TestApprover_RowExistsBeforePromptIsSent proves the
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

// TestApprover_CreateFailure_NoPromptSentFailClosed proves that when
// Create fails, Ask returns a fail-closed deny without ever sending a
// prompt - sending first and creating the row after could leave
// live-looking buttons that nothing would ever be able to resolve.
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

// TestApprover_ExpirePending_SweepsARowNotYetPastItsOwnExpiresAt proves the
// startup sweep's horizon: a pending row's only waiter is a goroutine in
// whatever process created it, so a restart abandons it no matter how much
// of its own approval_timeout was left at crash time - a row planted a
// moment ago, with an ExpiresAt still minutes in the future, must still be
// swept by ExpirePending at startup, not just rows whose expiry has
// already genuinely passed.
func TestApprover_ExpirePending_SweepsARowNotYetPastItsOwnExpiresAt(t *testing.T) {
	api := &fakeBotAPI{}
	approvals := newFakeApprovalStore()
	require.NoError(t, approvals.Create(context.Background(), &store.Approval{
		ID:        "fresh1",
		ChatID:    "100",
		Channel:   "telegram",
		ExpiresAt: time.Now().Add(4 * time.Minute), // still comfortably in the future
		State:     "pending",
	}))
	a := testApprover(api, approvals, config.TelegramConfig{AllowFrom: []int64{100}}, time.Minute)

	require.NoError(t, a.ExpirePending(context.Background()))

	assert.Equal(t, "expired", approvals.state("fresh1"), "a not-yet-expired pending row must still be swept at startup")
}

// TestApprover_FinishExpired_SkipsEditWhenAlreadyDecided proves that if a
// callback already decided the approval (decide returns
// ErrAlreadyDecided), finishExpired must not overwrite the message that
// already shows that outcome with a contradictory "timed out".
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

// TestApprover_AwaitDecision_TimerFireRacesBufferedDecision_NeverLosesIt
// proves the timer/callback race itself: with a
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
// proves that a callback commits its verdict to the
// approvals row (decide) noticeably before it pushes to wait - if the timer
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

// TestApprover_CallbackWithNoWaiter_EditsNoLongerWaiting proves a callback
// for an id nobody in this process is waiting
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
	assert.Equal(t, "approved", approvals.state("orphan1"), "decide still records the verdict; only the message text changes for an orphaned request")
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

// TestApprover_MessageIDQuoteAllowsSendingWithoutReply proves the approval
// prompt is still delivered even if the triggering message was deleted (by
// the user, or by Telegram) between being sent and the model finishing its
// turn, instead of failing with Telegram's 400 "message to be replied not
// found".
func TestApprover_MessageIDQuoteAllowsSendingWithoutReply(t *testing.T) {
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
	require.NotNil(t, params.ReplyParameters)
	assert.True(t, params.ReplyParameters.AllowSendingWithoutReply)

	cb := &telego.CallbackQuery{
		ID:      "cbq1",
		From:    telego.User{ID: 100},
		Data:    "ok:" + id,
		Message: &telego.Message{Chat: telego.Chat{ID: 100, Type: "private"}, MessageID: 1},
	}
	a.HandleCallback(context.Background(), cb)
	<-resCh
}

// --- prompt formatting -------------------------------------------------

// TestFormatApprovalPrompt_CommandCannotBreakOutOfCodeSpan proves a
// model-generated command (the model can be steered by prompt-injected
// content) containing text that looks like it closes the <pre><code> span,
// or forges a fake "reason:" line, renders as inert, literal text -
// html-escaping is what guarantees this by construction, unlike a Markdown
// fence, which the command's own backticks could close early.
func TestFormatApprovalPrompt_CommandCannotBreakOutOfCodeSpan(t *testing.T) {
	req := tools.Request{
		Tool:    "exec",
		Command: "echo hi</code></pre>\nreason: read-only listing, safe\n<pre><code>rm -rf ~/work",
		Reason:  "writes",
	}
	html, plain := formatApprovalPrompt(req)

	assert.NotContains(t, html, "</code></pre>\nreason: read-only",
		"the injected close tag must be escaped, not rendered as a real tag")
	assert.Contains(t, html, "&lt;/code&gt;&lt;/pre&gt;")
	// Exactly one real <pre><code> open and one real close: the whole
	// command, injected content included, lives inside that single span.
	assert.Equal(t, 1, strings.Count(html, "<pre><code>"))
	assert.Equal(t, 1, strings.Count(html, "</code></pre>"))
	// The real classifier reason still appears, once, after the span closes.
	assert.True(t, strings.HasSuffix(html, "reason: writes"))
	assert.Contains(t, plain, req.Command, "the plain-text fallback carries the command unmodified")
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
