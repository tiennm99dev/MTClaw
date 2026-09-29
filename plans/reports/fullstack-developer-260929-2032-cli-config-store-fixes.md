# Fixes: cli / config / store / gateway lock

Date: 2026-09-29. Status: DONE_WITH_CONCERNS (see "Not done / concerns"). Nothing committed.

All requested findings are fixed. Every fix has a regression test that fails
when the fix is reverted (mutation-checked by editing the fix out, running the
test, then restoring the file), except where noted.

## Per finding

| Finding | What changed | Test | Mutation check |
|---|---|---|---|
| M1 DSN escaping | `internal/store/sqlite/dialect.go`: `%`, `?`, `#` percent-escaped in the `file:` URI path (`uriPathEscaper`). | `TestDSN_EscapesURIMetacharactersInPath` (dialect_internal_test.go): dirs `a#1`, `b?x`, `c%41`, `d%2Fe`; file exists at exact path, mode 0600 (skipped on Windows). | Fails with escaping removed. |
| M2 deliver validation | `internal/config/validate.go`: exported `ValidateCronDeliverTo(cfg, job)` wrapping the existing check, unconditional. `internal/cli/cron_cmd.go`: called when `--deliver`, before the lock check, store open and model call. | `TestCronRunCmd_DeliverValidatesTargetBeforeAnyAPICall` (unlisted, empty, `*`; asserts zero OpenAI requests and no sendMessage), `TestCronRunCmd_DeliverToReachableChatOfDisabledJobSucceeds` (cli); `TestValidateCronDeliverTo_AppliesRegardlessOfEnabledFlags` (config). Uses fakeapi OpenAI and Telegram. | Fails with the call disabled. |
| M3 job timeout | `cron_cmd.go`: `context.WithTimeout(ctx, job.Timeout.Std())` around `loop.Run`; run still recorded under `WithoutCancel`. | `TestCronRunCmd_HonorsJobTimeout`: local stalling httptest server, 1 s timeout, asserts error, return in <15 s, and an `error` run row. (fakeapi.OpenAI has no stall option and is not mine, so the stall server is test-local.) | With timeout changed to 1 h the test failed (after ~110 s, the SDK's own 120 s limit). |
| M4 doctor dir check | `doctor_checks.go`: `checkStateDirWritable` replaced by `checkStorageDirWritable` (row "Storage dir writable"). Probes `filepath.Dir(storage dsn)` and, if set, `filepath.Dir(log.file)`. No MkdirAll: a missing dir is judged by its nearest existing ancestor. `config.StateDir` no longer used here. | `TestCheckStorageDirWritable` (missing dir not created; `~/.mtclaw` not created; read-only dir fails, skipped as root/Windows; log dir blocked by a file fails). Old `TestCheckStateDirWritable` removed. | Fails when the check creates `~/.mtclaw` again. |
| M5 retention | No code change, by decision. | n/a | n/a |
| L1 log.file | `validate.go` `validateLog`: rejects an existing directory, and a parent that is not creatable (reuses `ensureDirCreatable`). Doctor covers it via M4. | `TestValidate_LogFile` (3 subtests). | Fails with the check removed. |
| L2 `*` chat_id | `cronChatIDReachable` skips the `"*"` group key. | `TestValidate_CronDeliverTo/wildcard_group_key...` | Fails with the skip removed. |
| L3 YAML docs | `load.go`: second `Decode` must return `io.EOF`, else "multiple YAML documents are not supported"; empty file gives "config file is empty". | `TestLoad_RejectsEmptyAndMultiDocumentYAML` (empty, second doc, leading `---` still loads). | Each half fails when reverted separately. |
| L4 secret files | `load.go`: reads `max+1`, errors over 64 KiB, `strings.TrimSpace`. Doc comments updated. | `TestLoad_SecretEnvOverlay` subtests: over cap rejected (replaces the old "truncated" test), exactly at cap loads, spaces/tabs trimmed. | Both fail when reverted. |
| L5 recovery fallback | `dialect.go`: new `finishRecovery` opens read-write only to roll the WAL forward, closes, reopens read-only; `Migrate` then runs with write=false. No `MkdirAll` in the fallback. | `TestFinishRecovery_ReturnsReadOnlyHandleWithoutMigrating`. A real SQLITE_READONLY_RECOVERY could not be reproduced (a copied WAL with no shm opens fine read-only), so the test drives `finishRecovery` directly, not the `Open` branch. | Fails if it returns the read-write handle. |
| L6 `sessions rm` | `root.go`: `requireExistingDatabase()` extracted from the read-only branch of `openStore`; `sessions_cmd.go` `rm` calls it first. | `TestSessionsRmCmd_MissingDatabaseIsReportedNotCreated`, `TestSessionsRmCmd_DeletesExistingSession`. `TestOpenStore_WriteModeCreatesStorageDir_ReadOnlyDoesNot` used `sessions rm` as its write-open probe; it now uses `prompt`. | Fails with the guard removed. |
| L7 flock | `internal/gateway/lock_unix.go`: `tryFlock` treats only EWOULDBLOCK as held, returns other errors; `Acquire` retries 5 x 25 ms; `Held` probes with `LOCK_SH` (conflicts with the gateway's exclusive lock, not with other probes) and returns errors instead of "held". Windows file untouched. | `lock_unix_test.go` (new): `TestAcquire_WaitsOutAMomentaryProbe`, `TestTryFlock_OnlyWouldBlockMeansHeld` (EBADF as a stand-in for ENOLCK), `TestHeld_ConcurrentProbesDoNotSeeEachOther`. | Each fails when its part is reverted (attempts=1, EWOULDBLOCK check removed, `LOCK_EX` probe). |
| L8 onboard write | `onboard_cmd.go`: `publishExclusive` writes a `0600` temp file in the same dir, checks Write and Close, hard-links into place (EEXIST maps to `errConfigAlreadyExists`), always removes the temp. | `TestPublishExclusive` (content, mode, no overwrite, no leftover temp). | Fails when temp cleanup is removed. An ENOSPC write failure cannot be injected portably, so that exact path is covered by construction, not by a test. |
| L9 tie-breaks | `audit.go`, `cron_runs.go`: `, id DESC`. | `TestAuditAndCronRuns_ListTiesBrokenByIDDesc`. It drops the timestamp indexes first, because with the index present SQLite already returns rowid order and the test would pass without the fix. | Fails for each query when reverted. |
| L10 duplicate versions | `migrate.go`: `loadMigrations` now delegates to `readMigrations(fs.FS)`; duplicate versions are an error. | `migrate_internal_test.go`: `TestReadMigrations_RejectsDuplicateVersions`, `..._AcceptsDistinctVersions`. | Fails with the check removed. |
| L11 exit codes | `root.go`: `newRootContext` records the signal (own `signal.Notify`, handler released after the first signal as before) and now returns a third value; `Execute` returns an `interruptedError` (still `errors.Is(ErrInterrupted)`); new `cli.ExitCode(err)` gives 128+signal (130/143), 1 otherwise. `main.go` is now `os.Exit(cli.ExitCode(cli.Execute()))`. | `TestExitCode`, `TestNewRootContext_SIGTERMIsReportedAsSIGTERM`, SIGINT test extended. | `TestExitCode` fails when the code is fixed at 130. |
| Shell rule | `validate.go`: `tools.exec.shell` with exactly one element is rejected. | `TestValidate_ExecShell`. | Fails with the rule removed. |
| C1 | Removed phase/plan wording: `store_impl.go`, `sqlite/dialect_internal_test.go`, `sqlite/migrate_test.go` (two places), `config/load_test.go`. Grep for "phase" in my files is clean. | n/a | n/a |
| C2 | `messages.go`: `UPDATE sessions` now goes through `m.d.Rebind`. | None possible: SQLite's Rebind is the identity. | Not mutation-testable. |
| C5 | `config.IsValidLogLevel` exported; used by `validateLog` and the `--log-level` flag; `cli.isValidLogLevel` deleted. | `TestIsValidLogLevel`; existing `--log-level` tests still pass. | n/a (refactor) |
| C6 | `migrate.go` `migrateWrite`: when no migration was applied and the dialect's legacy version already equals the ledger version, `AfterMigrate` is skipped. | `TestMigrate_SkipsDriverBookkeepingWhenNothingChanged` (counting dialect wrapper; also proves a drifted `user_version` is still re-synced). | Fails with the skip disabled. |

## Docs impact

The docs agent must document:

- **Doctor row rename**: "State dir writable" is now "Storage dir writable". It checks the parent directory of `storage.dsn` and, when set, the directory of `log.file`. It creates nothing; a directory that does not exist yet is judged by its nearest existing ancestor. It no longer looks at `~/.mtclaw`.
- **`cron run --deliver`**: the job's `deliver_to` is validated with the same rules as config load, regardless of `cron.enabled` and `job.enabled`, before any model call. A manual run of a disabled job is still allowed. An empty `chat_id`, the `"*"` key, or an unreachable chat is refused. Fix the sentence in `docs/configuration.md` (`cron.jobs[].enabled`, line ~135) that says a disabled job "cannot deliver anywhere invalid".
- **`cron run` timeout**: `cron.jobs[].timeout` now also bounds a manual run (`docs/configuration.md` line ~137 says "Per-fire turn timeout"; mention manual runs).
- **Exit codes**: an interrupted run exits 128+signal: 130 for SIGINT, 143 for SIGTERM (was 130 for both). Any other failure is 1.
- **DSN escaping**: `storage.dsn` may contain `#`, `?` and `%`; they are literal path characters and the database is created at the exact path with mode 0600.
- **Shell rule**: `tools.exec.shell` must be empty (OS default) or have at least two elements (shell plus its command flag, for example `[/bin/bash, -lc]`); a single element fails validation. Update the `tools.exec.shell` row.
- **Secret files** (`openai.api_key_file`, `channels.telegram.token_file`): surrounding whitespace, including spaces and tabs, is trimmed (was only trailing CR/LF); a file over 64 KiB is a load error (was silently truncated).
- **Multi-document YAML**: a config file with more than one YAML document is rejected; an empty file fails with "config file is empty".
- **`log.file`**: validation now rejects a path that is an existing directory or whose parent cannot be created (`log.file` row currently says an unwritable path fails only at logger construction).
- **`sessions rm`** now reports "no database yet at ..." instead of creating an empty database when `storage.dsn` points at a missing file.
- **Migrations**: developers cannot add two migrations with the same number (build-time load error). Optional maintainer note.
- **Retention story (decision: docs only)**: nothing prunes `messages`, `exec_audit`, `cron_runs`, or `approvals`; only `/new`, `sessions rm` and ephemeral cron cleanup delete. Tool results are stored as message rows (web_fetch up to 1 MiB, read_file 256 KiB, exec 64 KiB by default), so a persistent cron job that fetches pages grows one session without bound, and `sessions show` loads a whole session into memory. Document the unbounded growth, `sessions rm <id>` as the way to drop a session, and a manual `VACUUM` (with the gateway stopped) to reclaim disk after deleting.

## Not done / concerns

- `internal/gateway/e2e_test.go:166` still says "the phase's own spec". I do not own that file; whoever owns internal/gateway should drop the wording. C3 and C4 were left to the other agent as instructed.
- L5 is tested at the `finishRecovery` level, not through `Open`, because a real SQLITE_READONLY_RECOVERY could not be produced in a test.
- L8 uses a hard link (`os.Link`) to publish without overwriting. On a filesystem without hard-link support onboard would fail with a link error instead of writing the config. I did not add a fallback; say if you want one.
- `config.StateDir()` is no longer used by doctor. I left it in place (other code and tests may use it).
- `Held` now takes a shared instead of exclusive flock. Behavior toward a live gateway is unchanged (still reports held).
- The M3 mutation check took ~110 s because the stalled SDK call only gives up at its own 120 s limit; the real test finishes in about 2 s.

## Validation

Run from `/workspace/tiennm99dev/MTClaw`:

- `gofmt -l .` : no output.
- `go build ./...` : ok.
- `go vet ./internal/cli/... ./internal/config/... ./internal/store/... ./internal/logging/... ./internal/version/... ./internal/gateway/... .` : ok.
- `GOOS=windows go vet ./internal/cli/... ./internal/gateway/... .` : ok; `GOOS=windows go vet ./internal/config/... ./internal/store/...` : ok.
- `go test -race -count=1 ./internal/cli/... ./internal/config/... ./internal/store/... ./internal/logging/... ./internal/version/... ./internal/gateway/...` : all ok (cli 5.0 s, config 1.2 s, store 1.3 s, store/sqlite 4.6 s, logging 1.0 s, version 3.0 s, gateway 16.0 s). `internal/config`'s docs-coverage test is green.

Files touched: internal/cli/{cron_cmd,doctor_checks,onboard_cmd,root,sessions_cmd}.go and their tests (`cron_cmd_test`, `doctor_test`, `onboard_test`, `root_test`, `root_unix_test`, `sessions_cmd_test`); internal/config/{load,validate}.go and tests; internal/store/{audit,cron_runs,messages,migrate,store_impl}.go, `store/migrate_internal_test.go` (new), `store/sqlite/{dialect.go,dialect_internal_test.go,migrate_test.go,store_test.go}`; internal/gateway/lock_unix.go and `lock_unix_test.go` (new); main.go.

Status: DONE_WITH_CONCERNS
