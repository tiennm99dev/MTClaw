package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/store"
)

// newTestStore opens a fresh writer *Store at a temp path.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(ctx, path, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

func TestSessions_EnsureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	s1, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)
	require.NotEmpty(t, s1.ID)

	s2, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	assert.Equal(t, s1.ID, s2.ID, "Ensure must return a stable id for the same identity triple")

	// A different identity triple gets a different session.
	s3, err := st.Sessions().Ensure(ctx, "telegram", "chat-2", "")
	require.NoError(t, err)
	assert.NotEqual(t, s1.ID, s3.ID)
}

func TestSessions_GetAndListNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	_, err := st.Sessions().Get(ctx, "does-not-exist")
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = st.Sessions().Delete(ctx, "does-not-exist")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestSessions_List_OrderedByUpdatedAtDesc(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	a, err := st.Sessions().Ensure(ctx, "telegram", "chat-a", "")
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	b, err := st.Sessions().Ensure(ctx, "telegram", "chat-b", "")
	require.NoError(t, err)

	list, err := st.Sessions().List(ctx, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, b.ID, list[0].ID, "most recently updated session first")
	assert.Equal(t, a.ID, list[1].ID)
}

// TestMessages_Append_BumpsSessionUpdatedAt proves a turn with no usage to
// record (mock provider, a cached or zero-usage response) still surfaces as
// recently active in `sessions list`'s ORDER BY updated_at DESC, instead of
// sinking behind sessions whose only activity was AddUsage. The session's
// updated_at is forced into the past first (rather than relying on sleeps
// around millisecond-resolution timestamps) so the assertion is
// deterministic instead of racing the clock on a loaded box.
func TestMessages_Append_BumpsSessionUpdatedAt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	dbHandle := storeDB(t, st)
	sentinel := toMillis(time.Now().Add(-time.Hour))
	_, err = dbHandle.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, sentinel, sess.ID)
	require.NoError(t, err)

	// Append with no usage to report - the exact shape AddUsage's own
	// updated_at bump never covers.
	require.NoError(t, st.Messages().Append(ctx, sess.ID, []store.Message{{Role: "user", Content: "hi"}}))

	got, err := st.Sessions().Get(ctx, sess.ID)
	require.NoError(t, err)
	assert.True(t, got.UpdatedAt.After(fromMillis(sentinel)), "Append must bump updated_at even when it records no usage")
}

func TestMessages_AppendIsAtomicAndGaplessUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "cli", "local", "")
	require.NoError(t, err)

	const goroutines = 4
	const perGoroutine = 10

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				err := st.Messages().Append(ctx, sess.ID, []store.Message{
					{Role: "user", Content: "hi"},
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	msgs, err := st.Messages().Recent(ctx, sess.ID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, goroutines*perGoroutine)

	// Gapless, ascending, starting at 1.
	for i, m := range msgs {
		assert.Equal(t, int64(i+1), m.Seq)
	}
}

func TestMessages_Append_ATurnIsAtomicallyVisible(t *testing.T) {
	// The correctness crux of this phase: an assistant tool_calls row must
	// never be visible without its matching tool rows. Append commits the
	// whole slice in one transaction, so simulate a crash mid-turn by
	// forcing the *last* insert in the batch to fail, and assert that none
	// of the batch - not even the earlier, individually-valid rows in the
	// same call - persisted.
	//
	// The schema (001_init.sql, kept verbatim from the phase spec) has no
	// per-row CHECK that content alone could trip, and the UNIQUE(session_
	// id, seq) constraint can never fire from stale data because Append
	// always recomputes MAX(seq) inside the same transaction it inserts
	// in - by construction, a seq collision within one Append call cannot
	// happen. So this test adds a connection-local, test-only trigger
	// (not touching the production schema) that aborts an insert carrying
	// a sentinel content value, standing in for whatever real error
	// (disk full, cancelled context, ...) could interrupt persisting a
	// turn partway through.
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "cli", "local", "")
	require.NoError(t, err)

	// Two ordinary messages land first, occupying seq 1 and 2.
	require.NoError(t, st.Messages().Append(ctx, sess.ID, []store.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
	}))

	dbHandle := storeDB(t, st)
	_, err = dbHandle.ExecContext(ctx, `
		CREATE TEMP TRIGGER fail_forced_content BEFORE INSERT ON messages
		WHEN NEW.content = 'forced-failure-sentinel'
		BEGIN SELECT RAISE(ABORT, 'forced test failure'); END`)
	require.NoError(t, err)

	turn := []store.Message{
		{Role: "user", Content: "do a thing"},
		{Role: "assistant", Content: "", ToolCalls: `[{"id":"call_1","type":"function"}]`},
		{Role: "tool", Content: "forced-failure-sentinel", ToolCallID: "call_1", ToolName: "exec"},
	}
	err = st.Messages().Append(ctx, sess.ID, turn)
	require.Error(t, err, "the third insert must be aborted by the sentinel trigger")

	msgs, err := st.Messages().Recent(ctx, sess.ID, 0)
	require.NoError(t, err)
	// Only the 2 original messages remain; none of the failed batch's
	// rows (including the first two, which would have succeeded
	// individually) were left behind.
	require.Len(t, msgs, 2)
	for _, m := range msgs {
		assert.NotEqual(t, "do a thing", m.Content)
		assert.NotEqual(t, "assistant", m.Role, "an assistant tool_calls row must never persist without its tool rows")
	}
}

