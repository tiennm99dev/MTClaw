// Package fakeapi implements network-free, httptest-backed stand-ins for
// the two upstream HTTP APIs MTClaw talks to (Telegram's Bot API, OpenAI's
// chat-completions endpoint), so internal/gateway's and internal/cli's
// end-to-end suites can drive the *real* telego client and the *real*
// openai-go SDK client against a local server instead of a mock provider or
// a hand-built fake transport. Both fakes work purely in wire format (JSON
// request/response bodies) - that is the whole point of testing through the
// real SDK path rather than swapping in internal/provider/mock. Neither type
// here imports "testing": Telegram's handlers report a malformed test setup
// by writing a Telegram-shaped error response, not by failing a *testing.T,
// so this package is usable from any test file in any package without
// coupling it to one test framework's assertion style.
package fakeapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
)

// telegramFakeToken satisfies telego's own token format check
// (^\d+:[\w-]{35}$}, telego@v1.11.1/bot.go) without naming a real bot: 35
// filler characters after the colon.
const telegramFakeToken = "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// telegramPollWait bounds how long a getUpdates call blocks on an empty
// queue before answering {"ok":true,"result":[]}. internal/channel/telegram
// runs long polling with UpdateInterval: 0 (channel.go), so a fake that
// answers an empty poll instantly makes telego spin as fast as the test
// process's CPU allows - measured during this phase's transport spike at
// four getUpdates calls in roughly 400ms idle against a non-blocking fake.
// This wait is the fix, not a nicety: never lower it to make a test "feel"
// faster.
const telegramPollWait = 250 * time.Millisecond

// telegramFakeBotID/Username are getMe's fixed identity: every fake bot
// looks the same, since no test in this repo depends on the bot's own
// identity being distinguishable from another test's.
const (
	telegramFakeBotID       = 987654321
	telegramFakeBotUsername = "mtclaw_fake_bot"
)

// Call is one recorded API request: the method name telego called (the
// Bot API's URL path's last segment, e.g. "sendMessage") and its request
// body, decoded from JSON. Telegram's JSON numbers decode as float64 (the
// encoding/json default for `any`), so callers comparing a chat or message
// id against Body must convert accordingly - ChatIDFromBody below is the
// shared way to do that.
type Call struct {
	Method string
	Body   map[string]any
}

// Telegram is an httptest-backed fake of the Telegram Bot API surface
// internal/channel/telegram actually calls: getMe, setMyCommands,
// sendMessage, editMessageText, answerCallbackQuery, sendChatAction, and
// getUpdates (via UpdatesViaLongPolling) - see
// internal/channel/telegram/api.go's botAPI interface, which is exactly
// this set. Point a real *telego.Bot at it with
// telego.WithAPIServer(fake.URL()), which is what
// channels.telegram.api_base_url plumbs through to in production code.
type Telegram struct {
	srv *httptest.Server

	mu            sync.Mutex
	queue         []map[string]any // each already carries its own "update_id"
	nextUpdateID  int
	nextMessageID int
	calls         []Call
	failNextSend  *telegramFailSpec
	wake          chan struct{} // closed and replaced on every Push, to wake a blocked getUpdates
}

// telegramFailSpec is FailNextSendMessage's one-shot script.
type telegramFailSpec struct {
	code int
	desc string
}

