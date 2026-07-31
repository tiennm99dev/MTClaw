---
phase: 2
title: "SQLite Store"
status: pending
priority: P1
dependencies: [1]
effort: ""
---

# Phase 2: SQLite Store

## Overview

Persistence layer: schema, embedded migrations, and the store interfaces the
agent loop, policy engine, and cron scheduler write through. Pure Go SQLite so the
binary stays static.

## Requirements

**Functional**
- Sessions keyed by `(channel, chat_id, thread_id)` so a Telegram chat maps to one
  durable conversation across restarts.
- Ordered message history with tool-call payloads preserved losslessly.
- Approval records and an exec audit trail that survive process death.
- Cron run history.
- Forward-only migrations applied automatically at open.

**Non-functional**
- CGO-free (`modernc.org/sqlite`).
- Concurrent CLI readers while the gateway writes.
- Store interfaces defined in `internal/store`, SQLite implementation in
  `internal/store/sqlite`, so tests can fake the store without a DB.

## Architecture

The gateway process owns all writes **during normal operation**. CLI commands
(`sessions list/show`, `approvals list`) open the file read-only, and WAL makes
concurrent reads safe.

`sessions rm` is the one exception — a second writer, by design, so a user can drop a
poisoned session without stopping the gateway. WAL permits one writer at a time, so
this is correct rather than merely tolerated, but it means the invariant is "one
*sustained* writer", not "one writer". Do not build anything on the stronger claim:
`busy_timeout` is what makes `rm` wait for an in-flight turn instead of failing.

Pragmas applied on every connection, in this order:

```
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA synchronous = NORMAL;
```

`foreign_keys` is per-connection in SQLite and off by default — it must be set on
the connection, not once at open, or the `ON DELETE CASCADE` below silently does
nothing. Set `db.SetMaxOpenConns(1)` for the writer handle to sidestep
`SQLITE_BUSY` entirely under the gateway's own concurrency; readers get their own
handle.

Migrations are `.sql` files in `internal/store/sqlite/migrations/`, embedded with
`//go:embed`, named `001_init.sql`, `002_….sql`. Each runs in a transaction, then
`PRAGMA user_version` is bumped to the file's number. No `golang-migrate`
dependency — the whole runner is ~40 lines and forward-only is all we need.

### Schema

```sql
-- 001_init.sql
CREATE TABLE sessions (
  id                TEXT PRIMARY KEY,          -- ulid-ish, sortable
  channel           TEXT NOT NULL,             -- 'telegram' | 'cli' | 'cron'
  chat_id           TEXT NOT NULL,
  thread_id         TEXT NOT NULL DEFAULT '',  -- forum topic; '' when absent
  title             TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  summary           TEXT NOT NULL DEFAULT '',  -- compaction target
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,          -- unix millis
  updated_at        INTEGER NOT NULL,
  UNIQUE (channel, chat_id, thread_id)
);

CREATE TABLE messages (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq          INTEGER NOT NULL,               -- per-session monotonic
  role         TEXT NOT NULL,                  -- system|user|assistant|tool
  content      TEXT NOT NULL DEFAULT '',
  tool_calls   TEXT NOT NULL DEFAULT '',       -- JSON array, assistant rows only
  tool_call_id TEXT NOT NULL DEFAULT '',       -- tool rows only
  tool_name    TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  UNIQUE (session_id, seq)
);
CREATE INDEX idx_messages_session_seq ON messages(session_id, seq);

CREATE TABLE approvals (
  id          TEXT PRIMARY KEY,                -- nonce, also the callback_data payload
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  channel     TEXT NOT NULL,
  chat_id     TEXT NOT NULL,
  tool        TEXT NOT NULL,
  command     TEXT NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',        -- classifier rationale in auto mode
  state       TEXT NOT NULL,                   -- pending|approved|denied|expired
  message_id  TEXT NOT NULL DEFAULT '',        -- telegram msg holding the buttons
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  decided_at  INTEGER,
  decided_by  TEXT NOT NULL DEFAULT ''         -- telegram user id
);
CREATE INDEX idx_approvals_state ON approvals(state, expires_at);

CREATE TABLE exec_audit (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL,                   -- no FK: audit outlives sessions
  command     TEXT NOT NULL,
  cwd         TEXT NOT NULL DEFAULT '',
  decision    TEXT NOT NULL,                   -- denied_rule|allowed_rule|approved|denied_user|expired|auto_allowed
  rule        TEXT NOT NULL DEFAULT '',        -- the matching regex, when any
  exit_code   INTEGER,
  duration_ms INTEGER,
  truncated   INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE INDEX idx_exec_audit_created ON exec_audit(created_at);

CREATE TABLE cron_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_name    TEXT NOT NULL,
  session_id  TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL,                   -- ok|error|skipped
  error       TEXT NOT NULL DEFAULT '',
  started_at  INTEGER NOT NULL,
  finished_at INTEGER
);
CREATE INDEX idx_cron_runs_job ON cron_runs(job_name, started_at);
```

`exec_audit.session_id` intentionally has no foreign key: deleting a session must
not erase the record that a command ran.

### Interfaces

