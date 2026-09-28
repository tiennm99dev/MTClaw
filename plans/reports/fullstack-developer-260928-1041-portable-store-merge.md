# Portable Store Merge Report

## Task

Merge `origin/feat/portable-store-and-verification` (3 commits, based on an
old `main` from around 2026-08-08) into `merge/portable-store` (which
equaled `main` at `5e844e4`), resolving all conflicts so that main's two
review/fix rounds plus its third round survive on top of the feature
branch's new store layout, config keys, and e2e harness.

## Result

`merge/portable-store` now sits at `8ad3079`, a real two-parent merge
commit (`5e844e4` and `e5ba7eea`). No push was made and `main` was never
touched.

## Conflicts resolved

### Store package (internal/store, internal/store/sqlite)

- `internal/store/migrations/002_approvals_session_index.sql`: a
  file-location conflict (git's rename detection tried to move it under
  `internal/store/sqlite/migrations/`). Resolved by keeping it at the new
  `internal/store/migrations/` path, matching `001_init.sql`'s move.
- `internal/store/approvals.go`, `cron_runs.go`, `sessions.go`: merged the
  feature branch's dialect-driven query bodies with main's later fixes -
  `Create`'s `ExpiresAt` zero-value guard, `ExpirePending`'s `channel`
  filter, and `List`'s `ORDER BY updated_at DESC, id DESC` tiebreaker.
  `SetSummary` was dropped (main removed it from the `SessionStore`
  interface; it was unreferenced everywhere else).
- `internal/store/sqlite/open.go`: deleted by the feature branch (its
  logic split into `dialect.go` + `internal/store/migrate.go`). Ported
  main's fixes into `internal/store/sqlite/dialect.go`'s `Open`: `0o700`
  directory permissions (was `0o755`), the `chmodOwnerOnly` call narrowing
  the db file and its `-wal`/`-shm` sidecars to `0600`, and the pool-size
  invariant comment.
- `internal/store/sqlite/migrate_test.go`: reconciled two incompatible
  API generations in one file (`package sqlite` internal tests calling a
  2-return `Open` vs. `package sqlite_test` calling the new 4-return
  `sqlite.Open`/`store.New`). Kept the feature branch's ledger/legacy-
  adoption tests and ported main's three additions
  (`TestOpen_WriterCreatesFileWithOwnerOnlyPermissions`,
  `TestOpen_ReadOnlyOpenOfUpToDateDatabaseSucceeds`,
  `TestOpen_ReadOnlyOpenOfBehindSchemaDatabaseFails`) onto the new API;
  dropped `TestForeignKeysAreOnForWriterAndReader` as redundant with the
  feature branch's `TestPragmasAndPoolAreSetOnWriterAndReader`. Bumped
  `currentSchemaVersion` from 1 to 2 (main's migration 002 exists now) and
  fixed the legacy-adoption test's assertions, which had assumed only one
  migration ever applies on top of an adopted legacy version.
- `internal/store/sqlite/store_test.go` and several `internal/cli/*_test.go`
  files auto-merged without conflict markers but still called the old
  2-return `sqlite.Open`/`sqlite.New` (leftover from main, since those
  hunks were textually untouched by either branch's diff and git's merge
  picked the stale side). Found via `go vet`/`go build` failures, not via
  conflict markers, and fixed: `internal/store/sqlite/store_test.go` (a
  stray `Open`/`toMillis`/`fromMillis`/`storeDB` that no longer resolve -
  added `newTestStoreWithDB` returning the raw `*sql.DB` alongside the
  `store.Store`, and inlined `time.Time.UnixMilli()`/`time.UnixMilli()`
  instead of the now-unreachable unexported `store` package helpers),
  `internal/cli/approvals_cmd_test.go`, `sessions_cmd_test.go` (3 call
  sites), and `cron_cmd_test.go`.

### Config (internal/config)

- `load.go`: merged `Load`'s doc comment, `expandStorage`'s doc comment
  (dropped its `plan.md` citations, explained the fold/warn behavior
  directly), and `ensureDirCreatable`'s doc comment - feature's version of
  that comment claimed the function creates the directory, which the
  actual (unconflicted) function body never did; kept main's accurate
  description, adapted from `storage.path` to `storage.EffectiveDSN()`.
- `validate.go`: kept feature's `validateStorage` (driver whitelist, then
  the dsn/path both-set check, then the parent-directory-creatable check
  via `EffectiveDSN()`) and re-added main's "must not be empty" guard,
  generalized from `storage.path` to `storage.EffectiveDSN()` - feature's
  version dropped that guard, which still matters for a hand-built
  `Config` that bypasses `Load`'s own default-filling. Also kept main's
  `validateLog` (log.level/log.format enum + case-insensitive/`warning`-
  alias validation), which the feature branch predates.