// NewTelegram starts the fake server. Callers must Close it once done,
// typically via t.Cleanup.
func NewTelegram() *Telegram {
	s := &Telegram{wake: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL returns the fake server's base URL - the value for
// channels.telegram.api_base_url.
func (s *Telegram) URL() string { return s.srv.URL }

// Token returns a bot token shaped to satisfy telego's own token-format
// validation. It is never compared against anything: the fake accepts every
// request regardless of which token the URL names, matching how
// api_base_url is meant to be used (pointed at a server that is not the
// real Telegram, where a "real" token has no meaning anyway).
func (s *Telegram) Token() string { return telegramFakeToken }

// Close shuts the fake server down.
func (s *Telegram) Close() { s.srv.Close() }

// Push enqueues an inbound update, overwriting u.UpdateID with the fake's
// own sequential counter so offset-based long polling behaves exactly like
// the real API regardless of what the caller set.
func (s *Telegram) Push(u telego.Update) {
	raw, err := json.Marshal(u)
	if err != nil {
		// telego.Update marshals unconditionally (plain fields and typed
		// pointers only, no channels or funcs); reaching this is a bug in
		// this fake, not in the caller, so panic loudly instead of silently
		// dropping the update.
		panic(fmt.Sprintf("fakeapi: marshal pushed update: %v", err))
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		panic(fmt.Sprintf("fakeapi: decode pushed update: %v", err))
	}
	s.pushLocked(body)
}

// PushMessage enqueues a plain inbound text message from a private chat -
// the shape internal/channel/telegram/gating.go's Decide expects for a DM.
func (s *Telegram) PushMessage(chatID, userID int64, text string) {
	s.Push(telego.Update{
		Message: &telego.Message{
			MessageID: s.nextMessageIDLocked(),
			Date:      time.Now().Unix(),
			Chat:      telego.Chat{ID: chatID, Type: "private"},
			From:      &telego.User{ID: userID, FirstName: "Test User"},
			Text:      text,
		},
	})
}

// PushCallback enqueues an inline-button press: a CallbackQuery whose
// Message is the prompt message replyToMessageID (the message that carried
// the buttons - see internal/channel/telegram/approver.go's Ask, which
// records that id via SetMessageID). It is built directly in wire-format
// terms rather than through a telego.Update value because
// telego.CallbackQuery.Message is typed as the MaybeInaccessibleMessage
// interface, whose implementations are unexported - only telego's own JSON
// decoder can construct one, which is exactly what happens once this map
// round-trips through getUpdates' response body and back into the real
// telego client.
func (s *Telegram) PushCallback(chatID, userID int64, data string, replyToMessageID int) {
	s.mu.Lock()
	id := s.nextUpdateID + 1
	s.mu.Unlock()

	s.pushLocked(map[string]any{
		"callback_query": map[string]any{
			"id":   fmt.Sprintf("cbq-%d", id),
			"from": map[string]any{"id": userID, "is_bot": false, "first_name": "Test User"},
			"message": map[string]any{
				"message_id": replyToMessageID,
				"date":       time.Now().Unix(),
				"chat":       map[string]any{"id": chatID, "type": "private"},
			},
			"chat_instance": fmt.Sprintf("%d", chatID),
			"data":          data,
		},
	})
}

// pushLocked assigns the next update_id to body and appends it to the
// queue, waking any getUpdates call currently blocked waiting for one.
// update_id is stored as float64, not int, so it has the exact same
// dynamic type numericField later reads back - encoding/json decodes every
// JSON number into a float64 when the target is `any` (the shape a real
// request body arrives in), and pendingLocked's offset comparison must see
// that same type for every queued update, including ones (like
// PushCallback's) that were never round-tripped through json.Unmarshal at
// all.
func (s *Telegram) pushLocked(body map[string]any) {
	s.mu.Lock()
	s.nextUpdateID++
	body["update_id"] = float64(s.nextUpdateID)
	s.queue = append(s.queue, body)
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

func (s *Telegram) nextMessageIDLocked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextMessageID++
	return s.nextMessageID
}

// FailNextSendMessage arms a one-shot failure: the next sendMessage call
// (and only that one) answers {"ok":false,"error_code":code,"description":desc}
// instead of the usual echoed Message. It exists to exercise the
// MarkdownV2-to-plain-text fallback in internal/channel/telegram/send.go,
// which only triggers on an HTTP 400 whose description contains "parse".
func (s *Telegram) FailNextSendMessage(code int, desc string) {
	s.mu.Lock()
	s.failNextSend = &telegramFailSpec{code: code, desc: desc}
	s.mu.Unlock()
}

// Calls returns every request received so far, in arrival order.
func (s *Telegram) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	copy(out, s.calls)
	return out
}

// SentTexts returns the text of every sendMessage call addressed to chatID,
// in send order - the shape a chunked, multi-message reply naturally takes.
func (s *Telegram) SentTexts(chatID int64) []string {
	var out []string
	for _, c := range s.Calls() {
		if c.Method != "sendMessage" {
			continue
		}
		if ChatIDFromBody(c.Body) != chatID {
			continue
		}
		if text, ok := c.Body["text"].(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// ChatIDFromBody extracts a numeric chat_id from a decoded request body,
// tolerating the float64 encoding/json produces for JSON numbers decoded
// into `any`. Returns 0 if chat_id is absent or not numeric.
func ChatIDFromBody(body map[string]any) int64 {
	switch v := body["chat_id"].(type) {
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// handle routes every request by the last path segment of its URL -
// "<apiURL>/bot<token>/<method>" (telego@v1.11.1/bot.go) - decodes the JSON
// request body telego always sends (see telegoapi.DefaultConstructor's
// JSONRequest), records the Call, and dispatches to the matching per-method
// responder. Every response is written with HTTP 200: telego derives
// success/failure solely from the JSON body's "ok" field
// (telegoapi.Response, telegoapi/api.go), and its FastHTTPCaller treats any
// >=500 status as a hard transport error - so a non-2xx here would either
// be ignored (a genuine Telegram-style error, which must still be a 2xx to
// be decoded at all) or, worse, misread as a transport failure that feeds
// the 8s WithLongPollingRetryTimeout stall this fake exists to avoid.
func (s *Telegram) handle(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body) // getMe/getUpdates may send an empty or trivial body

	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Body: body})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	switch method {
	case "getMe":
		writeResult(w, map[string]any{
			"id": telegramFakeBotID, "is_bot": true,
			"first_name": "MTClaw Fake Bot", "username": telegramFakeBotUsername,
		})
	case "getUpdates":
		s.handleGetUpdates(w, body)
	case "sendMessage":
		s.handleSendMessage(w, body)
	case "editMessageText":
		writeResult(w, echoedMessage(body))
	case "answerCallbackQuery", "setMyCommands", "sendChatAction":
		writeResult(w, true)
	default:
		writeError(w, http.StatusBadRequest, "unknown method "+method)
	}
}

// handleGetUpdates honors offset, blocking up to telegramPollWait for a new
// update to arrive if none is pending yet - see telegramPollWait's doc
// comment for why a non-blocking answer is not an acceptable shortcut.
func (s *Telegram) handleGetUpdates(w http.ResponseWriter, body map[string]any) {
	offset := int(numericField(body, "offset"))

	s.mu.Lock()
	pending := s.pendingLocked(offset)
	wake := s.wake
	s.mu.Unlock()

	if len(pending) == 0 {
		select {
		case <-wake:
		case <-time.After(telegramPollWait):
		}
		s.mu.Lock()
		pending = s.pendingLocked(offset)
		s.mu.Unlock()
	}

	writeResult(w, pending)
}

// pendingLocked returns every queued update whose update_id is >= offset.
// Callers must hold s.mu.
func (s *Telegram) pendingLocked(offset int) []map[string]any {
	var out []map[string]any
	for _, u := range s.queue {
		if int(numericField(u, "update_id")) >= offset {
			out = append(out, u)
		}
	}
	return out
}

// handleSendMessage answers the armed FailNextSendMessage script exactly
// once if one is set, otherwise assigns the next message_id and echoes a
// plausible Message built from the request body's chat_id/text.
func (s *Telegram) handleSendMessage(w http.ResponseWriter, body map[string]any) {
	s.mu.Lock()
	fail := s.failNextSend
	s.failNextSend = nil
	s.mu.Unlock()

	if fail != nil {
		writeError(w, fail.code, fail.desc)
		return
	}

	writeResult(w, map[string]any{
		"message_id": s.nextMessageIDLocked(),
		"date":       time.Now().Unix(),
		"chat":       map[string]any{"id": ChatIDFromBody(body), "type": "private"},
		"text":       body["text"],
	})
}

// echoedMessage builds editMessageText's response Message from the request
// body: same chat and message id, updated text.
func echoedMessage(body map[string]any) map[string]any {
	return map[string]any{
		"message_id": numericField(body, "message_id"),
		"date":       time.Now().Unix(),
		"chat":       map[string]any{"id": ChatIDFromBody(body), "type": "private"},
		"text":       body["text"],
	}
}

// numericField reads a float64-decoded JSON number field, defaulting to 0
// for anything absent or non-numeric.
func numericField(body map[string]any, key string) float64 {
	v, _ := body[key].(float64)
	return v
}

// writeResult writes {"ok":true,"result":result}.
func writeResult(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// writeError writes a Telegram-shaped error body. Per handle's doc comment,
// this is still sent with HTTP 200 - telego reads error_code/description
// from the JSON body, not the transport status.
func writeError(w http.ResponseWriter, code int, description string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": code, "description": description})
}