func TestMessages_CountAndDeleteBySession(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "cli", "local", "")
	require.NoError(t, err)
	require.NoError(t, st.Messages().Append(ctx, sess.ID, []store.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}))

	n, err := st.Messages().CountBySession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	require.NoError(t, st.Messages().DeleteBySession(ctx, sess.ID))
	n, err = st.Messages().CountBySession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSessions_DeleteCascadesMessagesAndApprovalsButPreservesAudit(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	require.NoError(t, st.Messages().Append(ctx, sess.ID, []store.Message{{Role: "user", Content: "hi"}}))

	require.NoError(t, st.Approvals().Create(ctx, &store.Approval{
		SessionID: sess.ID,
		Channel:   "telegram",
		ChatID:    "chat-1",
		Tool:      "exec",
		Command:   "ls",
		ExpiresAt: time.Now().Add(time.Minute),
	}))

	require.NoError(t, st.Audit().Append(ctx, &store.ExecAudit{
		SessionID: sess.ID,
		Command:   "ls",
		Decision:  "allowed_rule",
	}))

	require.NoError(t, st.Sessions().Delete(ctx, sess.ID))

	msgs, err := st.Messages().Recent(ctx, sess.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, msgs, "messages must cascade-delete with the session")

	approvals, err := st.Audit().List(ctx, 0)
	require.NoError(t, err)
	require.Len(t, approvals, 1, "exec_audit must survive session deletion")
	assert.Equal(t, sess.ID, approvals[0].SessionID)

	_, err = st.Sessions().Get(ctx, sess.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestApprovals_DecideTwiceReturnsAlreadyDecided(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	ap := &store.Approval{
		SessionID: sess.ID,
		Channel:   "telegram",
		ChatID:    "chat-1",
		Tool:      "exec",
		Command:   "ls",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	require.NoError(t, st.Approvals().Create(ctx, ap))

	require.NoError(t, st.Approvals().Decide(ctx, ap.ID, "approved", "user-1"))

	err = st.Approvals().Decide(ctx, ap.ID, "denied", "user-2")
	assert.ErrorIs(t, err, store.ErrAlreadyDecided)

	got, err := st.Approvals().Get(ctx, ap.ID)
	require.NoError(t, err)
	assert.Equal(t, "approved", got.State, "the second Decide must not have overwritten the first")
	assert.Equal(t, "user-1", got.DecidedBy)
}

func TestApprovals_DecideUnknownIDReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	err := st.Approvals().Decide(ctx, "nonexistent", "approved", "user-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestApprovals_SetMessageID_RecordsAfterCreate proves the M2 ordering fix's
// store side: a row can be created with no message_id yet (the prompt has
// not been sent when Create runs) and SetMessageID fills it in afterward.
func TestApprovals_SetMessageID_RecordsAfterCreate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	ap := &store.Approval{
		SessionID: sess.ID, Channel: "telegram", ChatID: "chat-1", Tool: "exec", Command: "ls",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	require.NoError(t, st.Approvals().Create(ctx, ap))
	assert.Empty(t, ap.MessageID, "no message id is known yet at Create time")

	require.NoError(t, st.Approvals().SetMessageID(ctx, ap.ID, "4242"))

	got, err := st.Approvals().Get(ctx, ap.ID)
	require.NoError(t, err)
	assert.Equal(t, "4242", got.MessageID)
}

// TestApprovals_SetMessageID_UnknownIDIsANoOp proves SetMessageID never
// fails just because the row is gone by the time the send confirms (already
// expired or decided) - it must not turn that race into an Ask error.
func TestApprovals_SetMessageID_UnknownIDIsANoOp(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	err := st.Approvals().SetMessageID(ctx, "nonexistent", "4242")
	assert.NoError(t, err)
}

// TestApprovals_Create_RejectsZeroExpiresAt proves a caller cannot create
// an approval that ExpirePending would silently expire on its very next
// sweep (toMillis maps a zero ExpiresAt to 0, which sorts before "now").
func TestApprovals_Create_RejectsZeroExpiresAt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	err = st.Approvals().Create(ctx, &store.Approval{
		SessionID: sess.ID, Channel: "telegram", ChatID: "chat-1", Tool: "exec", Command: "ls",
	})
	require.Error(t, err)
}

func TestApprovals_ExpirePending(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess, err := st.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	expired := &store.Approval{
		SessionID: sess.ID, Channel: "telegram", ChatID: "chat-1", Tool: "exec", Command: "ls",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, st.Approvals().Create(ctx, expired))

	stillPending := &store.Approval{
		SessionID: sess.ID, Channel: "telegram", ChatID: "chat-1", Tool: "exec", Command: "pwd",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, st.Approvals().Create(ctx, stillPending))

	n, err := st.Approvals().ExpirePending(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	got, err := st.Approvals().Get(ctx, expired.ID)
	require.NoError(t, err)
	assert.Equal(t, "expired", got.State)

	got, err = st.Approvals().Get(ctx, stillPending.ID)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.State)
}

func TestCronRuns_AppendAndList(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	require.NoError(t, st.CronRuns().Append(ctx, &store.CronRun{JobName: "daily", Status: "ok"}))
	require.NoError(t, st.CronRuns().Append(ctx, &store.CronRun{JobName: "other", Status: "error", Error: "boom"}))

	all, err := st.CronRuns().List(ctx, "", 0)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	daily, err := st.CronRuns().List(ctx, "daily", 0)
	require.NoError(t, err)
	require.Len(t, daily, 1)
	assert.Equal(t, "ok", daily[0].Status)
}

func TestCronRuns_Finish(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	run := &store.CronRun{JobName: "briefing", Status: "started"}
	require.NoError(t, st.CronRuns().Append(ctx, run))
	require.NotZero(t, run.ID)

	finishedAt := time.Now().Truncate(time.Millisecond)
	require.NoError(t, st.CronRuns().Finish(ctx, run.ID, "ok", "", finishedAt))

	got, err := st.CronRuns().List(ctx, "briefing", 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ok", got[0].Status)
	assert.Empty(t, got[0].Error)
	require.NotNil(t, got[0].FinishedAt)
	assert.WithinDuration(t, finishedAt, *got[0].FinishedAt, time.Millisecond)
}

func TestCronRuns_Finish_UnknownIDReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	err := st.CronRuns().Finish(ctx, 999999, "ok", "", time.Now())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestCronRuns_ExpireStarted_MovesStartedRowsBeforeCutoff is the M6
// regression test: a row still "started" as of a prior process's crash (no
// Finish ever ran) must be swept to "interrupted" at the next startup, the
// same restart-safety pattern ApprovalStore.ExpirePending already provides.
func TestCronRuns_ExpireStarted_MovesStartedRowsBeforeCutoff(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	stuck := &store.CronRun{JobName: "briefing", Status: "started"}
	require.NoError(t, st.CronRuns().Append(ctx, stuck))

	cutoff := stuck.StartedAt.Add(time.Second)
	n, err := st.CronRuns().ExpireStarted(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	got, err := st.CronRuns().List(ctx, "briefing", 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "interrupted", got[0].Status)
	require.NotNil(t, got[0].FinishedAt)
}

// TestCronRuns_ExpireStarted_LeavesFinishedRowsAlone proves a row already
// in a terminal status (ok/error/skipped) is never touched by the sweep.
func TestCronRuns_ExpireStarted_LeavesFinishedRowsAlone(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	done := &store.CronRun{JobName: "daily", Status: "ok"}
	require.NoError(t, st.CronRuns().Append(ctx, done))
	require.NoError(t, st.CronRuns().Finish(ctx, done.ID, "ok", "", time.Now()))

	n, err := st.CronRuns().ExpireStarted(ctx, time.Now().Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	got, err := st.CronRuns().List(ctx, "daily", 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ok", got[0].Status)
}

// TestCronRuns_ExpireStarted_LeavesRowsStartedAfterCutoff proves the sweep
// never touches a row that started at or after the cutoff - only a run
// genuinely stuck from before this startup is swept.
func TestCronRuns_ExpireStarted_LeavesRowsStartedAfterCutoff(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	before := time.Now()
	fresh := &store.CronRun{JobName: "briefing", Status: "started"}
	require.NoError(t, st.CronRuns().Append(ctx, fresh))

	n, err := st.CronRuns().ExpireStarted(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	got, err := st.CronRuns().List(ctx, "briefing", 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "started", got[0].Status)
}

func TestConcurrentReaderWhileWritingSucceedsUnderWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.db")

	writerDB, err := Open(ctx, path, false)
	require.NoError(t, err)
	defer writerDB.Close()
	writer := New(writerDB)

	sess, err := writer.Sessions().Ensure(ctx, "telegram", "chat-1", "")
	require.NoError(t, err)

	readerDB, err := Open(ctx, path, true)
	require.NoError(t, err)
	defer readerDB.Close()
	assert.True(t, readerDB.ReadOnly)
	reader := New(readerDB)

	var wg sync.WaitGroup
	wg.Add(2)

	writeErrs := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := writer.Messages().Append(ctx, sess.ID, []store.Message{{Role: "user", Content: "hi"}}); err != nil {
				writeErrs <- err
				return
			}
		}
	}()

	readErrs := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := reader.Sessions().List(ctx, 0); err != nil {
				readErrs <- err
				return
			}
		}
	}()

	wg.Wait()
	close(writeErrs)
	close(readErrs)
	for err := range writeErrs {
		require.NoError(t, err)
	}
	for err := range readErrs {
		require.NoError(t, err)
	}
}

// storeDB reaches into a *Store to get its underlying *DB for test-only raw
// SQL setup (planting a colliding row). Kept in the test file rather than
// exported, since production code never needs to bypass the store
// interfaces this way.
func storeDB(t *testing.T, st *Store) *DB {
	t.Helper()
	return st.db
}
