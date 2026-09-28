# Phase 2: Portable Migration Ledger and Dialect Seam

**Status:** Completed · **Effort:** 8h · **Blocks:** phase 3 · **Blocked by:** phase 1

Move every SQLite-specific construct below a `store.Dialect` seam and replace
`PRAGMA user_version` with a portable `schema_migrations` table, without
changing `store.Store`, without changing behaviour, and without breaking a
single existing database file.

## Context

- Interfaces that must not change shape: `internal/store/store.go:23-110`.
  Row structs: `internal/store/types.go`.
- The ten portability blockers, each verified:

| # | Blocker | Evidence |
|---|---|---|
| 1 | `PRAGMA user_version` as the migration ledger | `internal/store/sqlite/open.go:213`, `:247` |
| 2 | `AUTOINCREMENT` | `internal/store/sqlite/migrations/001_init.sql:17,48,62` |
| 3 | `?` placeholders throughout | every query file, e.g. `sessions.go:41`, `messages.go:38` |
| 4 | `ON CONFLICT (...) DO UPDATE ... RETURNING` | `internal/store/sqlite/sessions.go:26-32` |
| 5 | SQLite-only connection setup (`_pragma=`, WAL, `busy_timeout`, `_txlock=immediate`, `SetMaxOpenConns(1)`) | `open.go:84`, `:108-116`, `:142-154` |
| 6 | Driver-specific error inspection (`*sqlitedriver.Error`, code 264) | `open.go:32`, `:156-159` |
| 7 | No store factory; `sqlite.Open`/`sqlite.New` wired directly | `cli/root.go:60,64`, `cli/doctor_checks.go:120,122`, `gateway/gateway.go:67,72` (phase 3) |
| 8 | INTEGER unix-millis + TEXT ids need explicit type mapping | `migrations/001_init.sql:11-12`, `convert.go:16-30` |
| 9 | **`LIMIT -1` as "unlimited"** is SQLite-specific | `sessions.go:50-53`, `messages.go:66-72`, `audit.go:43-47`, `cron_runs.go:56-67` |
| 10 | **`LastInsertId`** is unsupported by the standard Postgres driver | `audit.go:33-37`, `cron_runs.go:29-34` |

- Behaviour that must survive the move, verbatim: WAL, `busy_timeout=5000`,
  `foreign_keys=ON`, `synchronous=NORMAL` (`open.go:109,143-148`);
  `SetMaxOpenConns(1)` on writers (`open.go:79-85`); `_txlock=immediate`
  (`open.go:113-114`); the `SQLITE_READONLY_RECOVERY` read-only→read-write
  fallback (`open.go:60-77`, `:156-159`); refusing a newer schema
  (`open.go:216-218`); the "behind the latest migration" read-only error
  (`open.go:222-224`).
- Existing coverage that must keep passing (moved, not rewritten):
  `internal/store/sqlite/store_test.go` (17 tests, `:27-379`) and
  `migrate_test.go` (`:13-97`).

## Requirements

1. `internal/store/sqlite/` ends up containing only driver quirks and a
   `store.Dialect` implementation — no MTClaw table names in SQL.
2. `schema_migrations` replaces `PRAGMA user_version`, with a real adoption
   path for existing v1 databases and no schema re-run.
3. Blockers 9 and 10 fixed while the code is open: unlimited means no `LIMIT`
   clause; all identity inserts use `RETURNING`.
4. Zero new module dependencies. `CGO_ENABLED=0` preserved.
5. `go test -race ./...` green, **including phase 1's e2e suite**, which is the
   real regression signal for this phase.

## Files

**Create**

