# Phase 1: E2E Verification Harness

**Status:** Completed · **Effort:** 6h · **Blocks:** phase 2 · **Blocked by:** none

Close v1's four unchecked acceptance criteria as far as is possible with no
OpenAI key and no Telegram bot token, by driving the *real* gateway and the
*real* CLI against `httptest` fakes of both upstream APIs.

## Context

- v1 plan, unchecked criteria: `plans/260731-2219-mtclaw-core-system/plan.md:194,196,203` and its "Remaining manual verification" section (`:208-226`).
- The two hardcoded transports that force this design:
  - `internal/gateway/gateway.go:97` calls `telegram.New`, which calls `telego.NewBot(token, telego.WithDiscardLogger())` at `internal/channel/telegram/channel.go:61` — no seam for an alternate API host.
  - `internal/gateway/gateway.go:74` calls `openai.New(cfg.OpenAI)`, but `internal/provider/openai/client.go:46-48` already honours `cfg.BaseURL`, so **the OpenAI side needs no new seam.**
- Secrets are unreachable from a hand-built `config.Config`: `apiKey`/`token`
  are unexported and written only by `setSecret`
  (`internal/config/types.go:100-103,133-136`). Tests must go through
  `config.Load(data, baseDir, env)` — the pattern already used at
  `internal/provider/openai/e2e_test.go:33-45`.
- Upstream facts verified in the module cache:
  - `telego.WithAPIServer(apiURL)` — `telego@v1.11.1/bot_options.go:110-121`.
  - Request URL shape `<apiURL>/bot<token>/<method>` — `telego@v1.11.1/bot.go:238-240`.
  - Token must match `^\d+:[\w-]{35}$` — `telego@v1.11.1/bot.go:26,40-43`. Use `123456789:` + 35 `A`s.
  - Default caller is `FastHTTPCaller` — `telego@v1.11.1/bot.go:106`.
- Methods our code actually calls (the fake must serve exactly these):
  `internal/channel/telegram/api.go:15-23` — `getMe`, `setMyCommands`,
  `sendMessage`, `editMessageText`, `answerCallbackQuery`, `sendChatAction`,
  `getUpdates` (via `UpdatesViaLongPolling`).
- Long-poll parameters in use: `Timeout: 30`, `UpdateInterval: 0`,
  `RetryTimeout: 8s`, `Buffer: 100` — `internal/channel/telegram/channel.go:150-154`.
- Approval callback payload is `"ok:"+id` / `"no:"+id` —
  `internal/channel/telegram/approver.go:123-126`; the handler requires
  `cb.Message` with a matching chat — `approver.go:168-186`.
- Existing helpers to reuse, not reinvent: fake HOME
  `internal/cli/onboard_test.go:27-36`; poll-until-true `waitCond`
  `internal/gateway/gateway_test_helpers_test.go:98-110`; discard logger
  `gateway_test_helpers_test.go:43-45`; a shell-argv-per-OS exec test config —
  see `internal/tools/exec_test.go`.
- Cron: `Scheduler.Start` sleeps to the next wall-clock minute
  (`internal/cron/scheduler.go:111-134`, `alignDelay` at `:139-142`) and `now`
  is injectable via `cron.New(..., now func() time.Time)`
  (`internal/cron/scheduler.go:62-68`).

## Requirements

1. A reusable, network-free fake of the Telegram Bot API and of the OpenAI
   chat-completions endpoint, usable from more than one test package.
2. `channels.telegram.api_base_url` config key so the real channel (and thus
   the real gateway) can be pointed at the fake. Documented in the same phase.
3. Gateway e2e coverage: DM round trip with a tool call, chunked reply,
   approval Approve, approval Deny, cron delivery.
4. CLI e2e coverage: `mtclaw prompt` completes a tool-using turn, and history
   survives across two separate command invocations.
5. Hermetic: no env credentials, no network, no writes to the real `~/.mtclaw`,
   green under `-race` on Windows and Linux.
6. Nothing ticked that a test does not assert. Whatever remains manual is
   phase 4's checklist, not a claim here.

## Files

**Create**

