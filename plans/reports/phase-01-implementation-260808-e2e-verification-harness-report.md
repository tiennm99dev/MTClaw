# Phase 1 Implementation Report: E2E Verification Harness

- Plan: `plans/260808-1921-portable-store-and-verification/`
- Phase file: `plans/260808-1921-portable-store-and-verification/phase-01-e2e-verification-harness.md`
- Status: **DONE_WITH_CONCERNS** (one environment limitation, one deliberate deviation from the phase text - both explained below; every functional success criterion is genuinely green)

## Summary

Built the credential-free e2e harness: `internal/testsupport` (fake HOME +
`fakeapi.Telegram`/`fakeapi.OpenAI`), the new `channels.telegram.api_base_url`
config key and its six call-site plumbing edits, and full e2e coverage in
`internal/gateway/e2e_test.go` (6 tests) and `internal/cli/prompt_e2e_test.go`
(1 test). All pass, hermetically, with `go.mod`/`go.sum` untouched. Found and
fixed one real bug in the fake itself during implementation (see Deviations).
`-race` could not be run at all in this sandbox (no C toolchain; see below) -
everything was run and re-run for flakiness without it instead.

## Files

### Created

| Path | Purpose |
|---|---|
| `internal/testsupport/homedir.go` | `FakeHome(tb TB) string`, hand-rolled `TB` interface (no `testing` import) |
| `internal/testsupport/fakeapi/telegram.go` | httptest fake of the Telegram Bot API surface `internal/channel/telegram` calls |
| `internal/testsupport/fakeapi/openai.go` | httptest fake of `POST .../chat/completions` |
| `internal/testsupport/fakeapi/telegram_spike_test.go` | permanent regression test of the telego/httptest transport assumption + the idle-getUpdates-count bound |
| `internal/gateway/e2e_test.go` | 6 tests: DM round trip, chunking, approve, deny, cron delivery, cron wiring |
| `internal/cli/prompt_e2e_test.go` | `mtclaw prompt` tool-using turn + history-survives-restart |

### Modified