| Path | Contents |
|---|---|
| `internal/store/dialect.go` | The `Dialect` interface and the DDL token renderer. |
| `internal/store/migrate.go` | `//go:embed migrations/*.sql`, migration loading (moved from `open.go:161-196`), ledger bootstrap, legacy adoption, apply loop. |
| `internal/store/migrations/001_init.sql` | Moved from `sqlite/migrations/`, with `{{IDENTITY}}` / `{{INT64}}` tokens. |
| `internal/store/sessions.go` `messages.go` `approvals.go` `audit.go` `cron_runs.go` `convert.go` `ids.go` | Moved from `sqlite/`, SQL rebound through the dialect. |
| `internal/store/sqlite/dialect.go` | Everything left of today's `open.go`: driver name, DSN builder, pragmas, pool config, read-only recovery, error classification, legacy `user_version` read/adopt/sync, and `Dialect` implementation + `init()` registration. |
| `internal/store/sqlite/testdata/v1_legacy_schema.sql` | Byte-frozen copy of today's `migrations/001_init.sql`. Never edited again; it is the definition of "what v1 wrote". |

**Modify**

| Path | Change |
|---|---|
| `internal/store/store.go` | No interface changes. Add only a package doc sentence pointing at `dialect.go`/`factory.go`. |
| `internal/store/sqlite/store_test.go` | `package sqlite` → `package sqlite_test`; construct via the sqlite opener; unexported-helper uses replaced with exported equivalents. Assertions unchanged. |
| `internal/store/sqlite/migrate_test.go` | Same package change; `PRAGMA user_version` assertions become `schema_migrations` assertions, **plus** one retained `user_version` assertion for the downgrade-guard sync. Add the legacy-adoption test. |
| `internal/agent/loop_test.go:25-28` | Store construction updated (see step 8). |
| `internal/cron/scheduler_test.go:31-34` | Same. |
| `internal/gateway/gateway_test_helpers_test.go:26-29` | Same. |
| `internal/tools/exec_test.go:31-34` | Same. |
| `internal/tools/registry_test.go:50-53` | Same. |
| `internal/cli/doctor_test.go:85` | Same. |

**Delete**

`internal/store/sqlite/open.go`, `sessions.go`, `messages.go`, `approvals.go`,
`audit.go`, `cron_runs.go`, `convert.go`, `id.go`,
`internal/store/sqlite/migrations/001_init.sql` (and the now-empty directory).

## Design

### `store.Dialect`

Minimal by construction: a method exists only where today's code already
varies by driver. No `Postgres`/`MySQL` constants, no unused methods.

```go
// internal/store/dialect.go
type Dialect interface {
    // Name is the driver name from storage.driver.
    Name() string

    // Rebind rewrites '?' placeholders into the driver's native form.
    Rebind(query string) string

    // DDLTokens maps migration template tokens to this dialect's SQL types.
    // Required keys: "{{IDENTITY}}", "{{INT64}}".
    DDLTokens() map[string]string

    // LegacyVersion reports a pre-schema_migrations version recorded by an
    // older MTClaw (SQLite: PRAGMA user_version). 0 means "no legacy state".
    LegacyVersion(ctx context.Context, q Querier) (int, error)

    // AfterMigrate runs once after a successful write-mode migrate, for
    // driver bookkeeping that is not part of the portable schema.
    AfterMigrate(ctx context.Context, db *sql.DB, applied int) error
}
```

Rejected from the interface, with reasons recorded in the file's doc comment:

- *Upsert form* — `INSERT ... ON CONFLICT (...) DO UPDATE ... RETURNING` is
  shared by SQLite ≥3.35 and Postgres, and `sessions.Ensure`
  (`sqlite/sessions.go:26-32`) already proves it works on
  `modernc.org/sqlite`. One portable statement beats a renderer with one
  caller.
- *Identity-insert strategy* — resolved by policy instead: **everything uses
  `RETURNING id`**. This forecloses MySQL, which is not a target (plan R13).
- *Limit clause* — resolved by omitting `LIMIT` when the caller wants no
  limit, which is portable everywhere.