| Path | Purpose |
|---|---|
| `internal/testsupport/homedir.go` | `FakeHome(tb TB) string` — points `HOME`/`USERPROFILE` at a temp dir. Declares its own minimal `TB interface { Helper(); TempDir() string; Setenv(k, v string) }` so this package never imports `testing`. |
| `internal/testsupport/fakeapi/telegram.go` | `Telegram` httptest server: update queue, request recorder, per-method handlers. Returns errors, never takes a `*testing.T`. |
| `internal/testsupport/fakeapi/openai.go` | `OpenAI` httptest server: scripted `/chat/completions` responses + recorded request bodies. |
| `internal/testsupport/fakeapi/telegram_spike_test.go` | Step 1's spike, kept as a permanent regression test of the transport assumption. |
| `internal/gateway/e2e_test.go` | `package gateway`. DM round trip, chunking, approval approve/deny, cron delivery. In-package because it reaches `gw.disp` and `gw.cronSched`. |
| `internal/cli/prompt_e2e_test.go` | `package cli`. Real command tree via `newRootCmd`. |

**Modify**

| Path | Change |
|---|---|
| `internal/config/types.go` | `TelegramConfig`: add `APIBaseURL string \`yaml:"api_base_url"\`` (place it next to `TokenFile`, above the unexported secret fields). |
| `internal/config/defaults.go` | Leave `APIBaseURL` empty in `Default()` (`:29-37`); empty means `https://api.telegram.org`. |
| `internal/config/validate.go` | In `validateTelegram` (`:101-139`): when `APIBaseURL != ""`, require an absolute `http`/`https` URL — mirror `validateOpenAI`'s `url.Parse` check at `:92-95`. |
| `internal/channel/telegram/channel.go` | Add unexported `newBot(token, apiBaseURL string) (*telego.Bot, error)` applying `WithDiscardLogger()` plus `WithAPIServer` when non-empty. Use it in `New` (`:61`), `SendOnce` (`:85`), `GetMe` (`:100`). Add `apiBaseURL string` params to `SendOnce` and `GetMe`. |
| `internal/channel/telegram/capture.go` | Same treatment for `CaptureSenders` (`:28`) — add an `apiBaseURL` param. |
| `internal/cli/send_cmd.go:32` | Pass `s.cfg.Channels.Telegram.APIBaseURL`. |
| `internal/cli/cron_cmd.go:236` | Same. |
| `internal/cli/doctor_checks.go:238` | Pass `tg.APIBaseURL`. |
| `internal/cli/onboard_prompts.go:132,136` | Pass `""` — onboard runs before a config exists; document that inline. |
| `internal/cli/onboard_test.go:100-106` | `fakeTelegramCapturer.GetMe`/`Capture` gain the extra parameter. |
| `docs/configuration.md` | New row under `channels.telegram` for `api_base_url`, stating it is a token-trust boundary. Required by `internal/config/docs_coverage_test.go:65`. |

**Delete** — none.

## Implementation steps

1. **Spike the transport assumption first (30 min, gate for the rest).**
   Write `telegram_spike_test.go`: stand up an `httptest.Server` that answers
   `/bot<token>/getMe` with `{"ok":true,"result":{"id":1,"is_bot":true,"username":"fake","first_name":"Fake"}}`
   and `/bot<token>/getUpdates` with one message update, then
   `telego.NewBot(fakeToken, telego.WithDiscardLogger(), telego.WithAPIServer(srv.URL))`,
   call `GetMe`, and read one update from `UpdatesViaLongPolling`. Run it on
   Windows with `-race`.
   - If fasthttp misbehaves, retry with `telego.WithHTTPClient(srv.Client())`.
   - If both fail, **stop and report** — the phase design changes (fall back to
     a `botAPI`-level fake, forfeiting real-transport coverage). Do not
     improvise past this.
2. **`internal/testsupport/homedir.go`.** Move the body of
   `internal/cli/onboard_test.go:27-36` here as `FakeHome(tb TB) string`, and
   make `useFakeHome` in `onboard_test.go` a one-line call so there is exactly
   one copy. Document why `TB` is hand-rolled (no `testing` import in a
   non-test package) and that `Setenv` forbids `t.Parallel`.
