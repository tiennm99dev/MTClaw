package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// defaultApprovalTimeout is used when the caller supplies a zero timeout,
// mirroring config.Default()'s tools.exec.approval_timeout.
const defaultApprovalTimeout = 5 * time.Minute

// Approver implements tools.Approver with Telegram inline Yes/No buttons.
// It owns its approvals rows end to end (create, decide, expire): the
// separate bookkeeping row internal/tools' exec tool creates before calling
// Ask is a channel-agnostic audit record, not the row these buttons act on.
type Approver struct {
	api       botAPI
	approvals store.ApprovalStore
	cfg       config.TelegramConfig
	timeout   time.Duration
	log       *slog.Logger

	mu      sync.Mutex
	waiters map[string]chan bool
}

var _ tools.Approver = (*Approver)(nil)

// NewApprover builds an Approver sending prompts via api and persisting
// state through approvals. timeout <= 0 uses defaultApprovalTimeout.
func NewApprover(api botAPI, approvals store.ApprovalStore, cfg config.TelegramConfig, timeout time.Duration, log *slog.Logger) *Approver {
	if log == nil {
		log = slog.Default()
	}
	if timeout <= 0 {
		timeout = defaultApprovalTimeout
	}
	return &Approver{
		api:       api,
		approvals: approvals,
		cfg:       cfg,
		timeout:   timeout,
		log:       log,
		waiters:   make(map[string]chan bool),
	}
}

// ExpirePending runs once at gateway startup: pending waiters are
// process-local goroutines, so a restart abandons whatever was pending and
// this sweep is what stops those rows from sitting "pending" forever.
func (a *Approver) ExpirePending(ctx context.Context) error {
	n, err := a.approvals.ExpirePending(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("telegram: expire pending approvals at startup: %w", err)
	}
	if n > 0 {
		a.log.Info("telegram: expired stale pending approvals from a prior run", "count", n)
	}
	return nil
}

// Ask sends an inline-button approval prompt and blocks until a decision, a
// timeout, or ctx ending - whichever comes first. See tools.Approver for
// the exact error-vs-bool contract this must honor.
func (a *Approver) Ask(ctx context.Context, req tools.Request) (bool, error) {
	chatIDNum, err := strconv.ParseInt(req.ChatID, 10, 64)
	if err != nil {
		return false, fmt.Errorf("telegram: invalid chat id %q: %w", req.ChatID, err)
	}

	id, err := newApprovalID()
	if err != nil {
		return false, fmt.Errorf("telegram: generate approval id: %w", err)
	}

	wait := make(chan bool, 1)
	a.mu.Lock()
	a.waiters[id] = wait
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.waiters, id)
		a.mu.Unlock()
	}()

	// Create the approvals row before anything is sent: a fast tap racing a
	// message that exists but has no row yet would hit "unknown or expired
	// approval", and a Create failure after the send would leave
	// live-looking buttons nothing could ever answer. MessageID is filled in
	// after the send below, once it is known.
	expiresAt := time.Now().Add(a.timeout)
	ap := &store.Approval{
		ID:        id,
		SessionID: req.SessionID,
		Channel:   req.Channel,
		ChatID:    req.ChatID,
		Tool:      req.Tool,
		Command:   req.Command,
		Reason:    req.Reason,
		ExpiresAt: expiresAt,
	}
	if err := a.approvals.Create(ctx, ap); err != nil {
		return false, fmt.Errorf("telegram: create approval row: %w", err)
	}

	kb := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("\u2705 Approve").WithCallbackData("ok:"+id),
		tu.InlineKeyboardButton("\U0001F6AB Deny").WithCallbackData("no:"+id),
	))
	text := formatApprovalPrompt(req)
	params := tu.Message(tu.ID(chatIDNum), EscapeMarkdownV2(text)).
		WithParseMode(telego.ModeMarkdownV2).
		WithReplyMarkup(kb)
	if tid := parseThreadID(req.ThreadID); tid != 0 {
		params = params.WithMessageThreadID(tid)
	}
	if mid, err := strconv.Atoi(req.MessageID); err == nil && mid != 0 {
		// Quote the message that triggered this approval so it is never an
		// unanchored prompt in a busy chat - the phase 6 spec's own
		// requirement, unblocked once Request carries a message id (phase 7).
		params = params.WithReplyParameters(&telego.ReplyParameters{MessageID: mid})
	}

	msg, err := sendOne(ctx, a.api, params, text)
	if err != nil {
		// The row already exists with nothing to edit; mark it decided now
		// instead of leaving it pending with no message that could ever
		// answer it.
		if derr := a.approvals.Decide(context.WithoutCancel(ctx), id, "expired", ""); derr != nil && !errors.Is(derr, store.ErrAlreadyDecided) {
			a.log.Error("telegram: mark approval expired after send failure failed", "approval_id", id, "error", derr)
		}
		return false, fmt.Errorf("telegram: send approval prompt: %w", err)
	}

	if err := a.approvals.SetMessageID(ctx, id, strconv.Itoa(msg.MessageID)); err != nil {
		a.log.Error("telegram: record approval message id failed", "approval_id", id, "error", err)
	}

	timer := time.NewTimer(a.timeout)
	defer timer.Stop()

	return a.awaitDecision(ctx, wait, timer, id, req.ChatID, msg.MessageID)
}

