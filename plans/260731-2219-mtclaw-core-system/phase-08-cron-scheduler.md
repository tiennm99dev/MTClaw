---
phase: 8
title: "Cron Scheduler"
status: completed
priority: P2
dependencies: [7]
effort: ""
---

# Phase 8: Cron Scheduler

## Overview

YAML-defined scheduled prompts: on a cron expression, run a prompt through the agent
loop and deliver the result to a Telegram chat. This is what makes MTClaw proactive
rather than purely reactive — the single extra feature selected beyond the core.

## Requirements

**Functional**
- Jobs declared in `cron.jobs`, evaluated against `cron.timezone`.
- Each job runs a prompt in either a dedicated persistent session or a fresh one.
- Result delivered to a configured chat, chunked like any other reply.
- Overlapping runs skipped, not queued.
- Every run recorded in `cron_runs` with status and error.
- `mtclaw cron list` and `mtclaw cron run <name>` for inspection and manual firing.
- Missed runs (process down) are **not** replayed.

**Non-functional**
- Scheduler runs inside the gateway process; no separate daemon, no OS cron.
- Cron turns reuse the same dispatcher and per-session serialization as chat turns.

## Architecture

`gronx` provides expression matching only; the loop is ours, which is what lets us
control overlap and delivery semantics.

**Verified API** (checked against the gronx README — an earlier draft of this plan got
it wrong in two ways): `IsDue` is a **method on an instance**, not a package function,
and it returns **`(bool, error)`**. `IsValid` *is* package-level. There is also
`NextTickAfter`, which `cron list` should use rather than hand-rolling next-due math.

```go
gron := gronx.New()
due, err := gron.IsDue(job.Schedule, now)   // (bool, error) — do not drop the error
gronx.IsValid("* * * * *")                  // package-level, used by config validation
next, err := gronx.NextTickAfter(expr, ref, false) // powers `cron list`
```

An expression that errors at evaluation time is a config bug that validation should have
caught; log it at error level, record the run as `error`, and do not fire.

```go
ticker := time.NewTicker(time.Minute)  // aligned to the next minute boundary
for range ticker.C {
    now := time.Now().In(loc)
    for _, job := range enabled {
        if due, err := gron.IsDue(job.Schedule, now); err == nil && due {
            fire(job, now)
        }
    }
}
```

Align the first tick to the next wall-clock minute so a job scheduled for `08:00`
does not fire at `08:00:37`.

**Minute de-duplication.** `IsDue` is true for the whole due minute, so "one tick per
minute" is an assumption about the ticker, not a guarantee from gronx. If alignment math
is slightly off, or a tick is delivered early, a fast job can fire twice inside the same
due minute — and the overlap flag below will not stop it, because the first run already
finished. Guard with a per-job `lastFired` truncated to the minute
(`now.Truncate(time.Minute)`); skip when it equals the current minute. The overlap flag
and the minute guard solve two different problems and both are required.

### Config

```yaml
cron:
  enabled: true
  timezone: Asia/Saigon          # IANA name, or "Local"
  jobs:
    - name: morning-briefing     # unique, used as the session key and in logs
      schedule: "0 8 * * *"      # standard 5-field cron
      prompt: "Summarize today's calendar and anything urgent in my notes."
      enabled: true
      session: persistent        # persistent | ephemeral
      timeout: 10m
      deliver_to:
        channel: telegram
        chat_id: "123456789"
```

- `session: persistent` → `Ensure("cron", "job:"+name, "")`, so the job accumulates
  history across days and can say "unlike yesterday…". Costs context growth; the
  phase-4 trim applies.
- `session: ephemeral` → a fresh session each run, deleted after delivery. Cheaper and
  deterministic.