- `validate_test.go`, `load_test.go`: adapted `TestValidate_StorageParentDir*`
  to set `storage.dsn`; rewrote `TestValidate_StoragePathEmpty` (which set
  `Storage.Path = ""` against a config whose `Storage.DSN` was already
  populated by `validConfig`, so it silently asserted nothing) into
  `TestValidate_StorageDSNEmpty`; merged `load_test.go`'s `captureStderr`
  helper conflict (both sides added distinct content at the same spot, no
  real overlap).

### CLI (internal/cli)

- `root.go`: kept the blank `_ "internal/store/sqlite"` import (needed
  since `store.Open` dispatches through a driver registry) and merged
  `openStore`'s "no database yet" friendly error, changed from
  `os.Stat(cfg.Storage.Path)` to `os.Stat(cfg.Storage.EffectiveDSN())` -
  after `Load`, `Storage.Path` is normalized to empty for the common case,
  so the old check would have always taken the "not found" branch.
- `doctor_checks.go`: kept main's read-only-first `checkDatabase` (never
  retries write-mode except when the file does not exist at all, so
  `doctor` never migrates a database a live gateway still holds), adapted
  to `store.Open`/`EffectiveDSN()`.
- `doctor_test.go`: `TestCheckDatabase_BehindSchemaIsReportedNotSilentlyMigrated`
  used the old 2-return `sqlite.Open` and `cfg.Storage.Path` (empty after
  the merge, since the test config sets `Storage.DSN`); rewrote it to open
  with the new API and simulate "behind schema" by deleting the ledger's
  highest-version row instead of poking `PRAGMA user_version` (which the
  read-only path no longer consults once the ledger exists).
- `send_cmd.go`, `cron_cmd.go`: both had inlined a second, duplicate
  `telegram.SendOnce` call with the token resolved locally; kept the
  shared `state.sendTelegram` helper both call, and made that helper
  forward `Channels.Telegram.APIBaseURL`.
- `onboard_test.go`, `internal/tools/registry_test.go`,
  `internal/tools/exec_test.go`: import-list conflicts (both sides added
  a different import at the same spot); merged to keep both.

### Telegram channel (internal/channel/telegram)

- `channel.go`: main added a redacted `botLogger` for the gateway's own
  bot (`New`) and an `oneShotBot` helper for `SendOnce`/`GetMe`; the
  feature branch added a `newBot(token, apiBaseURL)` helper threading an
  optional Bot API server override through every call site. Merged by
  giving `newBot` an explicit `logger telego.BotOption` parameter -
  `telego.WithLogger(newBotLogger(...))` from `New`, `telego.WithDiscardLogger()`
  from `oneShotBot` - so both fixes compose instead of one silently
  dropping the other.
- `capture.go`: merged `CaptureSenders`' doc comment and added the
  `apiBaseURL` parameter, forwarding through `oneShotBot`.

### Documentation

- `docs/architecture.md`: merged the `store/` package-boundary description
  (feature's post-refactor shape: `Dialect`, `migrate.go`, `factory.go`) with
  main's `version/` and the feature's `testsupport/` package descriptions;
  updated the gateway-lock and config-validation prose from `storage.path`
  to `storage.EffectiveDSN()`.
- `docs/configuration.md`: merged the `config.yml` acceptance intro with
  main's coverage-test caveat; merged the `storage` key table to keep all
  three rows (`driver`, `dsn`, `path`) plus the instance-lock-path detail,
  updated to say "the resolved DSN" instead of `storage.path`.
- `docs/verification.md`: added coverage rows for the store's file-permission
  and read-only-open tests. Replaced stale claims: "MarkdownV2" rendering
  is now "HTML" (main's Telegram rendering changed since this doc was
  written); the "note on `go test -race` and CI" section described a nil-
  approver panic and a CRLF/gofmt failure as open, unfixed bugs - both are
  now fixed (the nil-approver guard is in `exec.go`; LF line endings are
  pinned via `.gitattributes`), so the section was rewritten to state that
  plainly instead of leaving a false "not fixed here" claim in the repo.
  Removed a dangling, unlinked "phase 9 implementation report" citation.

## Other fixes made during the merge (not git conflicts, but broken by it)

`storage.path` stopped being reliably populated once `storage.dsn` became
the canonical field (`Load` always folds a legitimate path-only config
into `DSN` and clears `Path`). Several call sites still read `Storage.Path`
directly and would have silently broken for any config using `storage.dsn`
(the new default):

- `internal/gateway/lock.go`'s `LockPath` (`cfg.Storage.Path + ".lock"` ->
  `cfg.Storage.EffectiveDSN() + ".lock"`) - this is the one the task
  specifically flagged; the instance lock would have collapsed to a
  literal `.lock` file for any config using the new key.
