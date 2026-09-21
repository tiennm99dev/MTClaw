package cron

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adhocore/gronx"

	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/store"
)

// wallClockMinuteLayout is the lastFired de-dup key: the job's due minute as
// displayed in the scheduler's configured location, not the minute's
// absolute instant. This distinction matters across a DST fall-back
// transition, where the same local "HH:MM" occurs twice an hour apart in
// absolute time - time.Time.Truncate(time.Minute) operates on the absolute
// instant (per its own docs) and would treat those two occurrences as
// different minutes, letting the job fire twice. Comparing the formatted
// wall-clock string instead makes the second occurrence look identical to
// the first, so the minute guard suppresses it - which is what "fall back
// must not fire twice" requires. A spring-forward gap needs no special
// handling either way: the skipped local time never appears as a `now`
// reading at all, regardless of key scheme.
const wallClockMinuteLayout = "2006-01-02T15:04"

// Enqueuer hands a synthetic channel.Inbound to the gateway dispatcher. The
// scheduler has no idea a dispatcher or gateway exists beyond this func
// value - Gateway wires it to its own dispatcher when cron.enabled.
type Enqueuer func(channel.Inbound)

// Scheduler evaluates every configured job once a minute (aligned to the
// wall-clock boundary) and enqueues a synthetic turn for each job that is
// due, guarding against both re-entrant overlap and duplicate firing inside
// one due minute.
type Scheduler struct {
	jobs     []Job
	loc      *time.Location
	runs     store.CronRunStore
	sessions store.SessionStore
	enqueue  Enqueuer
	log      *slog.Logger
	gron     *gronx.Gronx

	now func() time.Time

	mu        sync.Mutex
	lastFired map[string]string // job name -> wallClockMinuteLayout key

	running map[string]*atomic.Bool // job name -> in-flight flag; fixed set, built once at construction
}

// New builds a Scheduler over jobs, evaluated against loc. enqueue is
// invoked once per job fire with a synthetic channel.Inbound; runs and
// sessions back cron_runs history and session resolution respectively. now
// defaults to time.Now when nil; tests override it to drive the scheduler
// without a real clock or a real ticker.
func New(jobs []Job, loc *time.Location, runs store.CronRunStore, sessions store.SessionStore, enqueue Enqueuer, log *slog.Logger, now func() time.Time) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	running := make(map[string]*atomic.Bool, len(jobs))
	for _, j := range jobs {
		running[j.Name] = &atomic.Bool{}
	}
	return &Scheduler{
		jobs:      jobs,
		loc:       loc,
		runs:      runs,
		sessions:  sessions,
		enqueue:   enqueue,
		log:       log,
		gron:      gronx.New(),
		now:       now,
		lastFired: make(map[string]string, len(jobs)),
		running:   running,
	}
}

// LogNextDue logs every enabled job's next due time (via gronx.NextTickAfter)
// in the scheduler's configured location, at info level, so a misconfigured
// timezone is obvious from the gateway's startup log instead of silently
// firing at the wrong hour.
func (s *Scheduler) LogNextDue() {
	ref := s.now().In(s.loc)
	for _, job := range s.jobs {
		if !job.Enabled {
			s.log.Info("cron: job disabled", "job", job.Name, "schedule", job.Schedule)
			continue
		}
		next, err := gronx.NextTickAfter(job.Schedule, ref, false)
		if err != nil {
			s.log.Error("cron: compute next due time failed", "job", job.Name, "schedule", job.Schedule, "error", err)
			continue
		}
		s.log.Info("cron: job scheduled", "job", job.Name, "schedule", job.Schedule, "next_due", next.Format(time.RFC3339))
	}
}

// missedTickThreshold is how late a ticker firing can arrive before
// tickWithCatchUp treats the gap as a missed tick (host suspend/resume, a
// long GC pause, SIGSTOP) rather than ordinary ticker jitter.
const missedTickThreshold = 90 * time.Second

// maxCatchUpMinutes bounds how many of the most recently skipped whole
// minutes one missed-tick gap evaluates, so an extreme gap (days of
// suspend) cannot replay an unbounded backlog of due jobs in one burst.
const maxCatchUpMinutes = 10