- `deliver_to.chat_id` must be in `channels.telegram.allow_from`, or be a group present
  in `channels.telegram.groups`. Validated at config load (extend phase 1's validator).
  Without this check, a config typo becomes an outbound message to an arbitrary chat.

### Overlap policy

Skip, do not queue. A per-job `atomic.Bool` guards entry; if a run is still in flight
when the next tick matches, record `status: skipped` and move on. Rationale: a daily
briefing that ran long is not worth running twice, and queueing them means a slow job
silently builds a backlog that all fires at once when it recovers.

This guards *concurrent* re-entry only. Repeat firing inside one due minute after a fast
run completes is a separate problem, handled by the minute guard above.

### Missed runs

Deliberately not replayed. If the gateway is down at 08:00, the 08:00 briefing does not
happen. Catch-up needs a persisted "last fired" watermark per job plus a policy for how
stale is too stale — real complexity for a feature whose value is "tell me things at
the right time". A run that fires four hours late is worse than no run. Documented as
intended behaviour.

### Approver for cron turns

A cron turn has no interactive user waiting. Wiring `TelegramApprover` would send
approval buttons to a chat where nobody is expecting them, then block for
`approval_timeout`.

Cron turns therefore use **`DenyAllApprover`** (phase 5), selected by the gateway's
approver mux on `meta.Channel == "cron"`: any command reaching the ask
branch is refused, and the model is told no interactive approver is available so it can
report that in its output. Consequence, and it must be documented: a cron job can only
run commands matched by `tools.exec.allow`. That is the correct default for unattended
execution — if a scheduled job needs a command, allow-list it explicitly.

This is also why `auto` mode does not change cron behaviour: even when the classifier
says "safe", an unmatched command in a cron turn still needs an approver it does not
have. Only the allow-list runs unattended.

### Delivery

Reuse the gateway dispatcher rather than calling the loop directly, so cron turns
inherit per-session serialization and cancellation. The scheduler enqueues a synthetic
`Inbound{Channel: "cron", ChatID: "job:"+name, Text: job.Prompt}` and the dispatcher
routes the result to `deliver_to` instead of back to the origin chat. A `NoReply`
result delivers nothing — a job that decides there is nothing to report should stay
quiet.

## Related Code Files

- Create: `internal/cron/scheduler.go` — aligned ticker, due evaluation, fire/skip
- Create: `internal/cron/job.go` — job type, session resolution, delivery routing
- Create: `internal/cron/scheduler_test.go`, `job_test.go`
- Create: `internal/cli/cron_cmd.go` — `cron list`, `cron run <name>`
- Modify: `internal/config/validate.go` — `deliver_to` reachability, unique names, `gronx.IsValid`, timezone
- Modify: `internal/gateway/gateway.go` — start the scheduler when `cron.enabled`
- Modify: `internal/gateway/dispatch.go` — synthetic inbound with an explicit delivery target

## Implementation Steps

1. Add `github.com/adhocore/gronx`.
2. Extend phase-1 validation: unique non-empty names; `gronx.IsValid(schedule)`;
   `time.LoadLocation(timezone)`; `deliver_to.channel == "telegram"` and telegram
   enabled; `chat_id` reachable per the rule above; `timeout > 0`; prompt non-empty.
3. `job.go`: resolve the session per `session:` mode; build the synthetic inbound.
   `Inbound` gains three cron-only fields — `DeliverTo`, `Timeout`, and
   `OnDone func(error)` — so the dispatcher does not need to know about cron: the
   worker applies `Timeout` when deriving the turn context and invokes `OnDone` when
   the turn finishes. Completion is only knowable via this callback, because the
   dispatcher runs the turn asynchronously in a worker goroutine.
4. `scheduler.go`:
   - `Start(ctx)` sleeps until the next minute boundary, then ticks every minute
   - per tick, for each enabled job, `gron.IsDue(schedule, now)` — handle the error return
   - on due: check the per-job `lastFired` minute guard first, then `CompareAndSwap` the
     running flag; if the flag was already set, write `cron_runs{status: skipped}` and
     continue. Set `lastFired` before enqueueing.
   - otherwise write a `started` run row and enqueue with `Timeout: job.Timeout` and
     an `OnDone` that updates the row to `ok`/`error` with the finished timestamp and
     clears the running flag
   - exit on `ctx.Done()`
5. Dispatcher change: when an inbound carries `DeliverTo`, send the result there;
   suppress delivery on `NoReply`.
6. `cron_cmd.go`:
   - `list` — name, schedule, next due time via `gronx.NextTickAfter` in the configured
     zone, enabled, last run status and time from `cron_runs`
   - `run <name>` — fire once immediately. Runs in-process against the same store, which
     means it does **not** go through a running gateway; it is a test/debug path and
     `--deliver` opts into actually sending rather than printing to stdout. Default is
     print-only, so `cron run` is safe to experiment with. For a `persistent` job it
     refuses to run while the gateway lock is held — the gateway may fire the same
     job's session concurrently and nothing serializes two processes appending to one
     session; pass `--ephemeral` or stop the gateway.
7. Gateway integration: start the scheduler in the run group when `cron.enabled`, log
   each job's next due time at startup so a wrong timezone is obvious immediately.

## Tests / Validation

- **Due evaluation**: injected clock across DST transitions in a non-UTC zone (spring
  forward skips an hour — a `2:30` job simply does not fire that day; fall back must not
  fire twice). Assert with a fixed `time.Location`, not the host zone.
- **Alignment**: first tick lands within a second of a minute boundary.
- **Overlap**: a job whose fake turn outlasts its interval records exactly one `skipped`
  row and does not run twice.
- **Minute de-dup**: drive two ticks inside the same due minute with a fast fake turn;
  assert exactly one run. This is the case the overlap flag does not cover.
- **gronx contract**: assert against the real library that `IsDue` is an instance method
  returning `(bool, error)` and that a malformed expression surfaces as an error rather
  than a silent `false` — the plan was wrong about this API once already.
- **Ephemeral vs persistent**: two runs of a persistent job share a session id and
  accumulate messages; two runs of an ephemeral job do not, and the ephemeral session is
  gone afterwards.
- **Delivery**: result goes to `deliver_to.chat_id`, not to a chat derived from the
  session; `NoReply` sends nothing.
- **Approver**: a cron turn hitting an unmatched exec command is refused with the
  "no interactive approver" message; an allow-listed command runs.
- **Validation**: duplicate job names, invalid expression, unknown timezone, `chat_id`
  not in the allowlist — each rejected at load with a message naming the job.
- **Missed runs**: scheduler started after a job's due time does not fire it.

## Success Criteria

- [x] A job with `* * * * *` fires once per minute, aligned to the boundary
- [x] Result is delivered to the configured chat, chunked
- [x] `NoReply` from a cron turn sends nothing
- [x] A long-running job's next due tick records `skipped`, never a double run
- [x] Two ticks inside one due minute produce exactly one run
- [x] Persistent jobs accumulate history; ephemeral jobs leave no session behind
- [x] A cron turn cannot run an unmatched command, and says why
- [x] An allow-listed command runs unattended
- [x] `mtclaw cron list` shows correct next-due times in the configured timezone
- [x] `mtclaw cron run <name>` prints without delivering unless `--deliver` is passed
- [x] Gateway startup logs each job's next due time
- [x] Config errors in `cron.jobs` name the offending job
- [x] A gateway restart does not replay missed runs

## Risk Assessment

- **Unattended exec is the sharp edge.** `DenyAllApprover` makes cron strictly weaker
  than chat, which is right, but a user who allow-lists a broad pattern to make a job
  work has handed unattended shell to the scheduler. `docs/security.md` must say that
  allow-list entries are effectively cron's permission grant.
- **Timezone and DST** are the classic cron bugs. Tests use a fixed location and an
  injected clock; the host zone must never appear in a test.
- **Minute-granularity ticking** means a job can fire up to ~1s late and, if a tick is
  delayed past a minute boundary under load, could be missed entirely. Acceptable for
  briefings; not acceptable for anything time-critical, which this is not for.
- **Persistent job context growth** is unbounded over months. The phase-4 trim caps it,
  but a persistent job with a large `max_history_turns` gets expensive quietly. `cron
  list` showing token totals would surface it; deferred unless it bites.
- **Cost.** A job every minute with a tool-using prompt is a real bill. Log token usage
  per run and mention the risk in the config docs.
