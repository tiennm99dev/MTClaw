# Phase 8 Implementation Report: Cron Scheduler

## Executed Phase
- Phase: phase-08-cron-scheduler
- Plan: `plans/260731-2219-mtclaw-core-system/`
- Status: completed

## Files Modified/Created

**Created**
- `internal/cron/job.go` — `Job` type, `JobsFromConfig` converter, `chatID()` helper
- `internal/cron/scheduler.go` — `Scheduler`, aligned ticker, `tick`/`tryFire`/`fire`/`onDone`, wall-clock minute de-dup + overlap CAS
- `internal/cron/scheduler_test.go`, `internal/cron/job_test.go`
- `internal/cli/cron_cmd.go` — `cron list`, `cron run <name>`
- `internal/gateway/cron_approver_test.go` — approver-mux unit test + full-turn integration test (unmatched exec refused, allow-listed runs)

**Modified**
- `internal/config/validate.go` — `gronx.IsValid`, `deliver_to` reachability (telegram enabled + allow_from/group), `session` enum, `timeout>0`, prompt/name checks, every message names the job
- `internal/config/validate_test.go`, `internal/config/load_test.go` — updated for the now-stricter cron validation (previously-passing invalid-schedule cases now correctly fail)
- `internal/gateway/gateway.go` — builds `cron.Scheduler` when `cron.enabled`, starts it in `Run`'s goroutine group, defensive `time.LoadLocation` re-check
- `internal/gateway/dispatch_test.go` — added `DeliverTo`/`NoReply`/`OnDone`/`Timeout` tests (dispatch.go itself needed **no** code change: phase 7 already implemented this generically)
- `internal/gateway/lock.go`, `internal/gateway/lock_test.go` — added `Held(path)` (read-only lock-liveness check, no side effects)
- `internal/store/store.go`, `internal/store/sqlite/cron_runs.go`, `internal/store/sqlite/store_test.go` — added `CronRunStore.Finish` (moves a `started` row to `ok`/`error` with a finished timestamp)
- `internal/cli/root.go` — registers `cron` command
- `go.mod`/`go.sum` — `github.com/adhocore/gronx v1.20.0` (direct dependency after `go mod tidy`)

## Tasks Completed
- [x] gronx dependency added; API verified against vendor source (`IsDue` is `(*Gronx).IsDue(expr string, ref ...time.Time) (bool, error)`; `IsValid`/`NextTickAfter` package-level)
- [x] Config validation extended: unique/non-empty names, `gronx.IsValid`, `time.LoadLocation`, `deliver_to` reachability, `session` enum, `timeout>0`, prompt non-empty — every message names the job
- [x] `job.go`: `Job` type, config conversion, chat-id derivation
- [x] `scheduler.go`: aligned start, per-minute tick, two independent guards (minute de-dup, overlap CAS), started/skipped/error/ok `cron_runs` bookkeeping, ephemeral session cleanup via `OnDone`
- [x] Dispatcher `DeliverTo`/`Timeout`/`OnDone`/`NoReply` — verified already correct from phase 7; added tests only
- [x] `cron_cmd.go`: `list` (schedule, next-due via `NextTickAfter`, enabled, last status/time) and `run <name>` (print-only default, `--deliver`, `--ephemeral`, gateway-lock refusal for persistent jobs)
- [x] Gateway integration: scheduler started in the run group, next-due logged at startup
- [x] Full test suite per acceptance criteria (see below)

## Tests Status
- Type check / build: `CGO_ENABLED=0 go build ./...` — pass
- `gofmt -l .` — clean
- `go vet ./...` — clean
- `go test ./...` — all packages pass
- `go test -race ./internal/cron/... ./internal/gateway/... ./internal/store/...` — pass
- `go mod tidy` — clean, no diff beyond gronx becoming a direct dependency

