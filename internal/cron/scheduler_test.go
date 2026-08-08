package cron

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adhocore/gronx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"

	// Blank import: registers "sqlite" with store.Open's driver registry.
	// Nothing else in this package's own dependency graph imports the
	// sqlite backend package, so this test file is the one place that
	// has to.
	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// --- test helpers -----------------------------------------------------------

// newTestStore opens a fresh sqlite-backed store.Store, matching the
// convention used by internal/gateway's own tests, so Ensure/Delete/Append
// behave exactly as they do in production.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cron-test.db")
	st, err := store.Open(ctx, config.StorageConfig{Driver: "sqlite", DSN: dbPath}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeEnqueue records every synthetic Inbound the scheduler hands it. When
// autoComplete is set, it invokes OnDone(nil) synchronously (simulating a
// turn that finishes instantly) so a test can exercise the minute-dedup
// guard without the overlap guard also being in play; otherwise a test
// drives completion itself via finish, to simulate an in-flight turn.
type fakeEnqueue struct {
	mu           sync.Mutex
	received     []channel.Inbound
	autoComplete bool
}

func (f *fakeEnqueue) enqueue(in channel.Inbound) {
	f.mu.Lock()
	f.received = append(f.received, in)
	auto := f.autoComplete
	f.mu.Unlock()
	if auto && in.OnDone != nil {
		in.OnDone(nil)
	}
}

func (f *fakeEnqueue) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func (f *fakeEnqueue) at(i int) channel.Inbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received[i]
}

func (f *fakeEnqueue) finish(i int, err error) {
	in := f.at(i)
	if in.OnDone != nil {
		in.OnDone(err)
	}
}

func testJob(name, schedule string) Job {
	return Job{Name: name, Schedule: schedule, Prompt: "do it", Enabled: true, Timeout: time.Minute}
}

// --- gronx contract ----------------------------------------------------------

// TestGronxContract_IsDueIsInstanceMethodReturningError pins down the exact
// API shape the phase plan got wrong once already: IsDue is a method on a
// *gronx.Gronx instance (not a package function) and returns (bool, error);
// a malformed expression must surface as a non-nil error, never a silently
// false "not due".
func TestGronxContract_IsDueIsInstanceMethodReturningError(t *testing.T) {
	g := gronx.New()

	due, err := g.IsDue("not a cron expression at all !!!", time.Now())
	require.Error(t, err, "a malformed expression must error, not silently report false")
	assert.False(t, due)

	// A 5-field expression is implicitly second-0-only once gronx.Segments
	// prepends a leading "0" seconds field, so the reference time must
	// itself have Second() == 0 to be due - time.Now() would flake on
	// whichever second the test happened to run on.
	onTheMinute := time.Date(2024, 1, 1, 8, 0, 0, 0, time.UTC)
	due, err = g.IsDue("* * * * *", onTheMinute)
	require.NoError(t, err)
	assert.True(t, due)

	assert.True(t, gronx.IsValid("* * * * *"), "IsValid is package-level")
	assert.False(t, gronx.IsValid("not a cron expression at all !!!"))
}

// --- alignment -----------------------------------------------------------

// TestAlignDelay_ComputesDelayToNextMinuteBoundary tests the computation
// directly rather than sleeping a real minute: for every now, now+delay
// must land exactly on the next whole minute, with 0 < delay <= 1 minute.
func TestAlignDelay_ComputesDelayToNextMinuteBoundary(t *testing.T) {
	cases := []time.Time{
		time.Date(2024, 1, 1, 8, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 1, 8, 0, 0, 1, time.UTC),
		time.Date(2024, 1, 1, 8, 0, 30, 0, time.UTC),
		time.Date(2024, 1, 1, 8, 0, 59, 999_999_999, time.UTC),
	}
	for _, now := range cases {
		delay := alignDelay(now)
		require.Greater(t, delay, time.Duration(0))
		require.LessOrEqual(t, delay, time.Minute)
		next := now.Add(delay)
		assert.True(t, next.Truncate(time.Minute).Equal(next), "now+delay must land exactly on a minute boundary, got %v", next)
	}
}

