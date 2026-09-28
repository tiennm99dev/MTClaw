# CLI / config / logging / version / build / CI / docs fix pass

Date: 2026-09-28. Branch `refactor/260928-full-review`. Scope:
`code-reviewer-260928-0953-cli-config-docs-ci-review.md`, constrained by the
decisions in `plans/260928-1041-third-round-review-fixes/plan.md`, plus the
cross-cutting phase-1 follow-ups (`DisableEnvironRead` wiring, doctor's
default shell, `refused_too_long` decider case, the `ExpirePending` channel
filter) and R6 (repo-wide plan/phase/finding-code comment cleanup).

Every finding in the source review was re-verified against current code
before fixing, since that review predates phase 1's agent/provider/store,
tools/policy, and telegram/gateway/cron passes.

## Files touched

Owned: `internal/cli/**`, `internal/config/**`, `internal/logging/**`,
`internal/version/**`, `main.go`, `Makefile`, `.github/workflows/**`,
`README.md`, `docs/**`.

Cross-cutting, explicitly authorized:
`internal/gateway/lock.go` (+test), `internal/gateway/gateway.go` (B9),
`internal/tools/registry.go` (+`registry_test.go`, R3),
`internal/channel/telegram/gating.go` (+test, B4),
`internal/store/store.go`, `internal/store/sqlite/approvals.go` (+test),
`internal/channel/telegram/approver.go` (+test) (B11 from the telegram
report - `ExpirePending` channel filter).

R6 repo-wide comment sweep additionally touched (comments/test names only,
no behavior change): `internal/gateway/dispatch_test.go`,
`internal/gateway/gateway_test.go`, `internal/provider/errors_test.go`,
`internal/channel/telegram/{chunk,render,send,approver,poll}_test.go`.

New files: `internal/cli/terminal.go` (+test), `internal/cli/sessions_cmd_test.go`,
`internal/cli/approvals_cmd_test.go`, `internal/config/load_unix_test.go`,
`internal/version/version_test.go`.

No files outside this list were modified.

## Bugs: fixed (test) / skipped (reason)

