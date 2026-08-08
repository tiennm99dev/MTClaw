# Verification: what CI proves vs. what still needs live credentials

This is the honest split between "asserted by a test that runs with no
network and no credentials" and "genuinely cannot be proven without a real
OpenAI key and/or Telegram bot token." Every row in Section A names a real
test function; every entry in Section B is a command you can run yourself,
with what to expect. Nothing appears in both.

This document supersedes the "Remaining manual verification" section of
[the v1 core-system plan](../plans/260731-2219-mtclaw-core-system/plan.md) -
that section now points here.

## Section A: verified automatically

### The v1 plan's four originally-unchecked criteria

The v1 plan shipped with four acceptance-criteria boxes unchecked because
proving them required a real OpenAI key and a real Telegram bot token,
neither available at the time. A credential-free e2e harness
(`internal/testsupport`: a fake HOME directory plus `httptest` fakes of the
Telegram Bot API and OpenAI's chat-completions endpoint) now drives the
*real* `gateway.New`/`gateway.Run`, the *real* `telego` client, the *real*
`openai-go` client, and a *real* SQLite database against those fakes instead
of a real network. This closes the mechanics of all four criteria - the
control flow through the real code, end to end - but **not** their
live-upstream half (real model behavior, real Telegram rendering, a real
60-second clock boundary); see Section B for what remains.

| Criterion | Closed by | Caveat |
|---|---|---|
| `mtclaw prompt "..."` completes a full tool-using agent turn in the terminal, and history survives a restart | `internal/cli/prompt_e2e_test.go::TestE2E_PromptCompletesToolUsingTurn` | Against `fakeapi.OpenAI`'s scripted tool-call response, not a real model. The real model's own tool-call *formatting* is untested here - see Section B item 1. |
| `mtclaw gateway` serves a Telegram DM end to end: message -> agent -> tool -> chunked reply | `internal/gateway/e2e_test.go::TestE2E_DMRoundTrip_ToolCallFedBackAndChunkAwareReply` (tool round trip) and `::TestE2E_ChunkedReply_SplitsAcrossMultipleSendMessageCalls` (>=2 `sendMessage` calls for an over-limit reply) | Against `fakeapi.Telegram`, a real `telego` client talking to an `httptest` server (not the real Telegram Bot API) - see Section B item 2 for what only a real chat can confirm (MarkdownV2 rendering, `/whoami`/`/new`/`/status`). |
| A cron job fires on schedule and delivers to the configured chat, with the run recorded in `cron_runs` | `internal/gateway/e2e_test.go::TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun` (delivery + `cron_runs` reaching `ok`) plus `::TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled` (the wiring gap the fake-clock test alone would bypass) | Uses an anchored, real-time-advancing fake clock 100ms from the next minute boundary (not a literally frozen one - see that test file's `fakeNowAtNextMinuteIn` doc comment) so the test finishes in under a second. The real up-to-60s minute-aligned wait inside `cron/scheduler.go`'s `Start` is never exercised this way - see Section B item 3. |
| The approval flow: an unmatched command produces inline buttons, Approve runs it, Deny returns a refusal to the model as a tool result | `internal/gateway/e2e_test.go::TestE2E_ApprovalApprove_RunsCommandAndAudits` and `::TestE2E_ApprovalDeny_RefusalReachesModelAndAudits` | Same fake-Telegram caveat as the DM round trip row above. |

### Transport assumption (the harness's own foundation)

| Criterion | Closed by |
|---|---|
| `telego` + `WithAPIServer` + long polling works against an `httptest` server (the premise the whole harness depends on) | `internal/testsupport/fakeapi/telegram_spike_test.go::TestSpike_TelegoTalksToHTTPTestServer` |
| A fake `getUpdates` that never returns instantly does not make `telego` busy-spin | same file, `::TestSpike_GetUpdatesDoesNotBusySpin` |

### Store portability seam (dialect, migration ledger, factory)

| Criterion | Closed by |
|---|---|
| A fresh database migrates to the latest schema and the ledger records it | `internal/store/sqlite/migrate_test.go::TestOpen_FreshDatabaseMigratesToLatest` |
| Reopening an up-to-date database applies nothing | same file, `::TestOpen_ReopenUpToDateDatabaseAppliesNoMigrations` |
| A database whose ledger names a migration newer than this binary understands is refused | same file, `::TestOpen_NewerLedgerVersionIsRefused` |
| A legacy (pre-ledger) v1 database adopts its bookkeeping without re-running `001_init.sql`, and a session written before the upgrade is still readable | same file, `::TestOpen_LegacyDatabaseAdoptsExistingSchemaVersion` |
| The frozen `testdata/v1_legacy_schema.sql` fixture matches a fresh migrate byte-for-byte in shape | same file, `::TestFreshSchemaMatchesFrozenLegacyFixture` |
| WAL, `busy_timeout=5000`, `foreign_keys=ON` on both writer and reader; writer pool capped at one connection | same file, `::TestPragmasAndPoolAreSetOnWriterAndReader` |
| `store.Open` with `Driver:"sqlite"` returns a usable `store.Store`; the deprecated `storage.path` alias round-trips through `EffectiveDSN()` the same way | `internal/store/factory_test.go::TestOpen_SQLiteDriverReturnsUsableStore` and `::TestOpen_SQLiteDriverHonorsPathAliasViaEffectiveDSN` |
| An unregistered driver name (e.g. `postgres`) is refused, naming the supported set | same file, `::TestOpen_UnregisteredDriverNamesSupportedSet` |
| Registering the same driver name twice panics (a programmer error, not a runtime config error) | same file, `::TestRegister_DuplicateDriverNamePanics` |

### `storage.dsn` / `storage.path` compatibility and the `.yml` config alias

| Criterion | Closed by |
|---|---|
| A config setting only `storage.dsn` loads with no warning | `internal/config/load_test.go::TestLoad_StorageDSNOnly` |
| A config setting only the deprecated `storage.path` loads, warns exactly once on stderr, and normalizes into `dsn` | same file, `::TestLoad_StoragePathAliasWarnsOnceAndNormalizes` |
| Setting both `storage.dsn` and `storage.path` fails to load, naming both keys | same file, `::TestLoad_StorageBothDSNAndPathSetFails` (whole-pipeline) and `internal/config/validate_test.go::TestValidate_StorageBothDSNAndPathSet` (validator alone) |
| `storage.driver: postgres` (or any name this binary does not register) fails to load, naming the supported set | `internal/config/load_test.go::TestLoad_StorageUnsupportedDriverFails` and `internal/config/validate_test.go::TestValidate_StorageUnsupportedDriver` |
| `~/.mtclaw/config.yml` is found when `config.yaml` is absent, with source reported as `"default"` | `internal/config/load_test.go::TestConfigPath_DefaultAliasPrecedence` (subtest "yml only") |
| With both `config.yaml` and `config.yml` present, `.yaml` wins and exactly one stderr warning names the ignored `.yml` | same test, subtest "both present" |
| With neither present, the reported default path still names `config.yaml` (so the eventual not-found error and `onboard`'s write agree) | same test, subtest "neither present" |
| `--config foo.yml` and `MTCLAW_CONFIG=foo.yml` are used byte-for-byte, never extension-rewritten, whether or not the named file exists on disk | `internal/config/load_test.go::TestConfigPath_ExplicitFlagNeverAliasResolved` |
| Every config key (including `storage.driver`/`storage.dsn`/`storage.path` and `channels.telegram.api_base_url`) has a documentation row | `internal/config/docs_coverage_test.go::TestConfigFieldsAreDocumented` |

### A note on `go test -race` and CI

`.github/workflows/ci.yml` is configured to run `go test -race ./...` across
the full `ubuntu-latest`/`macos-latest`/`windows-latest` matrix on every push
and pull request (installing mingw-w64 for the Windows leg specifically for
this, since `-race` needs a real C compiler that `windows-latest` does not
ship by default). That configuration is real and current.

**What it is not: currently green.** The last actual run on `main`
(`gh run view 30700212428`, the push that merged the v1 plan) failed on all
three legs, for two causes entirely unrelated to this plan's four phases:

1. **`internal/tools.TestExec_TimeoutKillsWholeProcessTree` panics with a nil
   pointer dereference under `-race` on `ubuntu-latest` and `macos-latest`**
   (`execTool.ask` at `exec.go:161` calling a nil `approver.Ask`, reached from
   `execTool.run`'s `VerdictAsk` branch even though the test sets
   `Allow: [".*"]`, which should route it to `VerdictRun` instead). This
   test passes reliably without `-race` (confirmed locally, repeatedly) but
   the CI log shows it reaching the wrong branch under `-race`'s altered
   scheduling. `internal/tools` is outside every phase of this plan's file
   ownership; nothing here touches `exec.go`, `exec_test.go`, or `policy.go`
   beyond a phase 2 signature-only edit to `newTestExecTool`'s store-opening
   call. This is a pre-existing bug, not a regression introduced by this
   plan, and it is not fixed here - see the phase 4 implementation report
   for the full citation.
2. **The `windows-latest` leg fails its own `gofmt check (Windows)` step
   before `-race` ever runs**, listing essentially every `.go` file in the
   repository as "unformatted." This is consistent with a CRLF/LF
   line-ending mismatch on checkout (`gofmt` is line-ending sensitive) rather
   than a real formatting problem - `gofmt -l .` on this development machine
   (also Windows) reports nothing. Also pre-existing, also outside this
   plan's scope, also not fixed here.

Both should be tracked and fixed before relying on this CI matrix as a
release gate. Locally, `-race` cannot run at all in the environment these
four phases were implemented in (`CGO_ENABLED=0`, no C compiler on `PATH`);
every test in this plan was instead run repeatedly (5x, `-count=1`) across
the touched packages to rule out flakiness by repetition - see the phase
1-4 implementation reports for the literal output.

## Section B: requires live credentials

Each of these needs a real OpenAI API key and/or a real Telegram bot token
and cannot be faked - the reason is stated inline. None of them is asserted
by any test named above.

### 1. A real OpenAI turn

**Why it can't be faked:** `fakeapi.OpenAI` returns a scripted response; it
proves the gateway/CLI code paths that consume a response, never whether a
real model actually produces tool-call JSON in the shape the SDK and this
code expect.

```sh
export OPENAI_API_KEY=sk-...
mtclaw doctor                                   # OpenAI rows should read OK
mtclaw prompt "list the files in my workspace"  # expect a completed, tool-using reply
```

Expected: `doctor`'s "OpenAI key resolves" / "OpenAI reachable" / "Model
exists" rows all read `OK`; `prompt` returns a real answer that used the
`list_dir` tool (visible via `--log-level debug` showing a `tool_calls`
entry), not an error.

### 2. A real Telegram DM round trip, and `/whoami` / `/new` / `/status`

**Why it can't be faked:** `fakeapi.Telegram` proves the gateway's own logic
(gating, dispatch, chunking, the approval keyboard); it cannot prove how the
*real* Telegram client renders MarkdownV2, nor exercise `/whoami`/`/new`/
`/status`, which have no dedicated unit or e2e test today (grep
`internal/channel/telegram/*_test.go` for `whoami`/`new`/`status`: none).

```sh
mtclaw onboard     # real bot token, capture your own Telegram user id
mtclaw gateway     # then, from Telegram:
# DM the bot anything -> expect a reply, correctly rendered (bold/code/etc.)
# /whoami -> expect your numeric user id and this chat's id
# /new    -> expect confirmation that history was cleared
# /status -> expect session/message counts for this chat
```

Expected: every command replies in the same chat with the described content
and correctly rendered Markdown (bold, code blocks); no `parse_mode` 400 in
the logs (or, if one occurs, a silent plain-text fallback per
`docs/architecture.md`'s request lifecycle).

### 3. A real cron fire on a real minute boundary

**Why it can't be faked:** `TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun`
uses a fake clock anchored 100ms from the next minute boundary specifically
so the test does not take up to 60 real seconds; the actual wait-until-`:00`
logic in `cron/scheduler.go`'s `Start` (`alignDelay`, then `tick()`) is
never exercised end to end with a genuinely uncontrolled wall clock.

```yaml
# in your config:
cron:
  enabled: true
  jobs:
    - name: manual-check
      schedule: "* * * * *"
      prompt: "say hello"
      enabled: true
      session: ephemeral
      timeout: 30s
      deliver_to: { channel: telegram, chat_id: "<your chat id>" }
```

```sh
mtclaw gateway   # wait for the next real minute boundary
mtclaw cron list # confirm next-due time, then watch Telegram
```

Expected: a message arrives in the target chat within a few seconds of the
minute rolling over, and `mtclaw approvals list` / a `sessions` inspection
(or a direct look via `sqlite3 ~/.mtclaw/mtclaw.db 'select * from cron_runs'`)
shows a `cron_runs` row for `manual-check` with `status = ok`.

### 4. `onboard` on a clean machine, including the Telegram ID capture window

**Why it can't be faked:** `internal/cli/onboard_test.go` exercises the
capture logic (`internal/channel/telegram/capture.go:26-40`) against a
scripted fake poller - it proves the branching (single sender, multiple
senders, manual fallback) but never a real 60-second window racing against
a real person actually messaging the bot on a real phone.

```sh
rm -rf ~/.mtclaw   # only on a machine you're happy to reset - or use a fresh one
mtclaw onboard
# during the capture window, message the bot from your real Telegram account
```

Expected: `onboard` reports your username and numeric ID, asks for
confirmation before writing it to `allow_from`, and the resulting
`~/.mtclaw/config.yaml` passes `mtclaw config validate` and `mtclaw doctor`
without needing any manual edit.

### 5. Token-leak log grep with a real credential present

**Why it can't be faked:** proving a real secret shape never reaches a log
line requires a real secret actually flowing through the process - the fake
harness's tokens are synthetic and never claimed to be indistinguishable
from what a real logging bug would leak.

```sh
export OPENAI_API_KEY=sk-...
export TELEGRAM_BOT_TOKEN=123456789:...
mtclaw gateway --log-level debug > /tmp/mtclaw.log 2>&1 &
# send a few messages, including one exec command, then stop it
grep -ri "$OPENAI_API_KEY" -r ~/.mtclaw/ /tmp/mtclaw.log
grep -ri "$TELEGRAM_BOT_TOKEN" -r ~/.mtclaw/ /tmp/mtclaw.log
```

Expected: zero hits from both `grep` invocations.

### 6. A tagged release producing five binaries

**Why it can't be faked:** `.github/workflows/release.yml` only actually
*runs* on a real `v*` tag push against GitHub's own Actions infrastructure;
a local `make release` rehearsal (already done once, see the phase 9
implementation report) builds the same five targets but never exercises the
workflow file itself, the GitHub release API call, or `SHA256SUMS`
attachment.

```sh
git tag v0.1.0
git push origin v0.1.0
```

Expected: a GitHub Actions "Release" run completes, and the release page for
`v0.1.0` has five binaries (`mtclaw-v0.1.0-{linux,darwin}-{amd64,arm64}`,
`mtclaw-v0.1.0-windows-amd64.exe`) plus `SHA256SUMS` attached.