// --- missed runs -----------------------------------------------------------

// TestScheduler_MissedRun_NotReplayed proves the scheduler has no
// catch-up/backlog logic: skipping straight from t0 to t0+10m (as a process
// restart after a long outage would) fires at most once, for the current
// minute only - the nine minutes in between are never evaluated or
// replayed.
func TestScheduler_MissedRun_NotReplayed(t *testing.T) {
	st := newTestStore(t)
	fe := &fakeEnqueue{autoComplete: true}
	jobs := []Job{testJob("every-minute", "* * * * *")}
	s := New(jobs, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	t0 := time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC)
	s.tick(t0)
	require.Equal(t, 1, fe.count())

	// Simulate the gateway being down for the next 9 minutes: the very next
	// tick this in-process scheduler ever sees is t0+10m, not t0+1m..t0+9m.
	s.tick(t0.Add(10 * time.Minute))
	assert.Equal(t, 2, fe.count(), "exactly one more fire for the new tick, no backlog replay")
}

// --- overlap guard -----------------------------------------------------------

// TestScheduler_OverlapGuard_RecordsSkippedNotDoubleRun drives two due
// ticks a minute apart while the first fire's turn is still "in flight"
// (OnDone not yet called): the second tick must not enqueue a second turn,
// and must record exactly one skipped cron_runs row.
func TestScheduler_OverlapGuard_RecordsSkippedNotDoubleRun(t *testing.T) {
	st := newTestStore(t)
	fe := &fakeEnqueue{} // autoComplete false: fires never finish on their own
	jobs := []Job{testJob("slow-job", "* * * * *")}
	s := New(jobs, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	t0 := time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC)
	s.tick(t0)
	require.Equal(t, 1, fe.count())

	// A due minute later, the first turn is still running.
	s.tick(t0.Add(time.Minute))
	assert.Equal(t, 1, fe.count(), "must not enqueue a second turn while the first is still in flight")

	runs, err := st.CronRuns().List(context.Background(), "slow-job", 0)
	require.NoError(t, err)
	require.Len(t, runs, 2, "one started row, one skipped row")

	var sawStarted, sawSkipped bool
	for _, r := range runs {
		switch r.Status {
		case "started":
			sawStarted = true
		case "skipped":
			sawSkipped = true
		}
	}
	assert.True(t, sawStarted)
	assert.True(t, sawSkipped)

	// Finishing the first fire clears the running flag; a later due tick
	// fires again.
	fe.finish(0, nil)
	s.tick(t0.Add(2 * time.Minute))
	assert.Equal(t, 2, fe.count())
}

// --- minute de-dup -----------------------------------------------------------

// TestScheduler_MinuteDedup_TwoTicksSameMinute_ExactlyOneRun drives two
// ticks that land on the exact same due minute with a fast (auto-completing)
// turn: the overlap guard alone would not catch this, since the first turn
// already finished by the time the second tick arrives. Only the minute
// guard does.
func TestScheduler_MinuteDedup_TwoTicksSameMinute_ExactlyOneRun(t *testing.T) {
	st := newTestStore(t)
	fe := &fakeEnqueue{autoComplete: true}
	jobs := []Job{testJob("fast-job", "* * * * *")}
	s := New(jobs, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	now := time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC)
	s.tick(now)
	s.tick(now) // same instant again - ticker jitter/misalignment scenario
	assert.Equal(t, 1, fe.count(), "the minute guard must suppress the second tick inside the same due minute")
}

// --- IsDue error handling ----------------------------------------------------

// TestScheduler_IsDueError_LogsRecordsErrorDoesNotFire covers a schedule
// that fails to evaluate (a config bug validation should have already
// caught): the job must not fire, and the attempt is recorded as an error
// run.
func TestScheduler_IsDueError_LogsRecordsErrorDoesNotFire(t *testing.T) {
	st := newTestStore(t)
	fe := &fakeEnqueue{autoComplete: true}
	jobs := []Job{testJob("broken", "not a valid cron expression !!!")}
	s := New(jobs, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	s.tick(time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC))
	assert.Equal(t, 0, fe.count(), "an evaluation error must never fire the job")

	runs, err := st.CronRuns().List(context.Background(), "broken", 0)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "error", runs[0].Status)
	assert.NotEmpty(t, runs[0].Error)
}