// Start sleeps until the next wall-clock minute boundary (see alignDelay),
// ticks once immediately, then once every minute until ctx is done. Started
// after a job's due time, it does not replay that miss - the first tick only
// evaluates jobs against the current minute, never anything in the past;
// that is a restart, not a missed tick, and only this already-running
// process's own ticker being delayed (see tickWithCatchUp) counts as one.
func (s *Scheduler) Start(ctx context.Context) {
	s.LogNextDue()

	timer := time.NewTimer(alignDelay(s.now()))
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}
	lastTick := s.now()
	s.tick(lastTick)

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			lastTick = s.tickWithCatchUp(s.now(), lastTick)
		case <-ctx.Done():
			return
		}
	}
}

// tickWithCatchUp evaluates now's tick and, if more than
// missedTickThreshold has passed since lastTick, first evaluates the most
// recently skipped whole minutes (bounded to maxCatchUpMinutes, walking
// backwards from now) - so a ticker firing late while this process kept
// running does not silently swallow a due job's only firing window. Walking
// backwards, not forwards from lastTick, matters once the gap exceeds the
// bound: an hourly job due 3 minutes ago must still fire even after a
// 2-hour suspend, rather than the bounded window being spent on the ten
// minutes right after lastTick - now two hours stale - while the job due
// just before now is never evaluated at all. Returns now, the new lastTick.
func (s *Scheduler) tickWithCatchUp(now, lastTick time.Time) time.Time {
	if gap := now.Sub(lastTick); gap > missedTickThreshold {
		s.log.Warn("cron: missed tick detected; catching up skipped minutes", "gap", gap.String())
		start := lastTick.Truncate(time.Minute).Add(time.Minute)
		earliest := now.Truncate(time.Minute).Add(-maxCatchUpMinutes * time.Minute)
		if start.Before(earliest) {
			start = earliest
		}
		for minute := start; minute.Before(now); minute = minute.Add(time.Minute) {
			s.tick(minute)
		}
	}
	s.tick(now)
	return now
}

// alignDelay returns how long to wait from now until the next whole
// minute, so a job due at HH:MM fires within about a second of that
// boundary instead of up to 59s late.
func alignDelay(now time.Time) time.Duration {
	next := now.Truncate(time.Minute).Add(time.Minute)
	return next.Sub(now)
}

// tick evaluates every enabled job's schedule against now (converted to the
// scheduler's configured location) and fires or skips each due one.
// Unexported so tests can drive it directly with fabricated clock values
// instead of waiting on a real ticker.
func (s *Scheduler) tick(now time.Time) {
	local := now.In(s.loc)
	for _, job := range s.jobs {
		if !job.Enabled {
			continue
		}

		due, err := s.gron.IsDue(job.Schedule, local)
		if err != nil {
			// A schedule that fails to evaluate is a config bug validation
			// should have already caught; log it, record the attempt, and
			// do not fire - never silently treat an evaluation error as
			// "not due".
			s.log.Error("cron: evaluate schedule failed; not firing", "job", job.Name, "schedule", job.Schedule, "error", err)
			s.recordRun(job.Name, "", "error", err.Error(), local, local)
			continue
		}
		if !due {
			continue
		}
		s.tryFire(job, local)
	}
}

// tryFire applies both required guards, in order, before actually
// enqueuing job's turn:
//
//  1. Minute de-dup: skip silently (no cron_runs row - this is not a new
//     event, just the same due minute observed again) if job already fired
//     for this wall-clock minute.
//  2. Overlap: CAS the per-job running flag; if a prior fire is still in
//     flight, record a "skipped" row and return.
//
// lastFired is set before enqueueing, per the phase spec, so a second tick
// landing before the turn even starts still sees the minute as handled.
func (s *Scheduler) tryFire(job Job, local time.Time) {
	key := local.Format(wallClockMinuteLayout)

	s.mu.Lock()
	if s.lastFired[job.Name] == key {
		s.mu.Unlock()
		return
	}
	running := s.running[job.Name]
	if !running.CompareAndSwap(false, true) {
		s.mu.Unlock()
		s.recordRun(job.Name, "", "skipped", "", local, local)
		return
	}
	s.lastFired[job.Name] = key
	s.mu.Unlock()

	s.fire(job, local)
}

