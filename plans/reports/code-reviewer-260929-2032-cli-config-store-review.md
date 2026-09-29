# Review: cli / config / store / logging / version / testsupport

Date: 2026-09-29. Tree: `main` at `e8b0b74` (clean). Read-only review.
Scope: `internal/cli` (+`prompts`), `internal/config`, `internal/store`
(+`sqlite`, `migrations`), `internal/logging`, `internal/version`,
`internal/testsupport` (+`fakeapi`), `main.go`, `Makefile`. I read the lock
code in `internal/gateway/lock*.go` only because the CLI calls it.

I checked every finding below against current source. None of them repeats
an item fixed or accepted in
`fullstack-developer-260928-1041-final-review-fixes.md` or
`fullstack-developer-260928-1041-portable-store-merge.md`.

How I probed: I copied `HEAD` into the scratchpad with `git archive` and
built `mtclaw` from it with `CGO_ENABLED=0`. I also built a small
`cmd/fakes` program that runs `fakeapi.NewOpenAI` and `fakeapi.NewTelegram`,
and pointed `openai.base_url` and `channels.telegram.api_base_url` at them. I
used a separate `HOME` for each case. The fake process was stopped afterward.

## High

None found.

## Medium

### M1. A `storage.dsn` containing `#`, `?` or `%` puts the database at the wrong path, readable by everyone
`internal/store/sqlite/dialect.go:201` builds the DSN as
`"file:" + filepath.ToSlash(path) + "?" + q` and never escapes the path.
SQLite treats `file:` as a URI, so:
- `#` starts a fragment,
- `?` starts the query string,
- `%XX` is decoded.

Probe with `dsn: <dir>/d#1/mtclaw.db`:
- `mtclaw prompt hi` exits 0 but writes the database to `<dir>/d`.
- That file has mode `-rw-r--r--`. `chmodOwnerOnly(dsn)` (`dialect.go:161`,
  `:175-178`) chmods the configured path, which does not exist, so the
  real file never gets `0600`.
- `sessions list` then reports "no database yet".
- The instance lock is still created at `<dir>/d#1/mtclaw.db.lock`, which is
  not next to the real database. Two configs whose paths truncate to the
  same prefix would share one database behind two different locks.
- `?` behaves the same way.
- `%41` fails with "unable to open database file (14)".

Confidence: **Confirmed by running.**

Fix: percent-encode the path before adding the query string, for example
`strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(filepath.ToSlash(path))`.
A scratch probe with modernc confirmed this creates `a#1/t.db`, `b?x/t.db`
and `c%41/t.db` at their exact paths. Add a regression test that opens a
path containing each of the three characters and asserts the file exists at
that exact path with mode `0600`.

### M2. `cron run --deliver` sends to chats that config validation would reject
`internal/config/validate.go:276` checks `deliver_to` reachability only when
`cfg.Cron.Enabled && job.Enabled`. `deliverCronResult`
(`internal/cli/cron_cmd.go:253-261`) checks only that the channel is
`telegram`, then sends to whatever `ChatID` is configured. `cron run` does
not check `cron.enabled` or `job.Enabled` either.

Probe: `cron.enabled: false`, `allow_from: [111]`, job
`deliver_to.chat_id: "999999"`.
- `config validate` prints OK.
- `mtclaw cron run j1 --deliver` prints "delivered to chat 999999" and exits
  0.
- The fake Telegram server recorded `sendMessage chat_id:999999`.