```go
package store

type Store interface {
    Sessions() SessionStore
    Messages() MessageStore
    Approvals() ApprovalStore
    Audit() AuditStore
    CronRuns() CronRunStore
    Close() error
}

type SessionStore interface {
    // Upsert-by-identity: returns the existing session or creates it.
    Ensure(ctx context.Context, channel, chatID, threadID string) (*Session, error)
    Get(ctx context.Context, id string) (*Session, error)
    List(ctx context.Context, limit int) ([]*Session, error)
    Delete(ctx context.Context, id string) error
    SetSummary(ctx context.Context, id, summary string) error
    AddUsage(ctx context.Context, id string, prompt, completion int) error
}

type MessageStore interface {
    // Append assigns seq atomically; the whole slice lands in one transaction.
    Append(ctx context.Context, sessionID string, msgs []Message) error
    // Recent returns the last n messages in ascending seq order.
    Recent(ctx context.Context, sessionID string, n int) ([]Message, error)
    DeleteBySession(ctx context.Context, sessionID string) error
}
```

`Append` taking a slice and committing once is the important detail: a turn's
assistant message plus its tool results must become visible together, or a crash
mid-turn leaves an assistant `tool_calls` row with no matching `tool` rows —
which the OpenAI API rejects on the next request.

## Related Code Files

- Create: `internal/store/store.go` — interfaces
- Create: `internal/store/types.go` — `Session`, `Message`, `Approval`, `ExecAudit`, `CronRun`
- Create: `internal/store/sqlite/open.go` — DSN, pragmas, migration runner
- Create: `internal/store/sqlite/migrations/001_init.sql`
- Create: `internal/store/sqlite/sessions.go`, `messages.go`, `approvals.go`, `audit.go`, `cron_runs.go`
- Create: `internal/store/sqlite/store_test.go`, `migrate_test.go`
- Create: `internal/cli/sessions_cmd.go` — `sessions list|show|rm`
- Modify: `internal/cli/root.go` — open the store in `PersistentPreRunE` for commands that need it

## Implementation Steps

1. Add `modernc.org/sqlite`. Register under driver name `sqlite`.
2. `open.go`: build DSN `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)`;
   also issue the pragmas via `ExecContext` after open so behaviour does not depend
   on driver DSN parsing. Create parent dirs. Expose `Open(path string, readOnly bool)`.
3. Migration runner: read embedded dir, sort by numeric prefix, compare each to
   `PRAGMA user_version`, apply pending ones in a tx, bump the version inside the
   same tx. Refuse to open when `user_version` exceeds the highest embedded
   migration — that means an older binary hit a newer DB, and continuing would
   corrupt data.
4. `types.go`: timestamps as `time.Time` in Go, unix millis in SQLite; conversion
   helpers in one place.
5. `sessions.go`: `Ensure` uses `INSERT … ON CONFLICT(channel, chat_id, thread_id) DO UPDATE SET updated_at=? RETURNING *`.
6. `messages.go`: `Append` opens a tx, reads `COALESCE(MAX(seq),0)`, inserts with
   incrementing seq, commits. `Recent` selects with `ORDER BY seq DESC LIMIT n` then
   reverses in Go.
7. `approvals.go`: `Create`, `Get`, `Decide(id, state, by)` guarded by
   `WHERE state='pending'` so a double-tap on the inline button cannot flip a
   decision, `ExpirePending(now)` returning the count expired.
8. `audit.go` / `cron_runs.go`: append-only writers plus simple list queries.
9. `internal/cli/sessions_cmd.go`: `list` prints a table (id, channel, chat, title,
   messages, tokens, updated); `show <id>` prints the transcript; `rm <id>` deletes
   and relies on cascade. All open the store read-only except `rm`.
10. Wire store open/close into the root command lifecycle.

## Tests / Validation

- Migration runner: fresh DB reaches the latest version; re-open is a no-op;
  a DB with a higher `user_version` is refused.
- `Ensure` is idempotent and returns a stable id for the same identity triple.
- `Append` assigns gapless seq under two concurrent goroutines on the same session.
- A partially-failing `Append` (forced error on the last row) leaves zero rows.
- `Delete` on a session cascades to messages and approvals but leaves `exec_audit`.
- `Decide` twice returns "already decided" on the second call.
- Concurrent read while writing succeeds under WAL (open a reader handle mid-write).

## Success Criteria

- [ ] `Open` creates, migrates, and opens a DB at a fresh path
- [ ] Re-opening an up-to-date DB applies no migrations
- [ ] Opening a DB from a newer schema version fails loudly
- [ ] `foreign_keys` is verifiably ON on the live connection (asserted in a test, not assumed)
- [ ] A tool-call turn is atomically visible: never an assistant `tool_calls` row without its `tool` rows
- [ ] `mtclaw sessions list` works while the gateway is running and writing
- [ ] `mtclaw sessions rm` cascades messages, preserves audit rows
- [ ] Binary still builds with `CGO_ENABLED=0`

## Risk Assessment

- **`foreign_keys` silently off** is the classic SQLite footgun and would make every
  cascade a no-op. Explicitly asserted in a test.
- **Atomic turn persistence** is the correctness crux of this phase. If it is wrong,
  the failure appears much later as opaque 400s from OpenAI about orphaned
  `tool_call_id`s. Test it directly here.
- **`modernc.org/sqlite` is a translation of the C source** — much slower than CGO
  SQLite on bulk writes. Irrelevant at personal-assistant volume (tens of writes
  per turn); revisit only if cron fan-out grows.
- **Unbounded growth.** No retention policy in v1. `exec_audit` and `messages` grow
  forever. Acceptable at this scale; note it in `docs/configuration.md` and leave
  pruning to a later plan.