// fire resolves the job's session, writes a "started" cron_runs row, and
// enqueues the synthetic turn. Any failure resolving the session or writing
// the row is logged and, in the session-resolution case, releases the
// running flag immediately (there is nothing in flight to clear it later).
func (s *Scheduler) fire(job Job, local time.Time) {
	ctx := context.Background()

	threadID := ""
	if job.Ephemeral {
		threadID = ephemeralThreadID(local)
	}

	sess, err := s.sessions.Ensure(ctx, "cron", job.chatID(), threadID)
	if err != nil {
		s.log.Error("cron: ensure session failed; not firing", "job", job.Name, "error", err)
		s.recordRun(job.Name, "", "error", fmt.Sprintf("ensure session: %v", err), local, local)
		s.running[job.Name].Store(false)
		return
	}

	run := &store.CronRun{JobName: job.Name, SessionID: sess.ID, Status: "started", StartedAt: local}
	if err := s.runs.Append(ctx, run); err != nil {
		s.log.Error("cron: append started run row failed", "job", job.Name, "error", err)
	}

	in := channel.Inbound{
		Channel:   "cron",
		ChatID:    job.chatID(),
		ThreadID:  threadID,
		Text:      job.Prompt,
		DeliverTo: job.DeliverTo,
		Timeout:   job.Timeout,
		OnDone:    s.onDone(job, run.ID, sess.ID),
	}
	s.enqueue(in)
}

// onDone builds the dispatcher's OnDone callback for one fire: it finishes
// the cron_runs row (ok/error, with a finished timestamp), clears the
// running flag so a later tick can fire again, and - for ephemeral jobs -
// deletes the run's session. OnDone fires once the turn itself is fully
// done (every history append complete), which is the earliest point an
// ephemeral session can be deleted without truncating its own history; the
// dispatcher has no later hook tied to the reply actually reaching the
// wire, and the reply text it delivers is already captured in the turn
// result independent of the session row.
func (s *Scheduler) onDone(job Job, runID int64, sessionID string) func(error) {
	return func(turnErr error) {
		defer s.running[job.Name].Store(false)

		status, errMsg := "ok", ""
		if turnErr != nil {
			status, errMsg = "error", turnErr.Error()
		}
		if err := s.runs.Finish(context.Background(), runID, status, errMsg, time.Now()); err != nil {
			s.log.Error("cron: finish run row failed", "job", job.Name, "run_id", runID, "error", err)
		}

		if job.Ephemeral {
			if err := s.sessions.Delete(context.Background(), sessionID); err != nil {
				s.log.Error("cron: delete ephemeral session failed", "job", job.Name, "session_id", sessionID, "error", err)
			}
		}
	}
}

// recordRun appends a cron_runs row for an event that never became an
// in-flight turn (an evaluation error, or an overlap skip), so both started
// and finished are the same instant. Store failures are logged, never
// propagated - losing a history row must never stop or crash the
// scheduler.
func (s *Scheduler) recordRun(jobName, sessionID, status, errMsg string, started, finished time.Time) {
	run := &store.CronRun{JobName: jobName, SessionID: sessionID, Status: status, Error: errMsg, StartedAt: started, FinishedAt: &finished}
	if err := s.runs.Append(context.Background(), run); err != nil {
		s.log.Error("cron: append run row failed", "job", jobName, "status", status, "error", err)
	}
}

// ephemeralThreadID returns a synthetic thread id unique to one run, so
// Ensure creates a fresh session identity every time instead of reusing the
// job's persistent one - mirrors resolveCLISession's --new path in
// internal/cli/prompt_cmd.go.
func ephemeralThreadID(now time.Time) string {
	return fmt.Sprintf("run-%d", now.UnixNano())
}