// --- DST ---------------------------------------------------------------------

// TestScheduler_DST_SpringForward_SkippedHourDoesNotFire drives ticks
// minute-by-minute across a spring-forward transition (2024-03-10 in
// America/New_York, where 02:00 jumps straight to 03:00): a job scheduled
// for the skipped local time must simply never fire that day. A fixed,
// explicitly-loaded *time.Location is used throughout - never the host
// zone.
func TestScheduler_DST_SpringForward_SkippedHourDoesNotFire(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	st := newTestStore(t)
	fe := &fakeEnqueue{autoComplete: true}
	jobs := []Job{testJob("early-morning", "30 2 * * *")}
	s := New(jobs, loc, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	start := time.Date(2024, 3, 10, 1, 58, 0, 0, loc)
	end := time.Date(2024, 3, 10, 3, 5, 0, 0, loc)
	for tm := start; tm.Before(end); tm = tm.Add(time.Minute) {
		s.tick(tm)
	}

	assert.Equal(t, 0, fe.count(), "02:30 never occurs as a wall-clock reading on the spring-forward day")
}

// TestScheduler_DST_FallBack_RepeatedHourFiresOnlyOnce drives ticks
// minute-by-minute across a fall-back transition (2024-11-03 in
// America/New_York, where 01:00-01:59 occurs twice): a job scheduled for a
// time inside the repeated hour must fire only once, even though the exact
// same local "HH:MM" reading genuinely occurs twice, an hour apart in
// absolute time.
func TestScheduler_DST_FallBack_RepeatedHourFiresOnlyOnce(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	st := newTestStore(t)
	fe := &fakeEnqueue{autoComplete: true}
	jobs := []Job{testJob("ambiguous-hour", "30 1 * * *")}
	s := New(jobs, loc, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	start := time.Date(2024, 11, 3, 0, 58, 0, 0, loc)
	end := time.Date(2024, 11, 3, 2, 5, 0, 0, loc)
	for tm := start; tm.Before(end); tm = tm.Add(time.Minute) {
		s.tick(tm)
	}

	assert.Equal(t, 1, fe.count(), "01:30 occurs twice in absolute time on the fall-back day, but must fire only once")
}

// --- session resolution ------------------------------------------------------

// TestScheduler_PersistentJob_SharesSessionAcrossRuns proves a persistent
// job's runs share one session id, so history accumulates across days.
func TestScheduler_PersistentJob_SharesSessionAcrossRuns(t *testing.T) {
	st := newTestStore(t)
	fe := &fakeEnqueue{autoComplete: true}
	job := testJob("briefing", "* * * * *")
	s := New([]Job{job}, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	t0 := time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC)
	s.tick(t0)
	require.Equal(t, 1, fe.count())
	firstSession := fe.at(0).ChatID + "/" + fe.at(0).ThreadID

	s.tick(t0.Add(time.Minute))
	require.Equal(t, 2, fe.count())
	secondSession := fe.at(1).ChatID + "/" + fe.at(1).ThreadID

	assert.Equal(t, firstSession, secondSession, "a persistent job's runs must reuse the same chat/thread identity")
	assert.Empty(t, fe.at(0).ThreadID, "persistent jobs use no per-run thread id")

	sess, err := st.Sessions().Ensure(context.Background(), "cron", job.chatID(), "")
	require.NoError(t, err)
	require.NoError(t, st.Messages().Append(context.Background(), sess.ID, []store.Message{{Role: "user", Content: "hi"}}))
	n, err := st.Messages().CountBySession(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestScheduler_EphemeralJob_FreshSessionPerRun_DeletedAfter proves an
// ephemeral job gets a brand-new session identity every run, and that
// session is gone once the run's OnDone fires.
func TestScheduler_EphemeralJob_FreshSessionPerRun_DeletedAfter(t *testing.T) {
	st := newTestStore(t)
	fe := &fakeEnqueue{} // manual completion, so the session still exists to inspect first
	job := Job{Name: "scratch", Schedule: "* * * * *", Prompt: "hi", Enabled: true, Ephemeral: true, Timeout: time.Minute}
	s := New([]Job{job}, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	t0 := time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC)
	s.tick(t0)
	require.Equal(t, 1, fe.count())
	in0 := fe.at(0)
	require.NotEmpty(t, in0.ThreadID, "ephemeral jobs use a unique per-run thread id")

	// Resolve the session id once, before finish deletes it - re-resolving
	// via Ensure afterward would silently recreate the (now-deleted) row
	// under the same identity, masking the very thing this test checks.
	sessionID := sessionIDFor(t, st, in0)
	sess1, err := st.Sessions().Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess1)

	fe.finish(0, nil)
	_, err = st.Sessions().Get(context.Background(), sessionID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the ephemeral session must be deleted once the run finishes")

	s.tick(t0.Add(time.Minute))
	require.Equal(t, 2, fe.count())
	in1 := fe.at(1)
	assert.NotEqual(t, in0.ThreadID, in1.ThreadID, "each ephemeral run gets a distinct session identity")
}

// sessionIDFor re-resolves the session id the scheduler used for in, via
// the same (channel, chat, thread) identity Ensure is idempotent over -
// the fake enqueuer only sees the Inbound, not the resolved store.Session.
func sessionIDFor(t *testing.T, st store.Store, in channel.Inbound) string {
	t.Helper()
	sess, err := st.Sessions().Ensure(context.Background(), in.Channel, in.ChatID, in.ThreadID)
	require.NoError(t, err)
	return sess.ID
}

// --- OnDone bookkeeping ------------------------------------------------------

// TestScheduler_OnDone_FinishesRunRow proves OnDone moves the started row to
// a terminal ok/error status with a finished timestamp.
func TestScheduler_OnDone_FinishesRunRow(t *testing.T) {
	st := newTestStore(t)
	fe := &fakeEnqueue{}
	jobs := []Job{testJob("job", "* * * * *")}
	s := New(jobs, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), nil)

	s.tick(time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC))
	require.Equal(t, 1, fe.count())

	fe.finish(0, errors.New("boom"))

	runs, err := st.CronRuns().List(context.Background(), "job", 0)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "error", runs[0].Status)
	assert.Equal(t, "boom", runs[0].Error)
	assert.NotNil(t, runs[0].FinishedAt)
}

// --- Start ---------------------------------------------------------------

// TestScheduler_Start_AlignsAndFiresThenStopsOnCtxDone drives Start with a
// stepping fake clock: the first two calls (LogNextDue, then the alignDelay
// computation) read a moment just under a minute boundary, so the real
// alignment sleep is short (tens of milliseconds) instead of up to 59s; the
// call once the alignment timer actually fires (used to evaluate due-ness)
// reads the boundary itself - mirroring how real time.Now() naturally
// advances between those two calls in production. A gronx 5-field
// expression is implicitly second-0-only (see the gronx contract test), so
// this distinction is not just cosmetic: using the pre-boundary reading for
// both would never be due.
func TestScheduler_Start_AlignsAndFiresThenStopsOnCtxDone(t *testing.T) {
	justBefore := time.Date(2024, 6, 1, 8, 0, 59, 950_000_000, time.UTC) // 50ms before the boundary
	boundary := justBefore.Truncate(time.Minute).Add(time.Minute)        // exactly on it, second == 0

	st := newTestStore(t)
	fe := &fakeEnqueue{autoComplete: true}
	jobs := []Job{testJob("aligned", "* * * * *")}

	var calls int32
	clock := func() time.Time {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return justBefore
		}
		return boundary
	}
	s := New(jobs, time.UTC, st.CronRuns(), st.Sessions(), fe.enqueue, discardLogger(), clock)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s.Start(ctx)

	assert.GreaterOrEqual(t, fe.count(), 1, "the aligned first tick must have fired the due job")
}