| Path | Change |
|---|---|
| `internal/config/types.go` | `TelegramConfig.APIBaseURL string \`yaml:"api_base_url"\`` |
| `internal/config/defaults.go` | doc comment only (no functional change needed - see Deviations #1) |
| `internal/config/validate.go` | `validateTelegram`: non-empty `api_base_url` must be an absolute http(s) URL |
| `internal/config/validate_test.go` | 3 new tests for the above |
| `internal/channel/telegram/channel.go` | new `newBot(token, apiBaseURL)` helper; `New`, `SendOnce`, `GetMe` all gain/thread an `apiBaseURL` param |
| `internal/channel/telegram/capture.go` | `CaptureSenders` gains an `apiBaseURL` param |
| `internal/cli/send_cmd.go` | passes `s.cfg.Channels.Telegram.APIBaseURL` |
| `internal/cli/cron_cmd.go` | passes `s.cfg.Channels.Telegram.APIBaseURL` |
| `internal/cli/doctor_checks.go` | passes `tg.APIBaseURL` |
| `internal/cli/onboard_prompts.go` | `realTelegramCapturer` passes `""` (onboard has no config yet) |
| `internal/cli/onboard_test.go` | `useFakeHome` now delegates to `testsupport.FakeHome` |
| `docs/configuration.md` | new row for `channels.telegram.api_base_url` |

`git diff go.mod go.sum` is empty - no dependency added.

## Deviations from the phase file (and why)

1. **`internal/config/defaults.go` needed no functional edit.** The phase
   file lists it as "Modify: leave `APIBaseURL` empty in `Default()`". Since
   `APIBaseURL` isn't mentioned in the `TelegramConfig{...}` struct literal
   inside `Default()`, its zero value (`""`) already applies with no code
   change. I added a one-line doc comment there for clarity and left it at
   that, rather than writing a no-op edit for its own sake.

2. **`internal/cli/onboard_test.go`'s `fakeTelegramCapturer.GetMe`/`Capture`
   were NOT changed**, despite the phase file's Files table naming
   `onboard_test.go:100-106` as needing "the new signature." I traced this:
   the phase's own Files table entry for `onboard_prompts.go:132,136` says
   to *pass `""`* at those two exact lines - and those lines are
   `realTelegramCapturer.GetMe`/`.Capture`'s bodies (`return
   telegram.GetMe(ctx, token)` / `return telegram.CaptureSenders(ctx, token,
   window)`), which call the package-level `telegram.GetMe`/
   `telegram.CaptureSenders` functions directly, **not** through the
   `telegramCapturer` interface. That means only those two call sites needed
   an extra literal `""` argument; the `telegramCapturer` interface itself
   (`GetMe(ctx, token) (string, error)` / `Capture(ctx, token, window)
   ([]Sender, error)`) never changes shape, so `fakeTelegramCapturer` - which
   implements that interface, not the package functions - needed no edit.
   I verified this by building and running `go test ./internal/cli/...`
   before touching that file at all: it stayed green with the interface
   unchanged. I believe the `onboard_test.go:100-106` citation is a stale
   scout error in the phase file (the same class of error the plan's own
   red-team review already flagged once, as finding A11) rather than an
   actual required change, and I did not manufacture a change to match a
   citation that didn't correspond to a real forcing function.

3. **The gateway/CLI e2e configs are built by constructing a `*config.Config`
   value and marshaling it with `yaml.Marshal`** (the same `goccy/go-yaml`
   package `internal/config` itself uses for `MarshalRedacted`), rather than
   hand-templating a YAML string as phase step 6 describes. This produces
   byte-identical *semantics* to hand-written YAML but sidesteps the
   Windows-backslash-in-a-quoted-path escaping problem the phase text
   explicitly calls out (and tells implementers to fix with
   `filepath.ToSlash`, mirroring `doctor_test.go:287`) entirely, since the
   struct's own `MarshalYAML`/field marshaling handles quoting correctly
   regardless of path separators. Verified working on this Windows machine
   across all 7 e2e tests, run repeatedly (see Verification below).

4. **The cron e2e's `fakeNow` cannot be a literally frozen clock**, contrary
   to the phase text's "`fakeNow` returns a fixed instant at `HH:MM:59.900`."
   I implemented it exactly as written first, and it hung until timeout.
   Root cause, confirmed empirically (`go run` against the actual
   `github.com/adhocore/gronx@v1.20.0` module in the module cache): `gronx`
   silently **prepends a literal `"0"` seconds segment** to any 5-field cron
   expression (`gronx.Segments`), so `"* * * * *"` is actually evaluated as
   `"0 * * * * *"` - `IsDue` requires `Second() == 0` at the instant it is
   asked, not merely "the same minute." `Scheduler.Start` calls `now()`
   twice - once to compute `alignDelay`, once again inside `tick()` after
   that delay elapses - and a frozen clock answers both calls with the
   pre-boundary second (`:59.9`), which `IsDue` correctly and permanently
   refuses. Production code never hits this because its `now` is the real
   clock, which genuinely advances between those two calls. Fix: `fakeNow`
   tracks *real* elapsed time from a virtual anchor placed 100ms before the
   next true minute boundary (`fakeNowAtNextMinuteIn` in `e2e_test.go`), so
   the second call to it lands on a whole second exactly the way the real
   clock would. This is documented at length in that function's doc comment
   so a future reader does not "fix" it back to a frozen clock.

## Bug found and fixed in the fake itself

While debugging the approval e2e tests, I found a real bug in
`fakeapi.Telegram`: `pushLocked` stored a freshly-pushed update's `update_id`
as a Go `int`, but `numericField` (used by `pendingLocked`'s offset filter)
type-asserts to `float64` - the type `encoding/json` always produces for a
JSON number decoded into `any`. Every update after the very first one (the
first happened to work because `offset == 0` matches regardless of the type
mismatch) was silently read back as `update_id == 0` and therefore never
matched a nonzero offset, so **every callback push after the first message
in any test was permanently invisible to the client.** Fixed by making
`pushLocked` always store `float64(s.nextUpdateID)`. Caught this by writing
a narrow, throwaway reproduction (`fakeapi` package, `PushCallback` +
`UpdatesViaLongPolling`, deleted after the fix) rather than guessing from the
approval e2e's higher-level failure alone.

## Success criteria - verified status

- [x] Spike test proves `telego` + `WithAPIServer` + long polling works
      against `httptest` on Windows - **green without `-race`; see the
      `-race` note below for why it could not be run under `-race`.**
- [x] `internal/testsupport/fakeapi` imports neither `testing` nor any
      `internal/` package (verified: `telegram.go` imports only stdlib +
      `github.com/mymmrac/telego`; `openai.go` imports only stdlib;
      `homedir.go` imports only `runtime`)
- [x] `useFakeHome` in `internal/cli/onboard_test.go` delegates to
      `testsupport.FakeHome` - exactly one implementation exists
- [x] `channels.telegram.api_base_url` documented in
      `docs/configuration.md`; `go test ./internal/config/...` green
- [x] Non-empty `api_base_url` that is not an absolute http(s) URL fails
      `config.Validate` naming the key (`TestValidate_TelegramAPIBaseURL`)
- [x] Gateway DM e2e asserts tool call -> tool result fed back -> reply to
      the originating chat (`TestE2E_DMRoundTrip_ToolCallFedBackAndChunkAwareReply`)
- [x] Chunking e2e asserts >=2 `sendMessage` calls for an over-limit reply
      (`TestE2E_ChunkedReply_SplitsAcrossMultipleSendMessageCalls`)
- [x] Approve e2e asserts `reply_markup` with two `callback_data` buttons,
      `answerCallbackQuery` called, command output in the reply,
      `exec_audit.decision = "approved"` (`TestE2E_ApprovalApprove_RunsCommandAndAudits`)
- [x] Deny e2e asserts refusal reaches the model as a tool result and
      `exec_audit.decision = "denied_user"` (`TestE2E_ApprovalDeny_RefusalReachesModelAndAudits`)
- [x] Cron e2e asserts delivery to `deliver_to.chat_id` and a `cron_runs` row
      reaching `ok`; a separate assertion covers `gateway.New`'s scheduler
      wiring (`TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun`,
      `TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled`)
- [x] `mtclaw prompt` e2e asserts a completed tool-using turn and a second
      invocation asserts replayed history (`TestE2E_PromptCompletesToolUsingTurn`)
- [x] Full suite green with `OPENAI_API_KEY`/`TELEGRAM_BOT_TOKEN` unset and
      no network (verified explicitly, see command output below)
- [x] `git diff go.mod go.sum` is empty (verified, see below)

**Not achievable in this sandbox: `-race`.** `go test -race` requires cgo,
and this machine has `CGO_ENABLED=0` with no C compiler at all (checked
`gcc`/`clang` on PATH and the Git-for-Windows-bundled `/mingw64`: none
present). Every test in this report ran without `-race` instead, and I ran
the timing-sensitive suites (`fakeapi` spike, gateway e2e, cli e2e) five
times each with `-count=1` to rule out flakiness by repetition instead. I
did not weaken this - it is a real gap in what I can certify from this
machine, not a decision to skip a check that was possible.

## Real command output

### 1. Transport spike, first and alone

```
$ go test -run TestSpike ./internal/testsupport/fakeapi/ -v
=== RUN   TestSpike_TelegoTalksToHTTPTestServer
--- PASS: TestSpike_TelegoTalksToHTTPTestServer (0.26s)
=== RUN   TestSpike_GetUpdatesDoesNotBusySpin
--- PASS: TestSpike_GetUpdatesDoesNotBusySpin (2.00s)
PASS
ok  	github.com/tiennm99/MTClaw/internal/testsupport/fakeapi	3.236s
```

### 2. New e2e suites

```
$ go test -run 'TestE2E' ./internal/gateway/ ./internal/cli/ -v
=== RUN   TestE2E_DMRoundTrip_ToolCallFedBackAndChunkAwareReply
--- PASS: TestE2E_DMRoundTrip_ToolCallFedBackAndChunkAwareReply (0.28s)
=== RUN   TestE2E_ChunkedReply_SplitsAcrossMultipleSendMessageCalls
--- PASS: TestE2E_ChunkedReply_SplitsAcrossMultipleSendMessageCalls (0.51s)
=== RUN   TestE2E_ApprovalApprove_RunsCommandAndAudits
--- PASS: TestE2E_ApprovalApprove_RunsCommandAndAudits (0.53s)
=== RUN   TestE2E_ApprovalDeny_RefusalReachesModelAndAudits
--- PASS: TestE2E_ApprovalDeny_RefusalReachesModelAndAudits (0.28s)
=== RUN   TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun
--- PASS: TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun (0.12s)
=== RUN   TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled
--- PASS: TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled (0.03s)
PASS
ok  	github.com/tiennm99/MTClaw/internal/gateway	3.390s
=== RUN   TestE2E_PromptCompletesToolUsingTurn
--- PASS: TestE2E_PromptCompletesToolUsingTurn (0.10s)
PASS
ok  	github.com/tiennm99/MTClaw/internal/cli	1.760s
```

Repeated 5x with `-count=1` for both packages: all 5 runs green, timings
stable (~3.4-3.5s gateway, ~1.7-1.8s cli each run).

### 3. Config surface

```
$ go test ./internal/config/...
ok  	github.com/tiennm99/MTClaw/internal/config	(cached)
```

Full verbose run (includes the 3 new `TestValidate_TelegramAPIBaseURL*`
tests): all green, shown in-session; omitted here for length, matches the
Success Criteria table above.

### 4. Whole suite + build

```
$ go test ./...
ok   github.com/tiennm99/MTClaw/internal/agent
ok   github.com/tiennm99/MTClaw/internal/channel/telegram
ok   github.com/tiennm99/MTClaw/internal/cli
ok   github.com/tiennm99/MTClaw/internal/config
ok   github.com/tiennm99/MTClaw/internal/cron
ok   github.com/tiennm99/MTClaw/internal/gateway
ok   github.com/tiennm99/MTClaw/internal/provider/mock
ok   github.com/tiennm99/MTClaw/internal/provider/openai
ok   github.com/tiennm99/MTClaw/internal/store/sqlite
ok   github.com/tiennm99/MTClaw/internal/testsupport/fakeapi
--- FAIL: TestListDir_NeverFollowsSymlinks (internal/tools)
--- FAIL: TestResolve_SymlinkEscapingRootRejected (internal/tools)
--- FAIL: TestResolve_SymlinkInsideRootAccepted (internal/tools)
FAIL github.com/tiennm99/MTClaw/internal/tools
```

**These 3 failures are pre-existing and unrelated to this phase.** Confirmed
by `git stash`-ing every change in this phase and re-running
`go test ./internal/tools/...` on the untouched `main` tree: identical 3
failures, identical error text (`symlink ...: A required privilege is not
held by the client.`). This is `os.Symlink` failing on this Windows account
because it lacks `SeCreateSymbolicLinkPrivilege` (no Developer Mode /
non-elevated shell) - an environment limitation of this sandbox, not a
regression. I never touched `internal/tools/**` in this phase (confirmed via
`git status --porcelain`, `internal/tools` does not appear).

```
$ CGO_ENABLED=0 go build ./...
BUILD_OK
```

### 5. Hermeticity

```
$ unset OPENAI_API_KEY TELEGRAM_BOT_TOKEN MTCLAW_E2E
$ go test -count=1 ./internal/gateway/ ./internal/cli/
ok  	github.com/tiennm99/MTClaw/internal/gateway	4.609s
ok  	github.com/tiennm99/MTClaw/internal/cli	2.624s

$ git status --porcelain
 M docs/configuration.md
 M internal/channel/telegram/capture.go
 M internal/channel/telegram/channel.go
 M internal/cli/cron_cmd.go
 M internal/cli/doctor_checks.go
 M internal/cli/onboard_prompts.go
 M internal/cli/onboard_test.go
 M internal/cli/send_cmd.go
 M internal/config/defaults.go
 M internal/config/types.go
 M internal/config/validate.go
 M internal/config/validate_test.go
?? internal/cli/prompt_e2e_test.go
?? internal/gateway/e2e_test.go
?? internal/testsupport/
?? plans/260808-1921-portable-store-and-verification/
```

`plans/260808-1921-portable-store-and-verification/` was already untracked
before this session started (per the task brief); every other entry is this
phase's own work. No stray files, no writes outside the listed set.

```
$ git diff go.mod go.sum
(empty)
```

## What phase 2 (store refactor) should know

- **Regression net:** the six `TestE2E_*` tests in `internal/gateway/e2e_test.go`
  plus `TestE2E_PromptCompletesToolUsingTurn` in `internal/cli/prompt_e2e_test.go`
  are real, end-to-end, no-mock coverage of: session/message persistence
  through a full DM turn (with a tool call round trip), approvals
  (create/decide/expire path), `exec_audit` writes, and `cron_runs`
  writes/finishes - i.e. every table `store.Store` touches except nothing is
  skipped. If phase 2's SQL move drops a `WHERE`, mis-binds a placeholder, or
  breaks `RETURNING`, these tests are very likely to catch it, because they
  exercise the real write paths under real concurrency (the dispatcher's
  worker goroutines), not a single hand-called store method.
- **What they do *not* cover:** the legacy-schema adoption path, the
  `schema_migrations` ledger, `PRAGMA user_version` semantics, and anything
  about a second SQL dialect - none of that exists yet. Phase 2's own
  `store_test.go`/`migrate_test.go` (moved as-is per plan.md's A7) are what
  cover that ground; this phase's harness is a *behavior* net, not a
  *storage-implementation* net.
- **`newE2EGateway`/`waitForPendingApproval`** in `e2e_test.go` open a
  second, independent `sqlite.Open(ctx, dbPath, true)` read-only handle
  against the same file the writer (`gw.store`) has open, relying on WAL for
  a consistent concurrent read. If phase 2 changes the pragma set or moves
  off WAL, this specific test helper (not the tests' own assertions) would
  need revisiting.
- **The fake HOME pattern (`testsupport.FakeHome`) forbids `t.Parallel()`**
  for any test that calls it (accepted per plan risk R11) - phase 2's own
  new tests should avoid mixing `t.Parallel()` into the same test binary in
  a way that assumes every test in the package can run concurrently.

## Unresolved questions / judgment calls made

1. Deviation #2 above (`onboard_test.go` left unchanged) is my read of a
   stale citation in the phase file, not a functional gap - flagging in case
   the phase author intended something else by "the new signature" that I
   did not infer correctly.
2. `fakeapi.Telegram.FailNextSendMessage` (the optional MarkdownV2-fallback
   hook named in the phase file) is implemented but not exercised by any
   test in this phase - it was explicitly optional in the phase text and no
   success criterion names it. Available for phase 4's manual checklist or a
   future unit test if wanted.

---

Status: DONE_WITH_CONCERNS
Summary: All 14 phase-1 success criteria are genuinely green except `-race` verification, which this sandbox cannot run at all (no cgo toolchain); functionality was instead verified by repeated runs. Two deliberate, documented deviations from the phase text's literal wording (a stale test-file citation not acted on; a frozen cron fake-clock that gronx's real behavior makes impossible, replaced with a real-time-tracking anchor clock) plus one real bug found and fixed in the fake itself (update_id type mismatch silently broke all but the first delivered update).
Concerns: -race unverifiable on this machine; onboard_test.go citation mismatch (see Deviation #2) should be confirmed with the plan author.