**B1/B10/B11 (CI/release toolchain and gating) - fixed.** `release.yml` now
sets up Go via `go-version: stable` + `check-latest: true` (moving tag,
matches the workspace's pinning policy) instead of `go-version-file: go.mod`.
`ci.yml` keeps its existing 3-OS matrix pinned to the `go.mod` minimum and
adds a new `test-stable` job (ubuntu, `stable` + `check-latest`) plus a
`govulncheck` job (`golang/govulncheck-action@v1`, stable Go). All four
actions bumped to their current majors: `actions/checkout@v7`,
`actions/setup-go@v7`, `actions/upload-artifact@v7`,
`softprops/action-gh-release@v3` (verified via each project's own
release notes/marketplace listing - v3 of `action-gh-release` moved to
Node 24, matching the runner deprecation). `release.yml` now runs `go vet`
and `go test` on the tagged commit before building anything, so a tag
pushed from a branch that never ran CI can no longer publish untested
artifacts. Verified `govulncheck ./...` locally with the sandbox's go1.27.1
toolchain (the stable equivalent): 0 reachable vulnerabilities (1 vuln in a
required-but-uncalled module, expected per `-show verbose`).

**B2 via R1 (help/completion fail without a config) - fixed.** Replaced the
two hard-coded `CommandPath()` string lists (`skipsConfigLoad`,
`isReadOnlyCommand`) with a `cobra.Command.Annotations["config"]` declared
on every runnable command (`configNone`/`configInspect`/`configFull`,
`root.go`). `configLevel` treats an unannotated cobra built-in (`help`,
`completion` and its shell subcommands, `__complete`/`__completeNoDesc`) as
`configNone`; any other unannotated command defaults to `configFull` (the
safer, pre-existing behavior). Tests: `TestHelpAndCompletionWorkWithoutConfig`
(`help`, `--help`, `completion bash`, `completion zsh` all run against a
fresh `HOME`), `TestEveryRunnableCommandDeclaresAConfigAnnotation` (walks
the whole tree, fails if any `Runnable()` command omits the annotation -
this is what makes a future renamed/added command that forgets to annotate
itself a caught mistake instead of a silent gap).

**B3 (6/7-field cron schedule silently never fires) - fixed.**
`isValidCronSchedule` requires exactly 5 whitespace-separated fields or an
`@macro` before ever calling `gronx.IsValid`. Tests:
`TestValidate_CronScheduleFieldCount` (6-field seconds-first and 7-field
year-suffixed both rejected; plain 5-field and `@daily` both pass).

**B4 (adding any `groups` entry defaults `require_mention` to `false`) -
fixed per the plan's binding decision.** `TelegramGroupConfig.RequireMention`
is now `*bool` with a new `MentionRequired()` accessor (`nil` or `true` means
required); `gating.go`'s `decide` calls `group.MentionRequired()` instead of
reading the field directly. `config.Bool(b bool) *bool` is the shared
literal-construction helper (used in `defaults.go` and every test that used
to write `RequireMention: true/false`). This does not attempt the more
invasive "merge with `*`" alternative the review's own unresolved-questions
section flagged as the larger option - the plan decided the smaller one.
Tests: `TestTelegramGroupConfig_MentionRequiredDefaultsTrueWhenUnset` (config
package), `TestDecide_GroupOmittingRequireMentionStillRequiresOne` (telegram
package: a listed group with only `allow_from` set, no `require_mention` key
at all, still gates on mention).

**B5 (no `time/tzdata`, `cron.timezone` fails to load on Windows/scratch
Linux) - fixed.** `main.go` imports `_ "time/tzdata"` (~450 KB), documented
inline.

**B6 (`onboard` overwrites `prompts/AGENTS.md`) - fixed.**
`writeStarterAgentsFile` now opens with `O_CREATE|O_EXCL`; on `ErrExist` it
keeps the existing file and prints a note instead of truncating it.
`writeOnboardConfig` also switched to `O_CREATE|O_EXCL`, closing the
stat-then-write race between `runOnboard`'s initial existence check and the
final write (a concurrent second `onboard` run in that gap now gets
`errConfigAlreadyExists` instead of silently overwriting the first one's
result). Test: `TestRunOnboard_PreservesExistingCustomizedAgentsMD`.

**B7 (first Ctrl-C at a secret prompt is ignored; second leaves echo off) -
fixed.** `stdioPrompter.Secret` now calls `term.GetState` before reading,
runs `term.ReadPassword` on its own goroutine, and `select`s on
`p.ctx.Done()`; on cancellation it calls `term.Restore` itself and returns
promptly instead of blocking until another byte arrives. Test:
`TestStdioPrompter_Secret_InterruptAbortsBlockedRead` covers the non-terminal
fallback path (falls through to the same ctx-aware `readLine` `Text` already
uses) hermetically; the terminal-attached branch's echo-restore itself needs
a real pty to drive end to end and is not covered by a new automated test -
flagged as a manual-verification item.

**B8 (untrusted content printed raw to the terminal) - fixed.** New
`sanitizeForTerminal`/`sanitizeForTable` helpers (`internal/cli/terminal.go`,
mirroring `internal/tools`'s own `escapeControlAndBidi`, which is unexported
in a different package) escape C0/C1 controls (except `\n`/`\t`), DEL, and
Unicode bidi-override/isolate characters as `\xNN`/`\uNNNN`; the table
variant additionally escapes real `\n`/`\t` so an embedded one cannot splice
extra rows/columns into a `tabwriter` table. Applied to `sessions show`
(`Content`, `ToolCalls`, `ToolCallID`, `ToolName`), `approvals list`
(`Command`), and `prompt`'s final printed reply. Tests:
`TestSanitizeForTerminal_*`, `TestSanitizeForTable_*` (unit),
`TestSessionsShowCmd_SanitizesUntrustedContent`,
`TestApprovalsListCmd_SanitizesEmbeddedControlCharsAndNewlines` (through the
real commands, real store).

**B9 (instance lock keyed to `$HOME`, not the database) - fixed per the
plan's binding decision (one gateway per database).** New
`gateway.LockPath(cfg) string` returns `cfg.Storage.Path + ".lock"`.
`gateway.New` derives the lock path from it (and creates that directory
before `Acquire`, since it now runs before `sqlite.Open`'s own MkdirAll).
`cron_cmd.go`'s `refuseIfGatewayLocked` and `doctor_checks.go`'s
`checkInstanceLock` both switched to the same function - `checkInstanceLock`
lost its `stateDir`-closure shape entirely, since the lock path now depends
on `cfg`, not a fixed directory. Documented in `docs/configuration.md`
(`storage.path` row) and `docs/architecture.md`. Tests: `TestLockPath_IsNextToTheDatabase`,
updated `TestCheckInstanceLock` (derives the Acquire path via `LockPath`
instead of a hard-coded `gateway.lock` name), new
`TestCronRunCmd_RefusesWhilePersistentJobsGatewayLockIsHeld` (end to end:
acquires the real lock path a `cron run` would check, confirms refusal, then
confirms `--ephemeral` bypasses it).

**B12 (unknown `log.level`/`log.format` load silently as info/text) -
fixed.** New `validateLog` in `validate.go` rejects both outright, matching
every other enum field. The `--log-level` flag is separately validated in
`prepare` (it overwrites `cfg.Log.Level` *after* `LoadFile` already
validated the file's own value, so the flag needs its own check). Tests:
`TestValidate_LogFields`, `TestPrepare_InvalidLogLevelFlagIsRejected`.

**B13 (`cron list` shows the finish time as "LAST RUN") - fixed.**
`newCronRunCmd` captures `started := time.Now()` before `loop.Run` and
passes it through to `recordManualRun`, which now takes `started` as a
parameter instead of reusing one `now` for both `StartedAt` and
`FinishedAt`. Test: `TestCronRunCmd_RecordsRunWithStartedBeforeFinished`
(end to end, asserts `StartedAt <= FinishedAt` against a real store).

**B14 (doctor migrates a live/behind-schema DB) - fixed, with one
deliberate deviation from the review's literal suggestion.** `checkDatabase`
now falls back to a read-write open only when the read-only open failed
because the file does not exist at all (`os.IsNotExist`); any other
read-only failure (most notably a schema-behind database, which a read-only
connection refuses rather than silently migrate) is reported as `FAIL` with
an actionable message ("restart the gateway... to migrate it; doctor itself
never migrates a database it only opened to inspect"), never retried
read-write. The review's suggested text split this into a WARN
specifically for the schema-behind case; I kept it as FAIL for every
non-`ErrNotExist` failure, since distinguishing "behind schema" from "some
other read-only failure, e.g. a permission error" would need a sentinel
error type from `internal/store/sqlite` (a package I do not own) rather than
string-matching an error message. FAIL is also the more conservative
choice for an unidentified failure. Test:
`TestCheckDatabase_BehindSchemaIsReportedNotSilentlyMigrated` (asserts the
schema version is provably untouched by a second, independent open).

**B15 (`FileNotFoundError` never checked; no onboard pointer) - fixed.**
`prepare` now does `errors.As` on `LoadFile`'s error and appends
"; run `mtclaw onboard` to create one" when it is a `*config.FileNotFoundError`.
Test: `TestPrepare_MissingConfigPointsAtOnboard`.

**B16 (`sessions rm` classified read-only, never logs to a file) - fixed
as a consequence of R1.** `sessions rm` now explicitly declares
`configAnnotation(configFull)` (previously it fell into the read-only list
by an oversight); every annotation is explicit per command, so this is no
longer implicit either way. Covered by the same
`TestEveryRunnableCommandDeclaresAConfigAnnotation` plus existing
`TestOpenStore_WriteModeCreatesStorageDir_ReadOnlyDoesNot`.

**B17 (`cron list` fails outright with no database yet) - fixed.** It now
stats `storage.path` first; a missing file means every job's last-run
columns render as `-` without ever calling `openStore`, instead of failing.
Test: `TestCronListCmd_NoDatabaseYetShowsDashesInsteadOfFailing` (also
asserts the database is still not created as a side effect of listing).

**B18 (TOCTOU between the lock check and the store open) - accepted and
documented, not fixed**, per the review's own "accept and document" option.
`newCronRunCmd`'s doc comment now states the window explicitly rather than
implying the check is atomic with the run.

**B19 (`--session X --new` silently ignores `--new`) - fixed.**
`cmd.MarkFlagsMutuallyExclusive("session", "new")`. Test:
`TestPromptCmd_SessionAndNewAreMutuallyExclusive` (through the real command
tree, so it also proves cobra's flag-group validation actually fires before
any turn starts).

**B20 (`deliver_to` reachability enforced even when cron/job disabled) -
fixed.** `validateCron` only calls `validateCronDeliverTo` when
`cfg.Cron.Enabled && job.Enabled`. Existing `TestValidate_CronJobs`/
`TestValidate_CronDeliverTo` updated to explicitly enable cron/the job where
the reachability check is exercised (previously implicit via test fixtures
that happened to leave both at their zero-value `false`, which no longer
reaches the check at all). New:
`TestValidate_CronDeliverToSkippedWhenCronOrJobDisabled` (three cases: cron
disabled, job disabled, both enabled still enforces).

**B21 (onboard's refuse-to-overwrite exits 0) - fixed.** `runOnboard`
returns a new sentinel `errConfigAlreadyExists` (the refusal message is
still printed either way). Test: updated
`TestRunOnboard_RefusesToOverwriteExistingConfig` now asserts
`errors.Is(err, errConfigAlreadyExists)`.

**B22 (unbounded secret-file read; empty file shows as "set") - fixed.**
`resolveSecret` now `os.Stat`s first (refusing anything that is not
`Mode().IsRegular()` - closes a FIFO/`/dev/zero`-class hang before ever
opening it) and reads through `io.LimitReader(f, maxSecretFileBytes)` (64
KiB). A value that is empty after trimming reports source `"unset"`, not
`"file:<path>"`. Tests: three new subtests under `TestLoad_SecretEnvOverlay`
(empty file -> unset, over-cap file truncated to exactly the cap) plus
`TestLoad_SecretFileRefusesNonRegularFile` in a new `//go:build unix` file
(a FIFO reference cannot compile on Windows at all, hence the split, mirror
of `internal/tools`'s own `fifo_unix_test.go`/`fifo_windows_test.go`).

**B23 (`sessions list` N+1 query) - mitigated, not fixed at the store
level.** Added `--limit` (default 50) to `sessions list`, per the review's
second, smaller option; the store-level "return the count from the list
query" option would change `SessionStore.List`'s return shape, a package I
was not asked to own for this. Test: `TestSessionsListCmd_LimitFlagBoundsResults`.

**B24 (still exits 1 on interrupt) - fixed.** `Execute` now runs its result
through `finalizeExecuteError(err, ctx.Err())`, returning a new
`cli.ErrInterrupted` when a command failed and the root context had already
ended (a signal); `main.go` maps that to exit 130. Test:
`TestFinalizeExecuteError` (all four quadrants: interrupted, own-failure,
succeeded-despite-interrupt, plain success).

**B25 (`make release` needs `shasum` on macOS; `make fmt` never fails) -
fixed.** `checksum := $(shell command -v sha256sum || echo "shasum -a 256")`
resolved once per invocation. `fmt` now runs `gofmt -w .`; a new `fmt-check`
target (what CI itself should eventually call, though `ci.yml`'s own inline
steps were left as-is since they were not broken) runs `gofmt -l .` and
exits non-zero on any hit. Verified locally: `make fmt-check` (clean repo,
exit 0) and `make release VERSION=v9.9.9-test` (built and checksummed all
five targets via `sha256sum`).

**B26 (build-wall-clock `Date`; unneeded `fetch-depth: 0`) - fixed via
R2.** See R2 below - `Date` is no longer stamped by the workflow at all;
it comes from the commit's own `vcs.time`, identical across every build of
the same commit. `fetch-depth: 0` dropped from `release.yml` (nothing needs
full history: `runtime/debug.ReadBuildInfo`'s VCS stamping works off the
current commit and working-tree status, not history depth).

## Refactors

**R1 (annotations replace the two `CommandPath()` lists) - done.** See
B2/B16 above. `root.go`'s `configLevel`/`configAnnotation`/
`isCobraBuiltinMetaCommand` are the new surface; every command constructor
in `internal/cli` sets `Annotations` explicitly.

**R2 (version via `runtime/debug.ReadBuildInfo`; release calls `make
release`) - done.** `internal/version/version.go` rewritten: `Version` is
still settable via `-X`, but `Commit`/`Date` resolve automatically from
`ReadBuildInfo`'s `vcs.revision`/`vcs.time` (with a `-dirty` suffix on
`vcs.modified`) the first time `String()` is called, only when `-ldflags`
left them at their zero defaults. `Makefile`'s `LDFLAGS` now sets only
`Version`; `RELEASE_LDFLAGS` and the `release` target are otherwise
unchanged (still `-trimpath`, still five cross-compiled targets).
`release.yml` was rewritten to call `make release VERSION=${{
github.ref_name }}` instead of hand-rolling the build loop, so there is
exactly one place that knows the recipe. Verified: `make build` locally
reports a real commit/date pulled from this git checkout with zero `-X`
flags for either. Tests: `internal/version/version_test.go` (format,
resolve-from-`ReadBuildInfo`, "-ldflags value is never overwritten").

**R3 (`tools.exec.mode: off` collapses into `enabled: false`) - done.**
`config.Load` calls a new `normalizeExecMode` right after decode: `mode:
"off"` forces `Enabled = false` (the `Mode` field itself is left as
written, for `config show`'s benefit). `tools.Registry.New`'s exec-tool
gate is now `cfg.Tools.Exec.Enabled` alone. `docs/configuration.md`'s
`tools.exec.enabled`/`tools.exec.mode` rows rewritten to state the
normalization instead of contradicting each other (the old D5 drift). Test:
`TestLoad_ExecModeOffNormalizesToEnabledFalse`; three `internal/tools`
tests that used to set `Mode = "off"` purely to keep the exec tool out of
an unrelated assertion were switched to `Enabled = false` (the actual
contract registry.go now checks), and the one test that specifically
exercised `Mode: "off"` against `New` directly (bypassing `config.Load`'s
normalization) was removed as redundant with `TestNew_ExecDisabled_ExecToolNotRegistered`
once `Mode` stopped being registry.go's own concern.

**R4 (delete dead doctor checks; add the missing one) - done, with a
narrower removal than proposed.** Deleted `checkAllowlistNonEmpty`
entirely (genuinely dead: `config.Validate` already guarantees this, and
every `Check` runs only against an already-validated `cfg`). Deleted the
per-job `gronx.IsValid` call in `checkCron` (also dead: `gronx.NextTickAfter`
already returns an error for an unparseable expression via its own internal
`Segments`/`IsDue` call, so the explicit pre-check added nothing).
**Kept** `checkCron`'s `time.LoadLocation` re-check, unlike the review's
literal suggestion to remove both: unlike the allowlist check, `checkCron`
is called directly (not only through `runDoctor`) by its own existing unit
test with a deliberately-invalid timezone, and `gronx.NextTickAfter(...,
time.Now().In(loc), ...)` panics on a `nil` `*time.Location` rather than
erroring - removing the guard would turn a hypothetical future caller
bypassing `Validate` into a panic instead of a FAIL row. Added
`checkSystemPromptFiles` (new "system_prompt_files readable" row): opens
each `agent.system_prompt_files` entry, reporting any that fail. Tests:
`TestCheckSystemPromptFiles`; `TestCheckAllowlistNonEmpty` removed (function
gone); `TestCheckCron` unchanged and still passes (the schedule-validity
FAIL case is now reached via `NextTickAfter`'s own error instead of the
deleted pre-check, same observable behavior).

**R5 (export shared literals) - partially done.** `gateway.LockPath` -
done (see B9), fixes the `"gateway.lock"` + `config.StateDir()` duplication
by removing the state-dir-based path entirely. `tools.ResolveShell` was
already exported by the tools-phase-1 pass under that name (not
`DefaultShell`, but the same intent); `doctor_checks.go`'s own
`defaultShellArgv()` duplicate is deleted, `checkShellExists` now calls
`tools.ResolveShell` directly. **Not done:** exporting the exec_audit
decision-label constants that `approvals_cmd.go`'s `decider()` duplicates
against `tools/policy.go`/`tools/exec.go` - that would mean editing two
files in a package (`internal/tools`) outside every explicit grant in this
task's scope, for a refactor whose only concrete requirement here was the
one new `decider()` case (see below), which I added using the existing
string-literal style the function already uses throughout.

**R6 (remove plan/phase/finding-code references) - done, repo-wide.**
Grepped all of `internal/` for `phase\s*\d+`, `round\s*\d+`,
`\b[BDHLMR]\d{1,2}\b` in comments/test names, and `finding`, both before
starting and again after finishing (the second pass caught several
references I had just introduced myself while writing new tests named
after review finding codes - fixed those too). Net zero matches remain
outside historical `docs/journals/*.md` (a stateful record, not evergreen
docs, left untouched) and one unrelated false positive ("without finding an
existing..." in `path_guard.go`). Touched files outside my normal ownership
for this alone: `internal/gateway/{dispatch,gateway}_test.go`,
`internal/provider/errors_test.go`,
`internal/channel/telegram/{chunk,render,send,approver,poll}_test.go` -
comments and test doc-strings only, no behavioral change, confirmed by the
full test suite passing unchanged.

## Cross-cutting phase-1 follow-ups

- **`tools.DisableEnvironRead()` wiring** - done. Called once at the start
  of both `mtclaw gateway`'s and `mtclaw prompt`'s `RunE` (the two
  process entry points phase 1's tools report flagged as needing it),
  logging a `Warn` on error (platform no-op, or a permission failure)
  without blocking startup. `docs/security.md` updated to state it is
  actually wired in now, not merely exported.
- **`internal/cli/doctor_checks.go`'s duplicated shell-default logic** -
  done (folded into R5 above): now calls `tools.ResolveShell` instead of
  its own `defaultShellArgv()`.
- **`approvals_cmd.go`'s `decider()` missing the `"refused_too_long"`
  case** - done: maps to `"policy"` (matches `denied_rule`/`allowed_rule`,
  since a too-long-to-review command is refused by the policy layer itself,
  before ever reaching a human or the classifier). Test:
  `TestDecider_RefusedTooLongMapsToPolicy`.
- **`internal/store/sqlite/approvals.go`'s `ExpirePending` missing a
  channel filter (B11 from the telegram-gateway-cron report)** - fixed,
  cleanly, since the task authorized editing this file plus the Telegram
  call site for exactly this. `ApprovalStore.ExpirePending` gained a
  `channel string` parameter; the sqlite query added `AND channel = ?`; the
  Telegram approver passes `"telegram"`. This is currently a no-op in
  practice (Telegram is still the only writer of `approvals` rows), but it
  closes the gap the phase-1 report flagged for whenever a second writer
  exists. Tests: updated `TestApprovals_ExpirePending`, new
  `TestApprovals_ExpirePending_ScopedToChannel` (a differently-channeled
  pending row survives a sweep scoped to `"telegram"`); `fakeApprovalStore`
  in `approver_test.go` updated to match.

## Docs drift: all 16 items re-verified against source

D1 (`"*"` / `require_mention` default), D2 (5-field schedule), D3
(missing `system_prompt_files` entry behavior), D4 (`agent.name` is not
display-only), D5 (`mode: off` vs `enabled: false` contradiction), D6
(`cron list`'s disabled-job rendering), D7 (`log.format`'s "If wrong: N/A"),
D8 (which packages import sqlite), D9 (Load's purity claim - already
correct in `architecture.md` from round 2; the one remaining stale
in-code doc-comment on `Load` itself reworded), D10 (stale "gronx is not
yet a dependency" / "a future onboard command" comments), D11 (release
notes citing an internal report / claiming untested targets are executed),
D12 ("phase 5 design document" reference in `security.md`), D13 (module
path vs. real repo owner in README links), D14 (`CGO_ENABLED=0` "used
everywhere" overclaim), D15 (lock location undocumented), D16 (doctor DB
check's stale "never disturbing a live gateway" claim) - all fixed in
`docs/configuration.md`, `docs/telegram-setup.md`, `docs/architecture.md`,
`docs/security.md`, and `README.md`. Specifics worth flagging:

- D13: kept the module path `github.com/tiennm99/MTClaw` per the plan's
  binding decision; changed the README's clone URL and releases-page link
  to the real owner (`tiennm99dev`), and added an honest paragraph next to
  the `go install` line stating it depends on GitHub's repo-rename redirect
  rather than presenting a real version as if `-ldflags` had stamped it.
- D14: reworded to state the exception explicitly (the bare `go install`
  line does not set `CGO_ENABLED` itself).
- Also folded in the phase-1-change alignment items the task called out
  by name: `agent.temperature` optional (already correct from the
  agent/provider/store pass - verified, not re-touched), Telegram HTML
  output (already correct in `architecture.md` from the telegram pass -
  verified), approval length limit and `DisableEnvironRead` (both in
  `security.md`, the latter's wording updated per above),
  process-group kill on Windows (README's Windows note corrected - it
  previously implied Windows had no tree-kill at all, when `exec_windows.go`
  already implements one via `taskkill /F /T`), history budget (256 KiB,
  added to `configuration.md`'s `agent.max_history_turns` row), `/new`
  cancels a running turn (new "Bot commands" section in
  `telegram-setup.md`, since the bot's commands were not documented
  anywhere at all before this), queued messages lost on restart (already
  documented from the telegram pass - verified, not re-touched),
  `require_mention` default (folded into D1's fix, above).

## Test gaps closed

Per the task's explicit list: `cron list`/`cron run`, `sessions show`,
`approvals list`, and approver wiring through the actual commands - all
now have end-to-end tests driving the real `newRootCmd`/`cobra.Command`
tree against a real temp-dir sqlite store (and, for `cron run`, a fake
OpenAI-compatible `httptest.Server`, mirroring the pattern
`internal/provider/openai`'s own httptest tests already use). New files:
`internal/cli/sessions_cmd_test.go`, `internal/cli/approvals_cmd_test.go`;
`internal/cli/cron_cmd_test.go` gained four new tests alongside the
existing approver-wiring one. B2/B3/B4/B6 each have dedicated regression
tests, listed under their own findings above.

## Validation

- `gofmt -l .`: clean.
- `go vet ./...`: clean.
- `go build ./...`: clean.
- `go test -race -count=1 ./...`: all packages pass (repeated after every
  significant edit, not just once at the end).
- `CGO_ENABLED=0 GOOS={linux,windows,darwin} GOARCH={amd64,amd64,arm64} go
  build .`: all three clean; `go vet ./...` also run clean under all three
  `GOOS` values.
- `go mod tidy -diff`: clean (no dependency changes anywhere in this pass).
- `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`: 0 reachable
  vulnerabilities (network was available in this sandbox).
- `make fmt-check`, `make lint`, `make build`, `make release
  VERSION=v9.9.9-test`: all run locally and pass; `make release`'s output
  binary reports a real commit/date pulled automatically from this git
  checkout.
- Smoke test (scratch `HOME`, binary built via `make build`): `mtclaw
  version`, `mtclaw help`, `mtclaw completion bash`, `mtclaw doctor` (with
  and without `--json`) all behave correctly - `doctor` fails with an
  actionable message and the correct non-zero exit code against a config-less
  scratch `HOME`, as expected. No gateway or other long-running process was
  started; build artifacts (`bin/`, `dist/`) and the scratch `HOME` were
  removed afterward.

## Unresolved / flagged for the final review pass

1. B7: the terminal-attached branch of `Secret`'s fix (the actual
   `term.GetState`/`term.Restore` echo-recovery path) has no automated
   test - it needs a real pty, which this sandbox does not have a way to
   drive hermetically. The non-terminal fallback path (same ctx-aware
   mechanism `Text`/`readLine` already use, and already tested there) is
   covered.
2. B14: chose FAIL over the review's suggested WARN for every
   non-`ErrNotExist` read-only-open failure, since distinguishing "behind
   schema" from "some other failure" cleanly would need a sentinel error
   type added to `internal/store/sqlite`, a package outside this phase's
   ownership. Flagging in case the store-owning agent wants to add one
   later and let `doctor` downgrade the schema-behind case specifically to
   WARN.
3. R4: kept `checkCron`'s `time.LoadLocation` re-check (a panic-avoidance
   guard for a direct, non-`runDoctor` caller) rather than removing it
   as the review's literal text suggested - see R4 above for the
   reasoning.
4. R5: the exec_audit decision-label constants duplicated across
   `tools/policy.go`/`tools/exec.go`/`approvals_cmd.go` were not exported,
   since that edit falls outside every explicit ownership grant for this
   phase. The one concrete requirement (a `decider()` case for
   `"refused_too_long"`) is done using the existing literal-string style.
5. B23: mitigated with `--limit` rather than fixed at the store level
   (`SessionStore.List` returning a count alongside each session) - the
   store-level option is a real fix but touches a package/interface this
   phase was not asked to own.

Status: DONE
Summary: All High/Medium findings from the source review are fixed with a
regression test, except B14 and R4 where a narrower, more conservative
version was implemented for a stated reason; all Low findings are fixed or
explicitly accepted/documented (B18) or mitigated (B23); all six refactors
are done, R5 partially (the literal-constants export is out of this
phase's file ownership); all 16 docs-drift items plus every named phase-1
alignment item are corrected; `gofmt`/`go vet`/`go test -race`/three-OS
cross-builds/`go mod tidy -diff`/`govulncheck` are all clean, and `make
release` was verified to build and checksum all five targets locally.