3. **`fakeapi.Telegram`.** Shape:
   ```go
   type Telegram struct { /* httptest.Server, mu, queue []telego.Update, calls []Call, nextMessageID int */ }
   func NewTelegram() *Telegram
   func (s *Telegram) URL() string
   func (s *Telegram) Token() string            // a regex-valid fake token
   func (s *Telegram) Push(u telego.Update)     // enqueue an inbound update
   func (s *Telegram) PushMessage(chatID, userID int64, text string) // convenience
   func (s *Telegram) PushCallback(chatID, userID int64, data string, replyToMessageID int)
   func (s *Telegram) Calls() []Call            // snapshot: method + decoded JSON body
   func (s *Telegram) SentTexts(chatID int64) []string
   func (s *Telegram) Close()
   ```
   Handler rules that matter:
   - Route on the last path segment after `/bot<token>/`; unknown method →
     `{"ok":false,"error_code":400,"description":"unknown method <name>"}`.
     Never a non-2xx transport error, and never a 5xx: `RetryTimeout: 8s`
     (`channel.go:152`) would turn one glitch into an 8-second stall.
   - `getUpdates`: honour `offset`; if nothing is queued, **block up to 250ms**
     on a condition/channel before returning `{"ok":true,"result":[]}`. Without
     that wait `telego` spins (`UpdateInterval: 0`).
   - `sendMessage`: assign an incrementing `message_id`, echo a plausible
     `Message` (`message_id`, `date`, `chat{id,type}`, `text`), record the full
     decoded body (so tests can assert `parse_mode`, `reply_markup`,
     `message_thread_id`, `reply_parameters`).
   - `answerCallbackQuery` → `{"ok":true,"result":true}`;
     `setMyCommands` → same; `sendChatAction` → same;
     `editMessageText` → an echoed `Message`.
   - Optional `FailNextSendMessage(code int, description string)` hook so the
     MarkdownV2→plain fallback at `internal/channel/telegram/send.go:89-95` can
     be exercised (return 400 with a description containing "parse").
4. **`fakeapi.OpenAI`.** Shape:
   ```go
   type Step struct { Content string; ToolCalls []ToolCall; Usage Usage }
   func NewOpenAI(steps ...Step) *OpenAI
   func (s *OpenAI) BaseURL() string          // srv.URL + "/v1"
   func (s *OpenAI) Requests() []map[string]any // decoded request bodies, in order
   ```
   Handler: match any path ending `/chat/completions`, return the next step as
   a full chat-completion object (`id`, `object`, `created`, `model`,
   `choices[0].message{role,content,tool_calls}`,
   `choices[0].finish_reason` = `tool_calls` when tool calls are present else
   `stop`, `usage`). Running out of steps → HTTP 500 with a body naming the
   test-authoring bug, so a mis-scripted test fails loudly instead of hanging.
   Do **not** import `internal/provider` — keep the fake in wire-format terms,
   which is the whole point of using the real SDK path.
5. **`api_base_url` plumbing.** Make the config/validate/docs edits and the
   six `telego.NewBot` call-site edits from the Files table. Confirm
   `go test ./internal/config/... ./internal/cli/...` is green before writing
   any e2e test.
6. **Gateway e2e: helper.** In `internal/gateway/e2e_test.go` write
   `newE2EGateway(t, opts)` that: calls `testsupport.FakeHome(t)`; starts both
   fakes; renders a YAML config as a string with
   `channels.telegram.{enabled: true, token_env: E2E_TG_TOKEN, api_base_url: <fake>, allow_from: [<userID>]}`,
   `openai.base_url: <fake>`, `agent.model: fake-model`,
   `agent.workspace`/`tools.filesystem.roots`/`tools.exec.cwd` under one
   `t.TempDir()`, `storage.path: <temp>/mtclaw.db`, and an explicit
   `tools.exec.shell` per `runtime.GOOS`; loads it with
   `config.Load(yaml, dir, map[string]string{"E2E_TG_TOKEN": fake.Token(), "OPENAI_API_KEY": "sk-fake"})`;
   calls `gateway.New(*cfg, discardLogger)`; runs `gw.Run(ctx)` in a goroutine
   with `t.Cleanup(cancel)` and a channel to collect its error.
   All paths through `filepath.ToSlash` when embedded in YAML (Windows
   backslashes are YAML escapes) — copy the treatment at
   `internal/cli/doctor_test.go:287`.