Coverage against the phase's required test list:
- gronx contract (instance method, `(bool, error)`, malformed expr errors) — `TestGronxContract_IsDueIsInstanceMethodReturningError`
- Alignment computed, not slept — `TestAlignDelay_ComputesDelayToNextMinuteBoundary`; `Start()` exercised for real with a stepping fake clock in `TestScheduler_Start_AlignsAndFiresThenStopsOnCtxDone`
- DST, fixed non-host `*time.Location` (`America/New_York`), injected clock — `TestScheduler_DST_SpringForward_SkippedHourDoesNotFire`, `TestScheduler_DST_FallBack_RepeatedHourFiresOnlyOnce`
- Overlap — `TestScheduler_OverlapGuard_RecordsSkippedNotDoubleRun`
- Minute de-dup — `TestScheduler_MinuteDedup_TwoTicksSameMinute_ExactlyOneRun`
- Missed runs not replayed — `TestScheduler_MissedRun_NotReplayed`
- Persistent vs ephemeral session behavior — `TestScheduler_PersistentJob_SharesSessionAcrossRuns`, `TestScheduler_EphemeralJob_FreshSessionPerRun_DeletedAfter`
- Delivery to `deliver_to` / `NoReply` sends nothing — `TestDispatch_DeliverTo_RoutesReplyToOverrideNotOrigin`, `TestDispatch_NoReply_WithDeliverTo_SendsNothing` (gateway package)
- Cron-approver refusal / allow-listed runs — `TestApproverMux_UnrecognizedOrCronChannel_FallsBackToDenyAll`, `TestCronTurn_ExecApprover_UnmatchedRefused_AllowListedRuns`
- Validation table naming the job — `TestValidate_CronJobs`, `TestValidate_CronDeliverTo`
- Missed-run non-replay is additionally implied by the scheduler having no catch-up path at all (verified by the same `TestScheduler_MissedRun_NotReplayed`)

## Design Decisions Worth Flagging

1. **Minute de-dup key is a formatted wall-clock string (`"2006-01-02T15:04"` in the configured location), not `now.Truncate(time.Minute)` on the raw instant.** `time.Time.Truncate` operates on the absolute instant per its own docs, so during a DST fall-back the same local `HH:MM` occurring twice (an hour apart in absolute time — confirmed empirically: `01:30 -04:00` and `01:30 -05:00`, exactly 3600s apart) would NOT be recognized as "the same minute" by a raw `Truncate`-based guard, and the job would genuinely fire twice. The phase's own acceptance criterion is "fall back must not fire twice," which only holds if the de-dup key is wall-clock-based. This is documented in `wallClockMinuteLayout`'s doc comment and directly exercised by `TestScheduler_DST_FallBack_RepeatedHourFiresOnlyOnce`. Spring-forward needs no special handling either way (the skipped local time never occurs as a `now` reading).
2. **`CronRunStore.Finish` added.** The phase text describes a started row later "updated... to ok/error," but the interface only had `Append`/`List`. Added `Finish(ctx, id, status, errMsg, finishedAt)` (UPDATE by id) to `store.CronRunStore` + sqlite impl, with tests. This was necessary for the started→terminal bookkeeping the phase explicitly requires; not in the phase file's listed file-ownership, but a minimal, additive, unavoidable extension.
3. **`gateway.Held(path)` added to `lock.go`.** `cron run` needs a read-only "is a gateway holding this lock" check for the persistent-job refusal; `Acquire` is side-effecting (creates/removes the file) and unsuitable. `Held` reads and checks liveness only, never mutates. Not in the phase's listed file-ownership either, but required for the explicitly-specified `cron run` refusal behavior.
4. **Ephemeral session deletion happens in `OnDone`, not after the reply is actually sent over the wire.** The dispatcher's `OnDone` fires once the turn is fully done (`before` `d.reply`, per `dispatch.go`), which is also the earliest point at which every history append for that turn is guaranteed complete. The reply's text is already captured in `result.Text` independent of the session row by that point, so deleting there does not truncate or race the delivery. There is no dispatcher hook tied to the network send completing, so this is both correct and the only available integration point.
5. **`cron.jobs[].session` is validated as an enum (`persistent`/`ephemeral`)** — not explicitly demanded by the task's validation bullet list, but cheap, load-bearing (it's exactly what `JobsFromConfig` branches on), and consistent with the rest of the phase's defensive validation.
6. **Existing tests updated, not just extended**: `internal/config/validate_test.go`'s `TestValidate_CronScheduleNotValidatedInPhase1` and `internal/config/load_test.go`'s matching case asserted `wantErr: false` for an invalid cron expression — correct for phase 1, superseded by phase 8. Both renamed/updated to assert the new (correct) rejection, naming the job.

## Issues Encountered
None blocking. No file-ownership conflicts (phases 1-7 already complete per the task brief; internal/cron did not exist).

## Next Steps
- Phase 9 (Hardening and Release) depends on this phase completing, per `plan.md`'s dependency graph — now unblocked.
- `docs/security.md` (owned by phase 9, per the phase file's own risk note) should state that `tools.exec.allow` entries are effectively cron's permission grant, since `DenyAllApprover` makes an unmatched command in a cron turn unrunnable no matter what.

## Unresolved Questions
None blocking. One judgment call worth surfacing: the phase file's pseudocode literally says `lastFired` truncated via `now.Truncate(time.Minute)`; I implemented the de-dup key as a wall-clock-formatted string instead, per point 1 above, since the literal reading does not actually satisfy the phase's own "fall back must not fire twice" requirement (verified empirically before implementing). Flagging in case the plan's author intended something else by that requirement.