- `internal/cli/cron_cmd.go`'s `cron list` existence probe
  (`os.Stat(s.cfg.Storage.Path)` -> `os.Stat(s.cfg.Storage.EffectiveDSN())`).

Both were found by reading every non-test `Storage.Path` reference after
the conflict-driven changes, not by a failing test - the existing test
suite did not happen to exercise a `storage.dsn`-only config through
either path.

Removed `plan.md`/finding-code citations (`plan.md's R7/R8/R13/A6/A7`,
"phase 4/9", "finding A4") from code and test comments across
`internal/store/{dialect,factory,migrate}.go`, `internal/store/sqlite/{dialect,dialect_internal_test}.go`,
`internal/store/factory_test.go`, `internal/testsupport/{homedir,fakeapi/telegram_spike_test}.go`,
`internal/gateway/e2e_test.go`, and `internal/config/{types,load,validate}.go`,
replacing each with the invariant or behavior it was actually citing.

## Validation

- `gofmt -l .`: clean.
- `go vet ./...`, `GOOS=windows go vet ./...`, `GOOS=darwin go vet ./...`: clean.
- `go test -race -count=1 ./...`: all packages pass (one real failure found
  and fixed along the way - see below).
- `CGO_ENABLED=0 go build .` for `linux/amd64`, `windows/amd64`,
  `darwin/arm64`: all succeed.
- `go mod tidy -diff`: no diff.

### `TestExec_NilApproverFailsClosedInsteadOfPanicking` failure and fix

This test (from the feature branch's nil-approver fix) failed under
`-race`: it set `Allow: [".*"]` and an unterminated-quote command, expecting
a go-shellwords tokenize failure to force `VerdictAsk` regardless of the
allow-list match. Main's current `Policy.Evaluate` matches deny/allow
against the *raw* command string, never a tokenized one - `go-shellwords`
is no longer imported anywhere in `internal/tools` production code - so the
allow-all rule matched immediately and the command actually ran (and failed
with a bash syntax error), never reaching the approver at all. This is
exactly the "process-tree test reconciliation" the task called out: the
nil-approver *fix* (`exec.go`'s `if approver == nil { approver = DenyAllApprover{} }`)
is still correct and still reachable, but the *test*'s premise for reaching
`VerdictAsk` was stale. Rewrote it to use the default config (no deny/allow
rule, `mode: approval`) and an ordinary command (`echo hi`), which reaches
`VerdictAsk` through the normal "no rule matched" path - the same
fail-closed guard, exercised the way it actually gets hit in production.

### Database upgrade check

Built main's pre-merge HEAD (`5e844e4`) in a scratch `git worktree`, wrote a
small throwaway seed program there (not committed) using main's own
`internal/store/sqlite.Open`/`New` to create a database with one session,
two messages, one exec_audit row, and one pending approval - `PRAGMA
user_version` came out at `2` (both pre-merge migrations applied, no
`schema_migrations` table, as expected for a pre-cutover database).

Ran the merged binary (`sessions rm <nonexistent-id>`, a write-mode command)
against that database:

- `schema_migrations` now has exactly two rows: `(1, '001_init.sql')` -
  adopted from the legacy `PRAGMA user_version` - and `(2,
  '002_approvals_session_index.sql')` - applied on top in the same open,
  since the adopted version (1) was behind the latest embedded migration
  (2).
- `PRAGMA user_version` stayed at `2`, in sync with the ledger.
- All seeded data survived: the session, both messages, the exec_audit row,
  and the pending approval, byte-for-byte.
- The `approvals.session_id` index exists exactly once (no duplicate from
  a double-apply).
- Reopening the database again (`sessions list`, `doctor`) left the ledger
  unchanged and reported "opens and is at the current schema version";
  `doctor`'s "Instance lock" check correctly derived the lock path from the
  resolved `storage.dsn`.

The scratch worktree, its build artifacts, and the seed binary were removed
after the check; no background processes were left running.

## Unresolved questions

None. Every conflict listed above resolved in favor of main's reviewed
behavior with the feature branch's new capability layered on top, per the
task's stated precedence rule.