7. **Gateway e2e: DM round trip.** Script `fakeapi.OpenAI` with step 1 = a
   `read_file` tool call against a file the helper wrote into the workspace,
   step 2 = final text. `fake.PushMessage(chatID, userID, "read my notes")`.
   Assert with `waitCond`: a `sendMessage` to `chatID` whose text contains the
   final text; `fake.Requests()` has length 2 and request 2 contains the file
   contents (proves the tool result was fed back); the session has messages in
   the store.
8. **Gateway e2e: chunking.** Same shape, step 1 = final text longer than
   `telegram.DefaultChunkLimit`. Assert ≥2 `sendMessage` calls, all to the same
   chat, in order.
9. **Gateway e2e: approval approve.** Config `tools.exec.mode: approval` with
   empty `allow`/`deny`. Step 1 = an `exec` tool call for a trivially portable
   command (`echo mtclaw-e2e` under the configured shell). Wait for a
   `sendMessage` carrying `reply_markup` with two `callback_data` values; read
   the pending row via `gw.store.Approvals()`/a direct query to learn the
   nonce (do **not** parse the keyboard — the store is the authority); then
   `fake.PushCallback(chatID, userID, "ok:"+id, promptMessageID)`. Assert:
   `answerCallbackQuery` was called; the final reply contains the command
   output; `exec_audit` has one row with `decision = "approved"`.
10. **Gateway e2e: approval deny.** Same, push `"no:"+id`. Assert the turn
    still completes (a reply is sent, `gw.Run` never errors) and that the
    *next* `fakeapi.OpenAI` request body contains a tool message describing the
    refusal — i.e. denial reached the model as a tool result, not as an error.
    Assert `exec_audit.decision = "denied_user"`.
11. **Gateway e2e: cron delivery.** Build a real `cron.Scheduler` in the test:
    `cron.New(cron.JobsFromConfig(cfg.Cron), loc, gw.store.CronRuns(), gw.store.Sessions(), gw.disp.dispatch, log, fakeNow)`
    where `fakeNow` returns a fixed instant at `HH:MM:59.900` so
    `alignDelay` (`cron/scheduler.go:139-142`) is ~100ms, with the job's
    schedule `* * * * *`. Run `sched.Start(ctx)` in a goroutine. Assert:
    delivery lands on `deliver_to.chat_id` (which differs from any inbound
    chat, proving `DeliverTo` routing), and `cron_runs` holds one row that
    reaches `status = "ok"` with a non-null `finished_at`.
    Separately assert `gw.cronSched != nil` when `cron.enabled: true` and
    `nil` when false — that covers the `gateway.New` wiring
    (`gateway.go:110-122`) the fake clock bypasses. State in a comment that
    the real minute-aligned ticker is deliberately not exercised (it would
    cost 60s) and that phase 4's manual checklist covers it.
12. **CLI e2e: `mtclaw prompt`.** In `internal/cli/prompt_e2e_test.go`:
    `testsupport.FakeHome(t)`, write a real config file to a temp path, set
    `OPENAI_API_KEY` via `t.Setenv` (the CLI loads through `LoadFile` →
    `processEnv`, `internal/config/load.go:30-49`), then
    `cmd := newRootCmd(&state{}); cmd.SetArgs([]string{"prompt", "list my notes", "--config", path}); cmd.SetOut(&buf)`
    and `require.NoError(cmd.ExecuteContext(ctx))`. Assert stdout carries the
    final text and stderr carries the `-> running <tool>` progress line
    (`prompt_cmd.go:106-121`). Then build a **second** root command and run
    another prompt against the same config: assert `fakeapi.OpenAI`'s second
    invocation's first request already contains the earlier turn's messages —
    that is "sessions and history survive a restart" for the CLI path.
13. **Run everything.** `go test -race ./...` on Windows. Then re-read every
    new assertion and delete any that passes vacuously (e.g. an
    `assert.NotNil` on something constructed two lines above).