### Migration ledger

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at {{INT64}} NOT NULL
);
```

Write-mode algorithm (one transaction for bootstrap + adoption, one
transaction per subsequent migration, matching today's `applyMigration`,
`open.go:237-251`):

1. `CREATE TABLE IF NOT EXISTS schema_migrations`.
2. `SELECT version FROM schema_migrations` → applied set.
3. If the set is empty and `dialect.LegacyVersion() = L > 0`: insert rows
   `1..L` (`name` from the embedded file list, `applied_at = now`). Commit.
   **No DDL is executed** — the schema already exists.
4. `max(applied) > max(embedded)` → refuse, reusing today's wording
   ("newer than this binary supports…", `open.go:217`).
5. Apply each embedded migration above `max(applied)`, each in its own
   transaction, inserting its ledger row inside that same transaction.
6. `dialect.AfterMigrate(db, max(applied))` → SQLite sets
   `PRAGMA user_version = <applied>`, preserving the v1 binary's own
   downgrade guard (see plan "Decision: migration-ledger cutover").

Read-mode: never writes. If `schema_migrations` is absent, fall back to
`dialect.LegacyVersion()`. Same two errors as today: newer-than-supported →
refuse; behind → "open it for writing once to initialize it"
(`open.go:222-224`).

## Implementation steps

1. **Freeze the v1 schema.** Copy `internal/store/sqlite/migrations/001_init.sql`
   verbatim to `internal/store/sqlite/testdata/v1_legacy_schema.sql`. Header
   comment: "frozen copy of what MTClaw v1 wrote; never edit".
2. **Write `dialect.go`** with the interface above plus
   `renderDDL(sql string, tokens map[string]string) string` (a
   `strings.NewReplacer`) and a `Querier` alias covering `*sql.DB`/`*sql.Tx`.
3. **Write `sqlite/dialect.go`.** Move, unchanged in behaviour:
   `driverName` (`open.go:25`), `sqliteReadOnlyRecovery` + `isReadOnlyRecovery`
   (`open.go:27-32,156-159`), `dsn` (`open.go:108-116`), `openHandle`
   (`:118-132`), `applyPragmas` (`:134-154`), the `MkdirAll` + recovery-retry
   logic (`:60-92`). Export:
   ```go
   func Open(ctx context.Context, dsn string, readOnly bool) (*sql.DB, store.Dialect, bool, error) // bool = effective readOnly
   ```
   `DDLTokens` returns `{"{{IDENTITY}}": "INTEGER PRIMARY KEY AUTOINCREMENT", "{{INT64}}": "INTEGER"}` —
   deliberately identical to what v1 emitted, so a fresh database is
   byte-comparable with a legacy one. `LegacyVersion` reads
   `PRAGMA user_version`. `AfterMigrate` writes it back.
   `Rebind` is the identity function, with a comment saying so explicitly.
4. **Move the migration file** to `internal/store/migrations/001_init.sql` and
   tokenize: the three `INTEGER PRIMARY KEY AUTOINCREMENT` columns
   (`:17,48,62`) become `{{IDENTITY}}`; every unix-millis column
   (`created_at`, `updated_at`, `expires_at`, `decided_at`, `started_at`,
   `finished_at`) becomes `{{INT64}}`. `prompt_tokens`/`completion_tokens`/
   `exit_code`/`truncated`/`seq` stay `INTEGER`. Nothing else changes — no
   reformatting, so `git diff -M` stays reviewable.
5. **Write `migrate.go`.** Move `loadMigrations` (`open.go:161-196`) and
   `migration`/`migrationNamePattern`, then implement the algorithm above.
   Render each migration's SQL through `renderDDL` before executing.
6. **Move the query files.** For each of `sessions.go`, `messages.go`,
   `approvals.go`, `audit.go`, `cron_runs.go`, `convert.go`, `id.go`
   (→ `ids.go`): change `package sqlite` → `package store`, drop the
   `store.` qualifier on row types, and route every statement through
   `d.Rebind(...)`. Three substantive edits, and only three:
   - **Unlimited lists.** Replace `if limit <= 0 { limit = -1 }` +
     `LIMIT ?` with: build the query without a `LIMIT` clause when
     `limit <= 0`, else append `LIMIT ?` and the argument. Applies at
     `sessions.go:50-53`, `messages.go:66-72`, `audit.go:43-47`,
     `cron_runs.go:56-67`.
   - **Identity inserts.** `audit.go:24-38` and `cron_runs.go:21-35` switch
     from `ExecContext` + `LastInsertId` to
     `QueryRowContext(... RETURNING id).Scan(&a.ID)`.
   - **Struct plumbing.** The five store structs hold `db *sql.DB` and
     `d Dialect` instead of `db *DB`.
   Everything else — column lists, `WHERE state='pending'` guards
   (`approvals.go:75`), the `MAX(seq)` transaction (`messages.go:26-63`),
   the descending-then-reverse trick (`messages.go:93-98`), error wrapping,
   `ErrNotFound`/`ErrAlreadyDecided` mapping — is copied unchanged.
7. **Wire the concrete store.** In `internal/store`, add the aggregate type
   (today's `sqlite.Store`, `open.go:253-270`) holding `*sql.DB` + `Dialect`
   and returning the five sub-stores. Its constructor stays unexported;
   phase 3's factory is the public entry point. To keep phase 2 compiling on
   its own, add a temporary internal constructor and have `sqlite` **not**
   reference it — the three production call sites still use the old path until
   phase 3, so phase 2 must keep `sqlite.Open` returning something those sites
   can use. Concretely: phase 2 adds `store.New(db *sql.DB, d Dialect) Store`
   (exported), and the three production call sites become
   `db, dia, _, err := sqlite.Open(...)` + `store.New(db, dia)`. Phase 3 then
   replaces those three with `store.Open(cfg, readOnly)`.
8. **Update the six test helpers** listed in Files/Modify to
   `db, dia, _, err := sqlite.Open(ctx, path, false)` + `store.New(db, dia)`,
   keeping their existing `t.Cleanup` close.
9. **Convert the two sqlite test files** to `package sqlite_test`. This is
   required, not stylistic: an in-package test importing `internal/store`
   would create a cycle through `sqlite`'s own dependency on `store`
   (plan finding A7). Do not resolve it by duplicating queries.
10. **Add the three new migration tests** to `migrate_test.go`:
    - *Legacy adoption.* Create a DB by executing `testdata/v1_legacy_schema.sql`
      and `PRAGMA user_version = 1` on a raw handle; insert one session row;
      close; open through the new path. Assert: `schema_migrations` has exactly
      one row (version 1), the session row still reads back, and no table was
      recreated (`sqlite_master` DDL for `sessions` is unchanged).
    - *Ledger newer than binary.* Insert `version = max+1` into
      `schema_migrations`; assert both read-only and read-write opens are
      refused with "newer than this binary supports".
    - *Downgrade sync.* After a fresh migrate, assert `PRAGMA user_version`
      equals the highest applied version.
11. **Re-verify the preserved invariants** by keeping and, where absent,
    adding assertions: WAL + `busy_timeout` + `foreign_keys` on both handles
    (extends `migrate_test.go:79-97`), writer pool capped at 1
    (`db.Stats().MaxOpenConnections == 1`), read-only fallback still upgrades
    on `SQLITE_READONLY_RECOVERY` (keep whatever coverage exists; if there is
    none, assert `isReadOnlyRecovery` classifies the code — do not fabricate a
    crashed-writer WAL in a unit test).
12. **Run the grep-based acceptance checks** from `plan.md` before declaring
    done. They are the substitute for the second backend nobody is building.

## Tests / validation

```powershell
go test -race ./internal/store/...            # narrowest first
go test -race ./internal/gateway/ ./internal/cli/   # phase 1's e2e net
go test -race ./...
$env:CGO_ENABLED=0; go build ./...
git diff go.mod go.sum                        # must be empty