This bypasses the exact failure the check exists to prevent ("an outbound
message to an arbitrary chat"). `docs/configuration.md:135` says the
opposite: "a job that can never fire cannot deliver anywhere invalid either".
An empty `chat_id` also gets as far as an API call.

Confidence: **Confirmed by running.**

Fix: export the existing check (for example
`config.ValidateCronDeliverTo(cfg, job) error`, wrapping
`validateCronDeliverTo`). Call it in `cron run` when `--deliver` is set,
before the turn runs. Fix the doc sentence.

### M3. `cron run` ignores `cron.jobs[].timeout`
`cron_cmd.go:157` calls `loop.Run(ctx, ...)` with the root command context,
which has no deadline. The scheduled path does apply it:
`internal/gateway/dispatch.go:489-490` wraps the turn in
`context.WithTimeout(d.rootCtx, in.Timeout)`.

So a manual run (for example from a system crontab:
`mtclaw cron run job --deliver`) is bounded only by per-request limits.
With the defaults that is up to 20 iterations × 120s `openai.timeout`, plus
exec timeouts. This contradicts `docs/configuration.md:137`, which describes
it as the "Per-fire turn timeout", and the command's claim to fire "the same
job".

Confidence: **Confirmed by reading.**

Fix: `runCtx, cancel := context.WithTimeout(ctx, job.Timeout.Std())` around
`loop.Run`. Still record the run under `context.WithoutCancel(ctx)` as now.
Add a test with a fake OpenAI that stalls longer than a 1s job timeout.

### M4. Doctor's "State dir writable" check tests `~/.mtclaw` even when nothing uses it, creates it, and can fail doctor
`doctor_checks.go:37` always resolves `config.StateDir()`, and `:96` runs
`os.MkdirAll(stateDir, 0o700)`. The database and lock actually live at
`filepath.Dir(cfg.Storage.EffectiveDSN())`, and `checkDatabase` already
covers that directory.

Probe: `HOME` set to a mode-555 directory, `storage.dsn` elsewhere.
- Doctor prints `FAIL State dir writable ... permission denied`.
- On the next row it prints "DB ... created and migrated" and "Instance
  lock" OK.
- Doctor exits 1 for a directory the install never uses.
- On a normal home it creates an empty `~/.mtclaw` as a side effect.

Confidence: **Confirmed by running.**

Fix: point the check at `filepath.Dir(cfg.Storage.EffectiveDSN())` and at
`filepath.Dir(cfg.Log.File)` when that is set. Drop the `MkdirAll`, or keep
it only for the DSN directory, which `checkDatabase` creates anyway. Rename
the check, for example "Storage dir writable". This also covers L1.

### M5. Stored history and audit tables grow without bound, and this is not documented
Nothing prunes `messages`, `exec_audit`, `cron_runs` or `approvals`. The
only deletes are `/new` (`gateway.go:309`), `sessions rm`, and ephemeral
cron cleanup. No `VACUUM` or `auto_vacuum` is set anywhere.

Tool results are persisted as message rows. The default caps are:
- web_fetch: 1 MiB (`defaults.go:58, 64`)
- read_file: 256 KiB
- exec: 64 KiB

A persistent daily cron job that fetches one page adds up to about
365 MiB per year to one session. The agent only ever reads the tail
(`Recent(maxTurns*8)`), but `sessions show` loads the whole session into
memory (`sessions_cmd.go:94`, `Recent(ctx, id, 0)`).

Confidence: the absence of pruning is **Confirmed by reading**. The growth
rate is **Plausible**; it depends on actual tool usage.

This is a product decision, so these are options only:
- (a) Document the growth, plus `sessions rm` and a manual `VACUUM`, as the
  retention story.
- (b) Add `storage.retention` and prune at gateway startup, next to
  `ExpirePending` and `ExpireStarted`.
- (c) Have `sessions show` page through history (`--limit`).

## Low

- **L1. A bad `log.file` passes `config validate` and doctor, then breaks
  every full command.** With `log.file` pointing at a directory:
  `config validate` prints OK and doctor has no log check, but `prompt`
  fails with `open log file ...: is a directory` (exit 1). `validateStorage`
  checks its own parent directory; `log.file` gets no check at all
  (`validate.go:337-348`, `logging/logger.go:28-40`).
  *Confirmed by running.* Fix: in `validateLog`, run the same
  `ensureDirCreatable(filepath.Dir(file))` check and reject an existing
  directory, or add a doctor row for it.
- **L2. `deliver_to.chat_id: "*"` passes validation.** `cronChatIDReachable`
  (`validate.go:311`) accepts any key in `tg.Groups`, and the default
  `Groups` map always contains `"*"`. With `cron.enabled: true`,
  `config validate` prints OK, and every fire would then fail at
  `sendMessage`. *Confirmed by running.* Fix: skip the `"*"` key in the
  group lookup.
- **L3. Only the first YAML document is read; the rest are silently
  ignored.** `load.go:71-72` calls `Decode` once. A file ending in
  `---\nversion: 99\nnotakey: 1` loads as OK, which gets around
  unknown-key rejection. An empty file fails with the unhelpful
  `parse config: EOF`. *Confirmed by running.* Fix: after the first
  `Decode`, call `Decode` again into a throwaway value and require
  `io.EOF`. Map an empty file to "config file is empty".
- **L4. Secret files are silently truncated and only partly trimmed.**
  `load.go:168` caps the read at 64 KiB with no error, so a larger file
  yields a truncated key. `load.go:173` trims only `\r\n`, so a trailing
  space or tab survives into the Authorization header or bot URL, and the
  user gets a confusing 401 or "invalid token format".
  *Confirmed by reading.* Fix: read `maxSecretFileBytes+1` and error if it
  is exceeded; use `strings.TrimSpace`.
- **L5. A read-only open can silently migrate the database.**
  `sqlite/dialect.go:127-133`: on `SQLITE_READONLY_RECOVERY`, `Open` sets
  `readOnly = false` and runs a write-mode `Migrate`. Doctor
  (`doctor_checks.go:140` area and its doc comment) and `sessions list`
  promise they never migrate, but after a crashed writer they will, possibly
  under a gateway that is still running. *Plausible*: this needs a
  crashed-writer WAL to trigger. Fix: in the fallback, open read-write only
  to finish recovery, then run `Migrate(..., write=false)`. Or return the
  recovery error to doctor.
- **L6. `sessions rm` (and any write-mode open) creates a new database for a
  mistyped `storage.dsn`.** Probe: `sessions rm abc` against a missing
  `typo/sub/mtclaw.db` created both directories and a migrated database,
  then printed "session abc not found". *Confirmed by running.* Fix:
  `sessions rm` should use the same "no database yet" stat guard as the
  read-only path (`root.go:129-133`).
- **L7. Lock probing can make a starting gateway refuse to run, and lock
  errors are reported as "another instance".** `Held`
  (`gateway/lock_unix.go:72-90`) briefly takes `LOCK_EX`. If a gateway calls
  `Acquire` (`:21-39`) at that same moment, it fails with "another instance is
  already running (pid <old pid>)", for example when a `doctor` or
  `cron run` coincides with a systemd restart. Separately, both functions
  treat any flock error (for example `ENOLCK` on NFS) as "held".
  *Plausible.* Fix: treat only `EWOULDBLOCK` as held and return other
  errors. In `Acquire`, retry `EWOULDBLOCK` a few times over about 100ms.
- **L8. `onboard` can leave behind a half-written config that blocks the
  next run.** `onboard_cmd.go:441-452`: if `f.Write` fails (disk full), the
  truncated file stays, and the `Close` error is dropped. The next `onboard`
  then refuses ("already exists") and every load fails to parse.
  *Confirmed by reading.* Fix: write to a temp file in the same directory
  with `0600`, then `os.Link` or `os.Rename` it into place. Or remove the
  file on any write error, and check `Close`.
- **L9. Ties in audit and cron-run listings come back in no fixed order.**
  `audit.go:46` (`ORDER BY created_at DESC`) and `cron_runs.go:78`
  (`ORDER BY started_at DESC`) have no tie-break. Rows written in the same
  millisecond come back in unspecified order, and `cron list` can show the
  wrong "last" run. This is the same issue already fixed for sessions.
  *Confirmed by reading.* Fix: append `, id DESC`.
- **L10. Duplicate migration version numbers are silently skipped.**
  `migrate.go:72, 110`: with `003_a.sql` and `003_b.sql`, both sort to
  version 3. After `003_a` applies, `current` is 3 and `003_b` is skipped on
  every database, with no error. Only a developer can trigger this.
  *Confirmed by reading.* Fix: reject duplicate versions in
  `loadMigrations` and add a unit test.
- **L11. Exit code 130 is also used for SIGTERM.** `main.go:21-25` maps
  `ErrInterrupted` to 130 for both signals; SIGTERM is conventionally 143.
  *Confirmed by reading.* Fix: record which signal fired in
  `newRootContext` and exit with 128 plus that signal's number. Or document
  that 130 means "any interrupt".

## Cleanup (ranked below real defects)

- **C1. Comments still cite a phase**, which breaks the "no phase/plan IDs"
  rule:
  - `internal/store/store_impl.go:21` ("added in a later phase", which is
    also stale because `store.Open` now exists)
  - `internal/testsupport/fakeapi/telegram.go:36`
  - `internal/store/sqlite/dialect_internal_test.go:23`
  - `internal/store/sqlite/migrate_test.go:237` and `:288`
  - `internal/config/load_test.go:453`
- **C2. One query skips the dialect's placeholder rewriting.**
  `messages.go:64` runs `UPDATE sessions SET updated_at = ?` without
  `m.d.Rebind`; every other query in the package uses it. It works on
  SQLite and breaks on a future Postgres dialect.
- **C3. Stale fake comment.** `fakeapi/telegram.go:201` still describes the
  "MarkdownV2" fallback that "only triggers on ... 'parse'". The code now
  uses HTML and falls back on any 400 while a parse mode is set.
- **C4. Misleading `...Locked` names.** `pushLocked` and
  `nextMessageIDLocked` (`fakeapi/telegram.go:181, 191`) take the lock
  themselves; by Go convention the suffix means the caller already holds
  it. `PushCallback` also reads `nextUpdateID+1` and pushes under two
  separate lock sections, so a concurrent push can mislabel the callback id.
  Rename to `push` / `nextMessageID` and assign the id inside `push`.
- **C5. Log-level validation is written twice.** `isValidLogLevel`
  (`cli/root.go:314`) repeats the switch in `config.validateLog`
  (`validate.go:337`). Export one `config.IsValidLogLevel` and call it from
  both places.
- **C6. Every write-mode open writes the database even when nothing
  changed.** `AfterMigrate` runs `PRAGMA user_version = N` on every
  write-mode open (`migrate.go:119`, `sqlite/dialect.go:97-101`), and
  `bootstrapLedger` takes `BEGIN IMMEDIATE`. So each `prompt`, `cron run`
  and `sessions rm` takes the write lock twice even when the schema is
  current. Skip `AfterMigrate` when nothing was applied and the legacy
  version already equals `current`.

## Test gaps on risky behavior

- DSN paths containing URI metacharacters (M1): no test.
- `cron run --deliver` and `cron run` timeout behavior (M2, M3): no test.
- A migration that fails partway (a bad SQL file), asserting both the
  schema and the ledger roll back: no test. The code is correct, since the
  DDL and the ledger insert share one transaction in `applyMigration`, but
  that correctness is not pinned by a test.
- Two write-mode `Open` calls racing the bootstrap on a new file (the
  "BEGIN IMMEDIATE serializes adoption" claim in `migrate.go:125-134`): no
  test.
- The read-only recovery fallback (L5): only the error-code classifier is
  tested, not what `Open` does after it.

## Verified non-issues

- **Token in one-shot Telegram errors:** `GetMe` and `SendOnce` errors
  against a refused connection contain no bot token (probed:
  "fasthttp do request: error when dialing 127.0.0.1:1").
- **SQL injection:** every value is a bound `?` parameter. The only dynamic
  SQL is `PRAGMA user_version = %d` (an int) and fixed DDL tokens. No
  identifier ever comes from user input.
- **Migration atomicity:** DDL and the ledger insert commit in one
  transaction (`migrate.go:208-223`), and SQLite DDL is transactional, so a
  failure partway through rolls back cleanly and is retried on the next
  open.
- **Duplicate YAML keys** are rejected (probed). The secret precedence
  "env var wins over file" is documented (`docs/configuration.md:62`).
- **Permissions:**
  - config: `0600` with `O_EXCL`
  - log file: `0600`, and existing files are narrowed to it
  - DB, `-wal`, `-shm`: `0600`
  - new directories: `0700`
  - lock file: `0644`, but it holds only a pid
- **Stale locks:** the lock is flock-based and released by the kernel, and
  the file's content is ignored. No stale-lock heuristic is needed.
- **Makefile `VERSION` interpolation:** only someone who can push tags can
  inject through it, and they can already edit the workflow. Not a new
  trust boundary.

## Unresolved questions

1. M5: should retention be documented only, or become a feature? This is a
   product call.
2. M2: should `cron run` on a disabled job, or with `cron.enabled: false`,
   be refused outright, or only have its `deliver_to` validated?

Status: DONE