// awaitDecision blocks until wait delivers a decision, timer fires, or ctx
// ends - whichever comes first - and is the exact select Ask's own doc
// comment describes. Split out from Ask so the timer/wait race (a callback
// landing at the same instant the timer fires) can be driven directly in
// tests with a synthetic wait/timer pair instead of racing real goroutines
// against real time.
func (a *Approver) awaitDecision(ctx context.Context, wait <-chan bool, timer *time.Timer, id, chatID string, messageID int) (bool, error) {
	select {
	case approved := <-wait:
		return approved, nil

	case <-timer.C:
		// A callback's Decide can land at the exact instant the timer
		// fires (Go's select picks randomly between two ready cases) or in
		// the small window between the timer firing and this goroutine
		// winning the scheduler - drain wait non-blocking before accepting
		// the timeout, so a decision that actually landed is not thrown
		// away in favor of a contradictory "timed out".
		select {
		case approved := <-wait:
			return approved, nil
		default:
		}
		if raced, approved := a.finishExpiredOrRace(context.WithoutCancel(ctx), id, chatID, messageID, "timed out waiting for a response"); raced {
			return approved, nil
		}
		return false, context.DeadlineExceeded

	case <-ctx.Done():
		// A SIGTERM or turn cancellation while buttons sit unanswered must
		// not block phase 7's shutdown drain for the full approval_timeout:
		// this case is why ctx is a select arm here, not garnish.
		if raced, approved := a.finishExpiredOrRace(context.WithoutCancel(ctx), id, chatID, messageID, "the gateway is shutting down"); raced {
			return approved, nil
		}
		return false, ctx.Err()
	}
}

// finishExpiredOrRace marks id expired (see finishExpired) and, if it lost
// the race to a decision a callback had already committed, reads the
// approvals row back and reports the actual verdict instead - so a timer or
// ctx.Done() firing in the narrow window between Decide committing and the
// callback's own push to wait does not report a contradictory timeout (or
// cancellation) while the DB, and the message the user already sees, say
// "approved"/"denied". raced is false - meaning the caller should report
// its own timeout/cancellation error as before - both when finishExpired
// won the race (genuinely expired) and when the follow-up read itself
// fails.
func (a *Approver) finishExpiredOrRace(ctx context.Context, id, chatID string, messageID int, note string) (raced, approved bool) {
	if !a.finishExpired(ctx, id, chatID, messageID, note) {
		return false, false
	}
	ap, err := a.approvals.Get(ctx, id)
	if err != nil {
		a.log.Error("telegram: load approval after losing the timeout race failed", "approval_id", id, "error", err)
		return false, false
	}
	return true, ap.State == "approved"
}

// finishExpired marks id expired and edits its message to explain why,
// removing the keyboard, reporting whether it lost the race to a callback
// that had already committed a decision (Decide returns ErrAlreadyDecided).
// Both the Decide call and the edit are otherwise best-effort: a failure
// here must not itself change the (already decided) outcome Ask returns. If
// the approval was already decided by a callback that won the race, the
// message already shows that outcome - overwriting it with "timed out"
// would contradict a decision that actually landed, so the edit is skipped
// entirely.
func (a *Approver) finishExpired(ctx context.Context, id, chatID string, messageID int, note string) (alreadyDecided bool) {
	err := a.approvals.Decide(ctx, id, "expired", "")
	if err != nil {
		if errors.Is(err, store.ErrAlreadyDecided) {
			return true
		}
		a.log.Error("telegram: mark approval expired failed", "approval_id", id, "error", err)
	}
	a.editOutcome(ctx, chatID, messageID, "\u23F1 "+note)
	return false
}