# mechanical seam checks (see plan.md acceptance criteria)
rg -n "PRAGMA" internal/store/*.go
rg -n "AUTOINCREMENT" internal/store/migrations/
rg -n "LastInsertId|LIMIT -1" internal/store/
rg -n "sessions|messages|approvals|exec_audit|cron_runs" internal/store/sqlite/*.go
rg -n "modernc.org/sqlite" internal/ --glob '*.go'
```

Manual upgrade rehearsal (do this once, by hand, before marking the phase
done):

```powershell
# 1. build the CURRENT main binary first, create a real database with it
git stash; go build -o mtclaw-v1.exe .; git stash pop
$env:MTCLAW_CONFIG="$env:TEMP\mtclaw-upgrade\config.yaml"
.\mtclaw-v1.exe prompt "hello"     # or: mtclaw doctor, enough to create the DB
# 2. build the new binary and open the same file
go build -o mtclaw-v2.exe .
.\mtclaw-v2.exe sessions list      # must list the v1 session, no migration error
# 3. confirm the ledger and the downgrade guard
.\mtclaw-v1.exe sessions list      # must STILL work (user_version unchanged at 1)
```

## Success criteria

- [ ] `internal/store/sqlite/` has no MTClaw table name in any SQL string
- [ ] `rg -n "PRAGMA" internal/store/*.go` is empty
- [ ] `rg -n "AUTOINCREMENT" internal/store/migrations/` is empty
- [ ] `rg -n "LastInsertId|LIMIT -1" internal/store/` is empty
- [ ] `internal/store/migrations/001_init.sql` rendered with the SQLite tokens is DDL-equivalent to `sqlite/testdata/v1_legacy_schema.sql`
- [ ] Legacy-adoption test passes: no DDL re-run, ledger seeded with version 1, pre-existing row intact
- [ ] Newer-ledger refusal test passes for both open modes
- [ ] `PRAGMA user_version` equals `max(applied)` after migrate
- [ ] WAL / `busy_timeout=5000` / `foreign_keys=ON` asserted on writer and reader; writer `MaxOpenConnections == 1`
- [ ] All 17 tests from the old `store_test.go` still pass, unmodified in intent, as `package sqlite_test`
- [ ] Phase 1's gateway and CLI e2e suites pass unchanged
- [ ] `go test -race ./...` green on Windows; `CGO_ENABLED=0 go build ./...` green; `go.mod`/`go.sum` unchanged
- [ ] Manual upgrade rehearsal above completed, including the v1-binary-still-opens step

## Risks and rollback

| Risk | Mitigation |
|---|---|
| Silent SQL regression in a ~1500-line move (plan R1) | Tests move wholesale rather than being rewritten; phase 1's e2e suite runs the real thing; the three intentional edits (LIMIT, RETURNING, struct fields) are enumerated so anything else in the diff is a review flag. |
| Adoption re-runs `001_init.sql` on a legacy DB (plan R2) | Adoption requires ledger-empty **and** `LegacyVersion > 0`, in one transaction, and executes no DDL. Frozen fixture test. |
| Two processes racing the bootstrap/adoption transaction | Bootstrap+adoption is a single `BEGIN IMMEDIATE` transaction (the writer DSN already forces it, `open.go:113-114`) with `busy_timeout=5000`; `CREATE TABLE IF NOT EXISTS` and `INSERT` of a fixed PK are idempotent under retry. |
| Read-only open attempts DDL on a legacy DB (plan R4) | Read path is assert-only: missing ledger → `LegacyVersion` fallback; a dedicated test opens the frozen fixture read-only. |
| `RETURNING` unsupported by `modernc.org/sqlite` for these tables | Already used in production at `sqlite/sessions.go:29-30`. If a surprise appears on the two identity tables, the fallback is a dialect method `InsertReturnsID() bool` + `LastInsertId` branch — one method, added only if forced. |
| Import cycle when tests need both packages (plan A7) | `package sqlite_test`, stated in step 9 so it is not "fixed" the wrong way. |

**Rollback:** one commit, revertible. No user data is rewritten — adoption only
*adds* a `schema_migrations` table. A reverted binary reads `PRAGMA
user_version`, still 1, and ignores the extra table. Verified by the manual
rehearsal's step 3.
