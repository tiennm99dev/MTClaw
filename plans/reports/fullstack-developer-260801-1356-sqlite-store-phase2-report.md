# Phase 2 (SQLite Store) Implementation Report

## Executed Phase
- Phase: phase-02-sqlite-store
- Plan: C:\Users\miti99\Workspaces\tiennm99\MTClaw\plans\260731-2219-mtclaw-core-system
- Status: completed

## Files Created
- `internal/store/types.go` (79) - Session, Message, Approval, ExecAudit, CronRun
- `internal/store/store.go` (85) - Store/SessionStore/MessageStore/ApprovalStore/AuditStore/CronRunStore interfaces, ErrNotFound, ErrAlreadyDecided
- `internal/store/sqlite/migrations/001_init.sql` (66) - verbatim schema from the phase spec
- `internal/store/sqlite/open.go` (230) - DSN builder, pragma application, migration runner, `Open`, aggregate `Store`/`New`
- `internal/store/sqlite/convert.go` (89) - unix-millis/time.Time and nullable conversion helpers (single place, per spec)
- `internal/store/sqlite/id.go` (30) - session id / approval nonce generation (crypto/rand)
- `internal/store/sqlite/sessions.go` (144) - SessionStore impl
- `internal/store/sqlite/messages.go` (100) - MessageStore impl (atomic Append)
- `internal/store/sqlite/approvals.go` (110) - ApprovalStore impl
- `internal/store/sqlite/audit.go` (65) - AuditStore impl
- `internal/store/sqlite/cron_runs.go` (65) - CronRunStore impl
- `internal/store/sqlite/migrate_test.go` (100) - migration/version/foreign_keys tests
- `internal/store/sqlite/store_test.go` (300) - full CRUD, atomicity, concurrency, cascade, decide-twice tests
- `internal/cli/sessions_cmd.go` (127) - `sessions list|show|rm`

## Files Modified
- `internal/cli/root.go` - added `state.store`, `openStore`/`closeStore` helpers, wired `Execute()` to close the store after the whole command tree runs (not via `PersistentPostRunE`, which cobra skips when `RunE` errors), registered `newSessionsCmd`
- `go.mod` / `go.sum` - added `modernc.org/sqlite v1.55.0` and its transitive deps via `go get` + `go mod tidy`