// HandleCallback processes one CallbackQuery arriving from an inline
// approve/deny button. Both authorization checks - the presser passes the
// channel allowlist, and the press came from the chat the approval was
// created in - exist so that visible-to-everyone inline buttons in a group
// cannot become another user's shell access. Every path ends by answering
// the callback query (required, or the button spins forever on the
// client).
func (a *Approver) HandleCallback(ctx context.Context, cb *telego.CallbackQuery) {
	verdict, id, ok := parseCallbackData(cb.Data)
	if !ok {
		a.answer(ctx, cb.ID, "")
		return
	}

	if cb.Message == nil {
		// No message context (e.g. a callback from an inline-mode result):
		// there is nothing here to authorize against or edit.
		a.answer(ctx, cb.ID, "")
		return
	}

	ap, err := a.approvals.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.answer(ctx, cb.ID, "unknown or expired approval")
			return
		}
		a.log.Error("telegram: load approval for callback failed", "approval_id", id, "error", err)
		a.answer(ctx, cb.ID, "internal error")
		return
	}

	chat := cb.Message.GetChat()
	chatID := strconv.FormatInt(chat.ID, 10)
	if ap.ChatID != chatID {
		// Must match the chat this approval was created in: otherwise a
		// user could tap a stale button ID copied from elsewhere.
		a.answer(ctx, cb.ID, "this approval belongs to a different chat")
		return
	}

	if !a.authorized(chat, cb.From.ID) {
		// The presser must themselves pass the same allowlist that gated
		// the original request into this chat - inline buttons are visible
		// to everyone in a group, so this is what stops group membership
		// from becoming shell access for someone else's command.
		a.answer(ctx, cb.ID, "you are not allowed to decide this")
		return
	}

	state := "denied"
	label := "denied"
	if verdict {
		state = "approved"
		label = "approved"
	}
	decidedBy := strconv.FormatInt(cb.From.ID, 10)

	if err := a.approvals.Decide(ctx, id, state, decidedBy); err != nil {
		if errors.Is(err, store.ErrAlreadyDecided) {
			a.answer(ctx, cb.ID, "already decided")
			return
		}
		a.log.Error("telegram: decide approval failed", "approval_id", id, "error", err)
		a.answer(ctx, cb.ID, "internal error")
		return
	}

	a.mu.Lock()
	wait, ok := a.waiters[id]
	a.mu.Unlock()

	if !ok {
		// Decide committed the verdict, but nobody in this process is
		// blocked on it - typically a pending row surviving a restart,
		// tapped before the startup ExpirePending sweep caught it, or Ask
		// having already returned for some other reason. Whatever tool call
		// this was gating belongs to a process that no longer exists to run
		// it, so "approved by user N" would be misleading; say so instead.
		a.editOutcome(ctx, chatID, cb.Message.GetMessageID(), "\u2753 this approval is no longer waiting for a response")
		a.answer(ctx, cb.ID, "")
		return
	}

	select {
	case wait <- verdict:
	default:
	}

	icon := "\u2705"
	if !verdict {
		icon = "\U0001F6AB"
	}
	a.editOutcome(ctx, chatID, cb.Message.GetMessageID(), icon+" "+label+" by user "+decidedBy)
	a.answer(ctx, cb.ID, "")
}

// authorized reports whether userID passes the same allowlist that would
// have gated an inbound message from chat: the channel allowlist for a
// private chat, or the matching group's allow_from (falling back to the
// channel allowlist when the group's own is empty) for a group/supergroup.
func (a *Approver) authorized(chat telego.Chat, userID int64) bool {
	if chat.Type == "private" {
		return containsID(a.cfg.AllowFrom, userID)
	}
	group, ok := lookupGroup(a.cfg.Groups, chat.ID)
	if !ok {
		return false
	}
	allow := group.AllowFrom
	if len(allow) == 0 {
		allow = a.cfg.AllowFrom
	}
	return containsID(allow, userID)
}

func (a *Approver) answer(ctx context.Context, callbackQueryID, text string) {
	if err := a.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: callbackQueryID, Text: text}); err != nil {
		a.log.Error("telegram: answer callback query failed", "error", err)
	}
}

// editOutcome rewrites the prompt message to text and removes its inline
// keyboard in one call.
func (a *Approver) editOutcome(ctx context.Context, chatID string, messageID int, text string) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		a.log.Error("telegram: edit outcome: invalid chat id", "chat_id", chatID, "error", err)
		return
	}
	params := &telego.EditMessageTextParams{
		ChatID:      tu.ID(id),
		MessageID:   messageID,
		Text:        EscapeMarkdownV2(text),
		ParseMode:   telego.ModeMarkdownV2,
		ReplyMarkup: &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{}},
	}
	if _, err := a.api.EditMessageText(ctx, params); err != nil {
		var apiErr *ta.Error
		if errors.As(err, &apiErr) && apiErr.ErrorCode == http.StatusBadRequest {
			params.ParseMode = ""
			params.Text = text
			if _, err2 := a.api.EditMessageText(ctx, params); err2 != nil {
				a.log.Error("telegram: edit approval outcome failed", "error", err2)
			}
			return
		}
		a.log.Error("telegram: edit approval outcome failed", "error", err)
	}
}

// parseCallbackData splits "ok:<id>" / "no:<id>" callback_data into a
// verdict and the approval id.
func parseCallbackData(data string) (verdict bool, id string, ok bool) {
	switch {
	case strings.HasPrefix(data, "ok:"):
		return true, strings.TrimPrefix(data, "ok:"), true
	case strings.HasPrefix(data, "no:"):
		return false, strings.TrimPrefix(data, "no:"), true
	default:
		return false, "", false
	}
}

// formatApprovalPrompt renders one Request as the Telegram message text:
// the command in a code block plus the classifier's reason, if any.
func formatApprovalPrompt(req tools.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Approval requested for %s:\n```\n%s\n```", req.Tool, req.Command)
	if req.Reason != "" {
		fmt.Fprintf(&b, "\nreason: %s", req.Reason)
	}
	return b.String()
}

// newApprovalID generates a random 128-bit nonce, hex-encoded, used both as
// the approvals row id and as the inline buttons' callback_data payload.
func newApprovalID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