## Tests / validation

```powershell
# 1. transport spike, first and alone
go test -race -run TestSpike ./internal/testsupport/fakeapi/ -v

# 2. new e2e suites
go test -race -run 'TestE2E' ./internal/gateway/ ./internal/cli/ -v

# 3. config surface (docs coverage is mechanical)
go test -race ./internal/config/...

# 4. whole suite, both key gates
go test -race ./...
$env:CGO_ENABLED=0; go build ./...

# 5. prove hermeticity: no credential envs, no ~/.mtclaw writes
Remove-Item Env:OPENAI_API_KEY -ErrorAction SilentlyContinue
Remove-Item Env:TELEGRAM_BOT_TOKEN -ErrorAction SilentlyContinue
go test -race -count=1 ./internal/gateway/ ./internal/cli/
git status --porcelain   # must be clean apart from intended files
```

Also verify `go.mod`'s `require` block is unchanged (`git diff go.mod go.sum`
must be empty).

## Success criteria

- [ ] Spike test proves `telego` + `WithAPIServer` + long polling works against `httptest` on Windows under `-race`
- [ ] `internal/testsupport/fakeapi` imports neither `testing` nor any `internal/` package other than nothing at all (Telegram fake may import `telego` for update types)
- [ ] `useFakeHome` in `internal/cli/onboard_test.go` delegates to `testsupport.FakeHome` — exactly one implementation exists
- [ ] `channels.telegram.api_base_url` documented in `docs/configuration.md`; `go test ./internal/config/...` green
- [ ] Non-empty `api_base_url` that is not an absolute http(s) URL fails `config.Validate` with a message naming the key
- [ ] Gateway DM e2e asserts tool call → tool result fed back → chunk-aware reply to the originating chat
- [ ] Chunking e2e asserts ≥2 `sendMessage` calls for an over-limit reply
- [ ] Approve e2e asserts `reply_markup` with two `callback_data` buttons, `answerCallbackQuery` called, command output in the reply, `exec_audit.decision = "approved"`
- [ ] Deny e2e asserts refusal reaches the model as a tool result and `exec_audit.decision = "denied_user"`
- [ ] Cron e2e asserts delivery to `deliver_to.chat_id` and a `cron_runs` row reaching `ok`; a second assertion covers `gateway.New`'s scheduler wiring
- [ ] `mtclaw prompt` e2e asserts a completed tool-using turn, and a second invocation asserts replayed history
- [ ] Full suite green with `OPENAI_API_KEY`/`TELEGRAM_BOT_TOKEN` unset and no network
- [ ] `git diff go.mod go.sum` is empty

## Risks and rollback

| Risk | Mitigation |
|---|---|
| `telego`+fasthttp will not talk to `httptest` on Windows (plan R5) | Step 1 spike gates the phase. Fallbacks in order: `WithHTTPClient`, then a `botAPI`-level fake. The third option loses the real-transport property and must be reported, not silently adopted. |
| Fake `getUpdates` busy-loop burns CPU / flakes the suite (R6) | 250ms blocking wait; never return non-2xx; assert in the spike that a 2s idle run issues fewer than ~20 `getUpdates` calls. |
| Exec approval e2e is OS-divergent | `tools.exec.shell` set explicitly per `runtime.GOOS` in the helper; the command is `echo mtclaw-e2e`, valid under both `powershell -NoProfile -Command` and `/bin/sh -c`. |
| Fixed sleeps make the suite flaky | Use `waitCond` (`gateway_test_helpers_test.go:98`) exclusively; no `time.Sleep` in assertions. |
| Adding `api_base_url` grows the permanent public config surface | Documented as a real feature (self-hosted Bot API server) with an explicit token-trust warning; see plan Open Question 4 if the user prefers a test-only seam. |

**Rollback:** every change here is additive except the `SendOnce`/`GetMe`/
`CaptureSenders` signatures and the `api_base_url` key. Reverting the phase is
`git revert` of one commit: no schema, no data, no config file on any user's
disk has changed (the new key defaults to empty and existing configs never
mention it).