## Tasks Completed
- [x] `modernc.org/sqlite` added, driver name `sqlite` (auto-registered by the module's own `init()`)
- [x] DSN built as `file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)[&mode=ro|&_txlock=immediate]`; pragmas re-issued via `ExecContext` after open regardless of DSN parsing
- [x] Migration runner: embedded `.sql` files, numeric-prefix order, one tx per migration with `PRAGMA user_version` bumped in the same tx; refuses to open (both modes) when `user_version` exceeds the highest embedded migration
- [x] Read-only open falls back to read-write on `SQLITE_READONLY_RECOVERY` (extended code 264, matched via `errors.As` on `*sqlite.Error`)
- [x] Writer handle: `db.SetMaxOpenConns(1)`; reader handles are independent `Open` calls with their own pool
- [x] `_txlock=immediate` on the writer DSN makes every `db.BeginTx` on that handle start `BEGIN IMMEDIATE` automatically - `Append`, and any future read-then-write transaction on the writer handle, gets this for free without hand-written `BEGIN IMMEDIATE` SQL
- [x] `Append(sessionID, []Message)` reads `COALESCE(MAX(seq),0)` and inserts the whole batch in one tx; any failure rolls back the entire batch via deferred `Rollback()` (Commit is the only path that keeps rows)
- [x] `Recent(n)` returns ascending seq (queries DESC, reverses in Go)
- [x] `Decide` guarded by `WHERE state='pending'`; distinguishes `ErrNotFound` vs `ErrAlreadyDecided`; `ExpirePending(now)` returns the expired count
- [x] `exec_audit` has no FK (verified: survives session `Delete`)
- [x] Session `Delete` cascades messages + approvals via `ON DELETE CASCADE` (works because `foreign_keys=ON` is set per-connection)
- [x] Timestamps: `time.Time` in Go, unix millis in SQLite, all conversions centralized in `convert.go`
- [x] `sessions list|show|rm` wired; list/show open read-only, rm opens read-write
- [x] Store open/close wired into root command lifecycle via `state.openStore`/`closeStore`, closed once in `Execute()` after `root.Execute()` returns (covers the error path cobra's `PersistentPostRunE` would miss)

## Deviations from the literal spec (all minimal, justified below)
1. **`MessageStore.CountBySession`** added (not in the phase file's illustrative interface list) - `sessions list`'s required "messages" column has no other way to get a per-session count without the CLI reaching around the `Store` abstraction into raw SQL. Kept minimal: one method, one query.
2. **Session/approval ID generation**: no ULID library is in the phase's dependency list, so `internal/store/sqlite/id.go` implements a dependency-free "ulid-ish, sortable" id (hex millis + random suffix) for sessions, and a 32-hex-digit random nonce for approvals (doubles as Telegram `callback_data`).
3. Root cause of the writer's `BEGIN IMMEDIATE` requirement is satisfied via the DSN's `_txlock=immediate` parameter (confirmed supported by `modernc.org/sqlite` v1.55.0's driver docs) rather than hand-issuing `"BEGIN IMMEDIATE"` SQL per transaction - simpler and applies uniformly to every transaction opened on the writer handle, which is exactly the invariant the spec asks for.

## Empirical verification done before implementing (Windows, modernc.org/sqlite v1.55.0)
Ran a throwaway probe program (not part of the repo) to confirm three load-bearing, undocumented-in-spec behaviors before relying on them:
- `mode=ro` in a `file:` URI DSN works as expected (SQLite's own URI parsing, gated by the driver always passing `SQLITE_OPEN_URI`); write attempts on it fail with `SQLITE_READONLY (8)`; opening a nonexistent path with `mode=ro` fails at `Ping` with `SQLITE_CANTOPEN (14)`, not `SQLITE_READONLY_RECOVERY` - so the not-yet-created-db case correctly does *not* trigger the read→write fallback.
- `PRAGMA journal_mode = WAL` issued as a *setter* against a read-only connection is a silent no-op (no error), so `applyPragmas` runs the same 4 statements unconditionally for both modes.
- A concurrent writer insert + reader select against the same WAL-mode file succeeds without contention.

## Tests Status
- Type check / build: pass (`go build ./...`, `CGO_ENABLED=0 go build ./...`)
- `go vet ./...`: pass
- `gofmt -l .`: clean (fixed one pre-existing misalignment in `internal/store/types.go` picked up incidentally)
- Unit tests: pass, `go test ./... -race` (race unavailable under `CGO_ENABLED=0`, per Go's own `-race` requirement - unrelated to whether the sqlite driver needs cgo; a plain, non-race `CGO_ENABLED=0 go test ./...` run was also green)
  - All phase-1 tests (`internal/config`) still pass unmodified
  - `internal/store/sqlite`: 16 tests, all passing, covering every phase-2 success criterion:
    - fresh DB migrates to latest; re-open no-op; newer `user_version` refused in both read-write and read-only mode
    - `foreign_keys` asserted ON via `PRAGMA foreign_keys` on both writer and reader connections
    - `Append` gapless under 4 concurrent goroutines x 10 messages each (`-race` clean)
    - atomic-turn test: forces a mid-batch abort via a connection-local `CREATE TEMP TRIGGER` (schema itself has no per-row CHECK to hook, and the UNIQUE(session_id,seq) constraint can't be forced stale by design - see note in the test's comment) and asserts zero rows from the failing batch persisted, including the earlier-in-batch rows that would have succeeded alone
    - session `Delete` cascades messages+approvals, preserves `exec_audit`
    - `Decide` twice: second call returns `ErrAlreadyDecided`, first decision unchanged
    - concurrent reader (separate read-only `*DB` handle) alongside an active writer, both hammering 20 iterations, no errors
- Manual CLI smoke test (built binary, seeded a session via a throwaway program, deleted before finishing): `sessions list` on a nonexistent DB fails cleanly (`SQLITE_CANTOPEN`); `list`/`show`/`rm` round-trip correctly; `rm` on an already-removed id returns "not found".

## Issues Encountered
None blocking. `go.mod`'s `go` directive was auto-normalized from `1.25` to `1.25.0` by `go get`/`go mod tidy` - a Go-toolchain formatting normalization, not a version bump; left as-is since reverting it would just be undone by the next `go mod tidy`.

## Next Steps
Phase 3 (OpenAI Provider) and Phase 4 (Agent Loop) can proceed; Phase 4 depends on `internal/store`'s interfaces (stable) and will be the first consumer of `MessageStore.Append`/`Recent` in a real agent turn.

## Unresolved Questions
None blocking. One judgment call worth flagging: `CountBySession` was added to `MessageStore` beyond the phase file's illustrative interface list (see Deviations #1) - if a later phase would rather compute this differently (e.g. a JOIN in `Sessions().List`), that method is trivially removable/replaceable since nothing else depends on it yet.
