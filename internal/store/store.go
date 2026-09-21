// Package store defines the persistence interfaces every other MTClaw
// package (agent loop, policy engine, cron scheduler, CLI) depends on. The
// SQLite implementation lives in internal/store/sqlite; keeping the
// interfaces here lets tests fake the store without a real database.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a lookup by id finds no row.
var ErrNotFound = errors.New("store: not found")

// ErrAlreadyDecided is returned by ApprovalStore.Decide when the approval
// is no longer pending (already approved, denied, or expired), so a
// double-tap on an inline button (or a race with expiry) cannot flip a
// decision that was already made.
var ErrAlreadyDecided = errors.New("store: approval already decided")

// Store aggregates every table-scoped store behind one handle.
type Store interface {
	Sessions() SessionStore
	Messages() MessageStore
	Approvals() ApprovalStore
	Audit() AuditStore
	CronRuns() CronRunStore
	Close() error
}

// SessionStore manages Session rows.
type SessionStore interface {
	// Ensure is upsert-by-identity: it returns the existing session for
	// (channel, chatID, threadID) or creates one, and is safe to call on
	// every inbound message.
	Ensure(ctx context.Context, channel, chatID, threadID string) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	// List returns the most recently updated sessions first. limit <= 0
	// means no limit.
	List(ctx context.Context, limit int) ([]*Session, error)
	// Delete removes the session and, via ON DELETE CASCADE, its messages
	// and approvals. exec_audit rows referencing it are preserved.
	Delete(ctx context.Context, id string) error
	SetSummary(ctx context.Context, id, summary string) error
	AddUsage(ctx context.Context, id string, prompt, completion int) error
}

// MessageStore manages Message rows.
type MessageStore interface {
	// Append assigns seq atomically and commits the whole slice in one
	// transaction: a turn's assistant message and its tool results become
	// visible together, or not at all, so a crash mid-turn never leaves an
	// assistant tool_calls row without its matching tool rows.
	Append(ctx context.Context, sessionID string, msgs []Message) error
	// Recent returns the last n messages in ascending seq order. n <= 0
	// means no limit.
	Recent(ctx context.Context, sessionID string, n int) ([]Message, error)
	// CountBySession returns the total message count for a session, used
	// by `sessions list` to show a per-session message count without
	// loading the whole history.
	CountBySession(ctx context.Context, sessionID string) (int, error)
	DeleteBySession(ctx context.Context, sessionID string) error
}

// ApprovalStore manages Approval rows.
type ApprovalStore interface {
	// Create inserts a, assigning ID and CreatedAt when they are zero.
	// ExpiresAt must be set: a zero value would be stored as the epoch and
	// swept as already expired on the next ExpirePending, silently denying
	// the command, so Create rejects it.
	Create(ctx context.Context, a *Approval) error
	Get(ctx context.Context, id string) (*Approval, error)
	// SetMessageID records the channel message id holding a pending
	// approval's inline buttons, once that message actually gets sent. The
	// row is created before the prompt is sent (so a fast tap can never
	// race an unwritten row), which means MessageID is not known yet at
	// Create time; this fills it in afterward.
	SetMessageID(ctx context.Context, id, messageID string) error
	// Decide moves a pending approval to state (approved|denied) atomically
	// guarded by WHERE state='pending'. It returns ErrAlreadyDecided if the
	// approval was already decided or expired, and ErrNotFound if id does
	// not exist.
	Decide(ctx context.Context, id, state, by string) error
	// ExpirePending moves every pending approval whose expiry is before now
	// to state=expired, returning the number expired.
	ExpirePending(ctx context.Context, now time.Time) (int, error)
}

// AuditStore is an append-only writer for the exec audit trail.
type AuditStore interface {
	// Append inserts a, assigning ID and CreatedAt when they are zero.
	Append(ctx context.Context, a *ExecAudit) error
	// List returns the most recent audit rows first. limit <= 0 means no
	// limit.
	List(ctx context.Context, limit int) ([]*ExecAudit, error)
}

// CronRunStore records cron run history. A row's status is one of ok,
// error, skipped, or interrupted (see ExpireStarted for the last).
// migrations/001_init.sql's own column comment predates "interrupted" and
// still lists only ok|error|skipped; it needs the same update.
type CronRunStore interface {
	// Append inserts r, assigning ID and StartedAt when they are zero.
	Append(ctx context.Context, r *CronRun) error
	// Finish sets the row's terminal status (ok or error), stamping
	// finishedAt. It matches on id alone and does not check the current
	// status. Used by the scheduler's OnDone callback, once the enqueued
	// turn actually completes, to close out the row Append created when
	// the job fired. Returns ErrNotFound if id does not exist; callers
	// treat that as a log-only failure, since losing this update must
	// never crash a running job.
	Finish(ctx context.Context, id int64, status, errMsg string, finishedAt time.Time) error
	// List returns the most recent runs first, optionally filtered to one
	// job name (empty means all jobs). limit <= 0 means no limit.
	List(ctx context.Context, jobName string, limit int) ([]*CronRun, error)
	// ExpireStarted moves every row still "started" as of before (a fire
	// whose OnDone never ran: a SIGKILL, an OOM, a dropped dispatch) to
	// "interrupted", stamping finishedAt, and returns the count moved.
	// Called once at gateway startup, mirroring
	// ApprovalStore.ExpirePending, so a `mtclaw cron runs` report never
	// shows a job as perpetually in flight after a hard process exit.
	ExpireStarted(ctx context.Context, before time.Time) (int, error)
}
