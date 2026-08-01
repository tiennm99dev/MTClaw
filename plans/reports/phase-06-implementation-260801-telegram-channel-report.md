# Phase 6 Implementation Report: Telegram Channel

- Plan: `plans/260731-2219-mtclaw-core-system/`
- Phase file: `plans/260731-2219-mtclaw-core-system/phase-06-telegram-channel.md`
- Status: **completed**

## Files created

- `internal/channel/channel.go` — `Channel` interface (`Name`, `Start`, `Send`) and `Inbound`.
- `internal/channel/telegram/channel.go` — `New`, `SendOnce`, `Channel` struct, `Start` lifecycle (getMe, registerCommands, ExpirePending, long-poll), `Send`, `SendTyping`, `Approver()` accessor.
- `internal/channel/telegram/poll.go` — `pumpUpdates` (dispatch `CallbackQuery`/`Message`), `handleMessage` (gate, intercept known commands, else forward to `out`).
- `internal/channel/telegram/gating.go` — `Decide` (pure), `containsID`, `lookupGroup`, `detectMention`, `utf16Slice`.
- `internal/channel/telegram/send.go` — `sendText`, `sendOne` (429 retry_after honoring, 400-parse fallback), `sleepCtx`, `parseThreadID`, `sendTyping`.
- `internal/channel/telegram/chunk.go` — `Split`, segment parser (fence-aware), `splitPlain`, `splitFence`, boundary helpers.
- `internal/channel/telegram/format.go` — `EscapeMarkdownV2`.
- `internal/channel/telegram/commands.go` — `Deps`, `SessionStatus`, `commandName`, `registerCommands`, `handleCommand`, `helpText`.
- `internal/channel/telegram/approver.go` — `Approver` (`tools.Approver` impl), `ExpirePending`, `Ask`, `HandleCallback`, `authorized`, prompt/outcome formatting.
- `internal/channel/telegram/api.go` — `botAPI` interface (the seam tests stub instead of `*telego.Bot`).
- Tests: `gating_test.go`, `chunk_test.go`, `format_test.go`, `approver_test.go` (also covers `sendOne`/`sendText` thread routing and fallback, since those live in the same package).
- `internal/cli/send_cmd.go` — `mtclaw send --chat <id> [--thread <id>] "<text>"`.

## Files modified

- `internal/cli/root.go` — registered `newSendCmd(s)` (one line; not in the phase file's explicit list, but `send_cmd.go` is dead code without it — every other CLI subcommand file is registered the same way).
- `go.mod` / `go.sum` — added `github.com/mymmrac/telego v1.11.1` and its transitive deps (`go mod tidy`). `go.mod`'s `go` directive was bumped `1.25.0` → `1.25.7` automatically: telego v1.11.1 itself declares `go 1.25.7`. Toolchain in this environment is 1.26.4, so this is a no-op in practice, just noting it since it wasn't requested standalone.
- `plans/260731-2219-mtclaw-core-system/phase-06-telegram-channel.md` — frontmatter `status: pending` → `completed`.
- `plans/260731-2219-mtclaw-core-system/plan.md` — phase 6 row `Pending` → `Completed`.

## API divergences from the phase file's sketch

Verified against the installed `github.com/mymmrac/telego@v1.11.1` source (not just its README):

- `UpdatesViaLongPolling` signature, `WithLongPollingUpdateInterval/RetryTimeout/Buffer`, and `tu.InlineKeyboard`/`InlineKeyboardRow`/`InlineKeyboardButton().WithCallbackData` all match the sketch exactly.
- `WithDefaultDebugLogger` exists and is never called. Also never called: `WithDefaultLogger`/`WithExtendedDefaultLogger` (both still print to `os.Stderr` directly, bypassing the app's slog, even though they redact the token via a `strings.Replacer`). Used `telego.WithDiscardLogger()` instead — no telego-internal logging at all, so "the token never appears in any log line" is true by construction rather than by trusting a redaction replacer.
- `CallbackQuery.Message` is `MaybeInaccessibleMessage` (an interface: `*Message` or `*InaccessibleMessage`), not `*telego.Message` directly. Guarded for `nil` and used via `.GetChat()`/`.GetMessageID()`.
- `MessageEntity.Offset`/`.Length` are UTF-16 code units, not bytes/runes — `utf16Slice` (gating.go) converts explicitly rather than doing a byte slice.
- Telegram API errors surface as `*github.com/mymmrac/telego/telegoapi.Error` wrapped via `fmt.Errorf("api: %w", …)`; `errors.As` unwraps it for the 429/400-parse branches in `send.go`.
- `EditMessageTextParams.ReplyMarkup` has no `omitempty`-safe "clear" shorthand: removing a keyboard requires passing `&telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{}}` explicitly (not `nil`, which would serialize as `"inline_keyboard":null`).

## Design decisions / deviations worth flagging

1. **`tools.Request` has no message id, so an approval prompt cannot literally "quote the triggering message."** `Request` (phase 5, out of scope to change) carries `SessionID/Channel/ChatID/ThreadID/Tool/Command/Reason` only. The approval prompt does carry `ThreadID` correctly (routes to the right forum topic — the actually-testable, actually-required behavior), but there is no message id available to set as `reply_parameters`. Treated the "approval prompts always quote" phrasing as guidance for `Channel.Send`'s `replyTo` parameter in general (which the gateway, phase 7, can use with `Inbound.MessageID` for other purposes), not as something `Approver.Ask` can implement with the data it's given.
2. **`internal/tools/exec.go` (phase 5) already creates its own `store.Approval` row before calling `Approver.Ask`**, and decides it itself afterward via `decideApproval` (with `DecidedBy: "approver"`, a static string). This phase's `Approver` additionally creates and fully owns a *second*, independent approval row (its own random id, its own message id, its own real `decided_by`) per the phase-06 spec's explicit steps ("Create the approvals row... with a random id", "calls `Approvals().Decide(id, state, userID)`"). This means one exec approval produces two rows in the `approvals` table: exec.go's generic bookkeeping row (empty `message_id`, decided post-hoc) and the Telegram-specific row the buttons actually act on. Functionally correct and independently testable, but a `SELECT * FROM approvals` will show 2 rows per prompt. Not something phase 6 can fix without touching phase 5's `exec.go`.
3. Added `internal/channel/telegram/api.go` (not in the phase file's file list) defining `botAPI`, the minimal interface `*telego.Bot` satisfies structurally and tests stub — exactly what the task's testability note asked for, kept to one file.
4. `chunk.go`'s `Split` treats `limit` as a byte-length ceiling (not a UTF-16/rune count), which is always at least as conservative as Telegram's true 4096-character limit for multi-byte UTF-8 input.
5. Bot-command interception (`/start`, `/new`, `/status`, `/whoami`, `/stop`, `/help`) happens in `poll.go` before anything reaches `out`; these never become an agent turn. `Deps` is nil-safe — `/new`/`/status`/`/stop` degrade to an explanatory reply until phase 7 wires a real implementation.
6. `SendTyping` issues one chat action per call (matching `sendChatAction: typing`); the "refresh every 4s while a turn runs" ticker is the caller's (phase 7 gateway's) responsibility, per the phase file's own wording.

## Tests

All in `internal/channel/telegram/*_test.go`, no network, no real bot:

- **Gating**: table test covering every branch of the flowchart (private allow/deny, bot sender, group no-entry/`*`/own-`allow_from`, `require_mention` true/false, mention-by-reply, `/cmd@bot`, bare `/cmd` rejected, unsupported chat type, nil message, positive-ID group key).
- **Chunker**: 4095/4096/4097 boundaries, paragraph/line/sentence preference (each verified to consume its separator rather than leaving it dangling), 10k single word, no-whitespace text, empty string, a fence spanning the limit (every resulting chunk fence-balanced), and a short fence-containing message reconstructing byte-for-byte (proves segments merge back together when they fit, rather than always splitting at fence edges).
- **Formatter**: special-char escaping, inline/fenced code left untouched, no double-escaping, unmatched-backtick handling, empty input.
- **Approver**: approve, deny, double-tap no-op, timeout (marks `expired`, edits message, fast), ctx-cancel-while-pending (returns promptly, marks `expired`), wrong-user callback, wrong-chat callback, unknown id, and thread routing (prompt carries `message_thread_id`).
- **Send fallback**: a fake transport returning a 400-parse error once then success proves the message is delivered exactly once, with the retry attempt using `ParseMode: ""` and the raw text.
- **Thread routing on `Channel.Send`**: `sendText` sets/omits `message_thread_id` correctly.

## Build/quality gates

- `gofmt -l .` — clean.
- `go vet ./...` — clean.
- `go build ./...` and `CGO_ENABLED=0 go build ./...` — clean.
- `go test ./...` — all packages pass (phases 1–5 unaffected, phase 6 new tests green).
- `go mod tidy` — no-op after the `go get`.

## Unresolved questions

- Confirm whether the exec.go-owned generic approval row (see deviation #2) should eventually be reconciled with the channel-owned row in a later phase (e.g. phase 7 or a hardening pass) — flagging it here since it wasn't visible until this phase's `Approver` was implemented against the real `store.ApprovalStore` interface.
- `internal/channel/telegram/api.go` is an addition beyond the phase file's literal file list; flagging for awareness, not asking for a decision — it's minimal and exists solely for the testability seam the task explicitly requested.
