# Phase 4 Implementation Report: Polish, Docs, Manual Checklist

- Plan: `plans/260808-1921-portable-store-and-verification/`
- Phase file: `plans/260808-1921-portable-store-and-verification/phase-04-polish-docs-and-manual-checklist.md`
- Date: 2026-08-08
- Status: **DONE_WITH_CONCERNS** - every functional requirement of this phase is genuinely done and verified; the one thing withheld is `go test -race ./...` being green, which cannot be claimed truthfully (see "Latent bug found" below) and was never achievable locally in this environment either.

## Summary

Added the `config.yml` extension alias (default branch only, `.yaml` wins on
conflict with one warning, explicit `--config`/`MTCLAW_CONFIG` never
rewritten), six new `ConfigPath` tests, refreshed
`docs/architecture.md`/`docs/configuration.md`/`README.md` to describe the
real post-refactor store seam and the alias, wrote `docs/verification.md`
(the new authoritative split between automated and live-credential
verification), rewrote the v1 plan's acceptance criteria and "Remaining
manual verification" section to point at it with named test citations, and
marked all 4 phases of this plan Completed with acceptance boxes ticked only
where genuinely proven. While investigating the `-race`/CI success
criterion I found a real, pre-existing bug in `internal/tools` (unrelated to
this plan, outside its file ownership, not fixed here) that makes the last
actual CI run red - documented honestly in `docs/verification.md` rather
than glossed over.

## Files modified

| Path | Change |
|---|---|
| `internal/config/paths.go` | `ConfigPath`'s default branch now delegates to new `defaultConfigPath(home)`: `config.yaml` wins if it exists; else `config.yml`; else `config.yaml`. Both-present prints one `os.Stderr` warning naming the ignored `.yml`. New `fileExists` helper (any `os.Stat` error other than not-exist is treated as absent, per the phase's own instruction). Doc comments updated to state this is now the one place `ConfigPath` touches the filesystem. |
| `internal/config/load_test.go` | Added `TestConfigPath_DefaultAliasPrecedence` (4 subtests: yaml-only, yml-only, both, neither) and `TestConfigPath_ExplicitFlagNeverAliasResolved` (2 subtests: explicit `.yml` that exists, explicit path that does not exist on disk) - 6 cases total, all via `testsupport.FakeHome`/`t.TempDir`, none touching the real home directory. Added the `internal/testsupport` import. |
| `internal/cli/root.go` | `--config` flag help text now mentions the `.yml` fallback. |
| `docs/configuration.md` | Intro paragraph: documents the alias and its precedence. `storage` section: added a "Downgrade note" paragraph for R15 (see below). |
| `docs/architecture.md` | Rewrote the `store/` bullet in the package-boundaries block to describe the real post-refactor shape (generic SQL impl, tokenized `migrations/`, `dialect.go`, `factory.go`, `sqlite/` registering itself via blank import). Added `testsupport/` to the layout block and to the `docs/` line (now lists `verification.md`). Added a new "Schema versioning" section explaining why `PRAGMA user_version` is kept in sync with the ledger even though nothing reads it back - the downgrade-guard rule a future migration author must not break. |
| `README.md` | Added a short "Upgrading, then wanting to go back?" note in the Install section, pointing at the R15 downgrade caveat and `docs/configuration.md`. |
| `plans/260731-2219-mtclaw-core-system/plan.md` | Ticked the three previously-unchecked acceptance boxes (`prompt` tool-using turn, gateway DM round trip, cron delivery) each with an inline citation of the phase-1 test that closes it and an explicit "against a fake upstream" caveat plus a pointer to what remains manual. Rewrote "Remaining manual verification" to point at `docs/verification.md` instead of repeating a now-partially-stale list. |
| `plans/260808-1921-portable-store-and-verification/plan.md` | `status: completed`; all 4 phase rows -> Completed; every Acceptance Criteria box re-verified by hand this session and ticked with an inline test/command citation, **except** `go test -race ./... green on Windows and on Linux CI`, left unticked with the reason (see below). |
| `plans/260808-1921-portable-store-and-verification/phase-0{1,2,3,4}-*.md` | Inline `**Status:**` line updated `Pending` -> `Completed` on all four (phases 1-3 had shipped but never updated their own header line; phase 4's own Success Criteria list ticked the same way as the plan-level list, same one exception left unticked). |

## Files created

| Path | Purpose |
|---|---|
| `docs/verification.md` | Section A ("verified automatically"): every closed criterion mapped to a real, named test - the v1 plan's four criteria, the transport spike, the store portability seam (migration ledger, dialect, factory), the `storage.dsn`/`path`/`.yml`-alias config surface - plus an explicit, evidence-based note that CI's `-race` matrix is correctly configured but was not green on its last actual run. Section B ("requires live credentials"): the 6 items the phase spec asks for at minimum, each with a runnable command and an expected-output line, and a one-line reason it cannot be faked. |

## Latent bug found (each prior phase found one; this is phase 4's)

While verifying the "`go test -race ./...` green on Windows and on Linux
CI" criterion, I checked the repository's actual GitHub Actions history
(`gh run list`, `gh run view`) rather than assume the workflow file's mere
existence proves it green. **The last real CI run on `main`
(`30700212428`, the push that merged the v1 plan, commit `8e59ae2` - the
exact commit these four phases are uncommitted on top of) failed on all
three legs:**

1. **`internal/tools.TestExec_TimeoutKillsWholeProcessTree` panics with a
   nil-pointer dereference under `-race`, on both `ubuntu-latest` and
   `macos-latest`.** The stack trace shows `execTool.run` taking the
   `VerdictAsk` branch (`exec.go:137`) into `execTool.ask` (`exec.go:161`),
   which calls `e.approver.Ask(...)` on a nil `approver` - but the test
   passes `Allow: []string{".*"}`, which should route every command to
   `VerdictRun`, never `VerdictAsk`, and the test does pass reliably without
   `-race` (confirmed locally, 5x `-count=1`, and by the fact that
   `go test ./...` in this session shows only the pre-existing symlink
   failures, not this one). Something about `-race`'s altered goroutine
   scheduling causes the policy to resolve the wrong verdict for this
   specific test. I did not root-cause further or fix it: `internal/tools`
   is entirely outside every phase of this plan's file ownership (confirmed
   via `git status --porcelain`: nothing in `internal/tools` is touched by
   my changes), and the bug predates this plan (it is on the commit this
   plan started from, before phase 1 began).
2. **The `windows-latest` leg fails its own `gofmt check (Windows)` step**
   before `-race` ever runs, flagging essentially every `.go` file in the
   repository (107 of them) as unformatted. I reproduced the identical
   symptom locally: `gofmt -l .` on this machine also flags all 107 files,
   and `git config --get core.autocrlf` reports `true` here. Stripping `\r`
   from a copy of any flagged file (e.g. `internal/config/paths.go`) and
   re-running `gofmt -l` against the LF-only copy reports **zero** issues -
   proving the content is correctly formatted and the "unformatted" verdict
   is purely a CRLF-checkout artifact, not a real `gofmt` violation. This
   confirms the same root cause on both this machine and the CI runner.

Both are pre-existing, both are unrelated to this plan's four phases (config
seam, store seam, e2e harness, this phase's `.yml` alias and docs), and
neither is fixed here - fixing #1 means editing `internal/tools`, outside
this phase's ownership; fixing #2 means a repo-wide `.gitattributes`/
`core.autocrlf` change that would touch every tracked file, which is a
decision for the user, not something to smuggle into a docs-and-config-alias
phase. Both are documented in `docs/verification.md`'s "A note on
`go test -race` and CI" section and reflected in the unticked acceptance box
below, rather than silently worked around or hidden.

## Success criteria (phase file) - verified status

- [x] `~/.mtclaw/config.yml` loads when `config.yaml` is absent; `mtclaw config path` reports it with source `default`
- [x] With both present, `.yaml` wins and one stderr warning names the ignored `.yml`
- [x] With neither present, the not-found error names `config.yaml`
- [x] `--config foo.yml` and `MTCLAW_CONFIG=foo.yml` load `foo.yml`; neither is extension-rewritten
- [x] Six `ConfigPath` test cases pass and touch no real home directory
- [x] `docs/architecture.md` describes the post-refactor `store/` layout and contains the schema-versioning rule about `PRAGMA user_version`
- [x] `docs/verification.md` exists; every Section A row names a real test that exists; every Section B entry has a command and a one-line reason it cannot be faked
- [x] `plans/260731-2219-mtclaw-core-system/plan.md` boxes are ticked only where a named phase 1 test asserts them
- [x] `rg -n "user_version|storage\.path|sqlite\.Open" docs\ README.md` returns only intentional, still-true mentions
- [ ] `go test -race ./...` green - **not achievable and not claimed.** Cannot run locally (`CGO_ENABLED=0`, no C compiler); the real CI run that does exercise it is currently red for the two pre-existing, out-of-scope reasons above. `CGO_ENABLED=0 go build ./...` is green.

## Real command output

### Six new `ConfigPath` tests

```
$ go test ./internal/config/... -run 'TestConfigPath' -v
=== RUN   TestConfigPath_DefaultAliasPrecedence
=== RUN   TestConfigPath_DefaultAliasPrecedence/yaml_only
=== RUN   TestConfigPath_DefaultAliasPrecedence/yml_only
=== RUN   TestConfigPath_DefaultAliasPrecedence/both_present:_yaml_wins,_one_warning_naming_the_ignored_yml
=== RUN   TestConfigPath_DefaultAliasPrecedence/neither_present:_yaml_is_still_the_reported_name
--- PASS: TestConfigPath_DefaultAliasPrecedence (0.01s)
    --- PASS: TestConfigPath_DefaultAliasPrecedence/yaml_only (0.00s)
    --- PASS: TestConfigPath_DefaultAliasPrecedence/yml_only (0.00s)
    --- PASS: TestConfigPath_DefaultAliasPrecedence/both_present:_yaml_wins,_one_warning_naming_the_ignored_yml (0.00s)
    --- PASS: TestConfigPath_DefaultAliasPrecedence/neither_present:_yaml_is_still_the_reported_name (0.00s)
=== RUN   TestConfigPath_ExplicitFlagNeverAliasResolved
=== RUN   TestConfigPath_ExplicitFlagNeverAliasResolved/explicit_.yml_path_is_returned_verbatim
=== RUN   TestConfigPath_ExplicitFlagNeverAliasResolved/explicit_path_with_no_matching_file_on_disk_is_still_returned_verbatim
--- PASS: TestConfigPath_ExplicitFlagNeverAliasResolved (0.00s)
    --- PASS: TestConfigPath_ExplicitFlagNeverAliasResolved/explicit_.yml_path_is_returned_verbatim (0.00s)
    --- PASS: TestConfigPath_ExplicitFlagNeverAliasResolved/explicit_path_with_no_matching_file_on_disk_is_still_returned_verbatim (0.00s)
PASS
ok  	github.com/tiennm99/MTClaw/internal/config	0.535s
```

### Full config suite

```
$ go test ./internal/config/... -v
... (all TestLoad_*, TestValidate_*, TestMarshalRedacted_*, TestConfigFieldsAreDocumented* green, in addition to the two above) ...
PASS
ok  	github.com/tiennm99/MTClaw/internal/config	(cached)
```

### E2E regression net (phases 1-3, re-run untouched by this phase)

```
$ go test -run TestE2E ./internal/gateway/ ./internal/cli/ -v -count=1
--- PASS: TestE2E_DMRoundTrip_ToolCallFedBackAndChunkAwareReply (0.28s)
--- PASS: TestE2E_ChunkedReply_SplitsAcrossMultipleSendMessageCalls (0.52s)
--- PASS: TestE2E_ApprovalApprove_RunsCommandAndAudits (0.78s)
--- PASS: TestE2E_ApprovalDeny_RefusalReachesModelAndAudits (0.28s)
--- PASS: TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun (0.12s)
--- PASS: TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled (0.03s)
ok  	github.com/tiennm99/MTClaw/internal/gateway	3.846s
--- PASS: TestE2E_PromptCompletesToolUsingTurn (0.09s)
ok  	github.com/tiennm99/MTClaw/internal/cli	1.950s
```

Every one of these prints the phase-3 `storage.path` deprecation warning
exactly once (their config helpers build via `config.Default()` + field
mutation, which goes through the path-only shape) - confirming this phase's
`config.yml` alias work did not disturb that fold-before-validate ordering.

### Whole suite

```
$ go test ./... -count=1
ok  	github.com/tiennm99/MTClaw/internal/agent
ok  	github.com/tiennm99/MTClaw/internal/channel/telegram
ok  	github.com/tiennm99/MTClaw/internal/cli
ok  	github.com/tiennm99/MTClaw/internal/config
ok  	github.com/tiennm99/MTClaw/internal/cron
ok  	github.com/tiennm99/MTClaw/internal/gateway
ok  	github.com/tiennm99/MTClaw/internal/provider/mock
ok  	github.com/tiennm99/MTClaw/internal/provider/openai
ok  	github.com/tiennm99/MTClaw/internal/store
ok  	github.com/tiennm99/MTClaw/internal/store/sqlite
ok  	github.com/tiennm99/MTClaw/internal/testsupport/fakeapi
--- FAIL: TestListDir_NeverFollowsSymlinks       (pre-existing, environmental)
--- FAIL: TestResolve_SymlinkEscapingRootRejected (pre-existing, environmental)
--- FAIL: TestResolve_SymlinkInsideRootAccepted   (pre-existing, environmental)
FAIL	github.com/tiennm99/MTClaw/internal/tools
```

Identical to the bar stated in the brief: no NEW failures, same 3
pre-existing symlink-privilege failures (Windows account lacks
`SeCreateSymbolicLinkPrivilege`), unrelated to this phase, `internal/tools`
untouched by any change in this session.

### Build and dependency check

```
$ CGO_ENABLED=0 go build ./...
BUILD_OK

$ git diff go.mod go.sum
(empty)
```

### Mechanical docs sweep

```
$ rg -n "user_version|storage\.path|sqlite\.Open" docs\ README.md
docs/architecture.md: 5 matches, all in the new "Schema versioning" section (user_version)
docs/configuration.md: 4 matches, all in the storage section (storage.path deprecation prose + table row)
docs/verification.md: 4 matches, all in table rows citing storage.path tests/config keys
```

No `sqlite.Open` reference anywhere in `docs/` or `README.md` - the
implementation detail is fully internal to `internal/store` now.

### Manual by-hand verification (real binary, scratch `$USERPROFILE`)

```
$ mtclaw config path                       # config.yml only
...\.mtclaw\config.yml (source: default)
$ mtclaw config validate
OK: ...\.mtclaw\config.yml

$ cp config.yml config.yaml                # both present now
$ mtclaw config path
warning: both ...\.mtclaw\config.yaml and ...\.mtclaw\config.yml exist; using ...\.mtclaw\config.yaml (ignoring ...\.mtclaw\config.yml)
...\.mtclaw\config.yaml (source: default)
$ mtclaw config validate                    # exactly one warning line, then OK
warning: both ...config.yaml and ...config.yml exist; using ...config.yaml (ignoring ...config.yml)
OK: ...\.mtclaw\config.yaml

$ rm config.yaml config.yml                 # neither present
$ mtclaw config path
...\.mtclaw\config.yaml (source: default)

$ mtclaw --config explicit.yml config path
...\explicit.yml (source: flag)
$ MTCLAW_CONFIG=explicit.yml mtclaw config path
...\explicit.yml (source: env:MTCLAW_CONFIG)

$ mtclaw --config both.yaml config validate      # storage.dsn + storage.path both set
Error: storage.dsn: set either storage.dsn or the deprecated storage.path, not both
$ mtclaw --config pg.yaml config validate        # storage.driver: postgres
Error: storage.driver: must be one of: sqlite, got "postgres"
$ mtclaw --config ok.yaml doctor | grep -i "db\|dsn"
OK      DB opens and migrates          <dsn path> opens and is at the current schema version
```

All scratch directories/binaries cleaned up after use; no real
`~/.mtclaw` touched.

## Design decisions / ambiguities resolved

1. **`root.go:89` citation was stale by the time I got here** (line numbers
   shifted from phases 1-3's own edits to that file) - the real flag
   registration line is at `root.go:97` in the current tree. Edited the
   actual line rather than chasing the stale number, per the plan's own
   precedent (finding A11) of treating a line-number citation as
   informational, not load-bearing.
2. **Where to put the six `ConfigPath` test cases.** The phase file already
   corrected its own earlier mistake (a nonexistent `paths_test.go`) to
   `load_test.go` (finding A11 in plan.md) - followed that, added the
   `internal/testsupport` import there (no cycle: `testsupport` imports no
   `internal/` package).
3. **Where to put the R15 downgrade release note.** The phase spec says
   "document this as an upgrade/downgrade note where a user will actually
   find it" without naming a file. No `CHANGELOG.md`/`docs/project-changelog.md`
   exists in this repo (checked). Chose `docs/configuration.md`'s `storage`
   section (the most relevant existing doc, in my ownership) plus a short
   pointer in `README.md`'s Install section (the first thing a user reads
   before ever running `onboard`) - not `.github/workflows/release.yml`'s
   release-notes body, which is not in this phase's file ownership and is
   also not something a user reads before hitting the problem.
4. **Section A scope in `docs/verification.md`.** Kept it to what phases
   1-4 of *this* plan actually closed (the v1 plan's four criteria, the
   store seam, the config surface, the transport spike) rather than
   re-deriving all 21 rows of the v1 security checklist that phase 9 of the
   *other* plan already closed and documented in its own report - citing
   that checklist wholesale here would be scope creep with no new
   assertion behind it (YAGNI). The CI `-race` note is the one addition
   beyond the phase spec's literal ask, included because omitting it after
   discovering it would have made Section A's claim about CI coverage
   false by omission.
5. **Whether to update phase 1-3's own `**Status:**` header lines and the
   inline `Success criteria` list in this phase's own file.** Not explicitly
   in this phase's Files table, but the v1 plan's phase files all carry
   `status: completed` in frontmatter once done, and leaving three stale
   "Pending" headers next to a plan-level table that says Completed would
   be exactly the kind of inconsistency `docs/*` accuracy review is for.
   Low-risk, one-line, planning-artifact-only edits; done for all four
   phase files plus this phase's own Success Criteria list, ticked/unticked
   with the same rule as the plan-level list.

## Issues encountered

- No file-ownership conflicts: `git status --porcelain` before and after
  this session's edits confirms only `docs/*`, `README.md`,
  `internal/config/paths.go`, `internal/config/load_test.go`,
  `internal/cli/root.go`, and the two plan files (plus phase-0X headers)
  changed. Nothing in `internal/tools`, `internal/store`, or any
  phase-2/3-owned file was touched.
- The latent CI/`-race`/gofmt-CRLF finding above is the substantive issue
  of this phase - reported prominently rather than worked around, per the
  reporting rules.

## Unresolved questions

None outstanding. Two items in the phase file needed a judgment call
(the stale `root.go:89` citation, and where to put the R15 note); both are
recorded under "Design decisions" above with the reasoning, not silently
decided.

---

Status: DONE_WITH_CONCERNS
Summary: `.yml` alias, its six tests, and the docs/verification refresh are all genuinely done and re-verified by hand with a real binary; `docs/verification.md` and the v1/phase-4 plan-checkbox updates are complete and honest. The sole gap is `go test -race ./... green`, which was never achievable in this environment and, on investigation, is also not currently true in CI - for two pre-existing bugs entirely outside this plan's scope, reported here rather than hidden.
Concerns: (1) `internal/tools.TestExec_TimeoutKillsWholeProcessTree` nil-derefs under `-race` on Linux/macOS - needs a fix in a package no phase of this plan owns. (2) The Windows CI leg's `gofmt check (Windows)` step fails on a CRLF/`core.autocrlf=true` checkout artifact (reproduced locally), unrelated to any real formatting issue, blocking `-race` from ever running on that leg. Neither blocks this phase's own deliverables; both are documented in `docs/verification.md` and left for a maintainer to pick up.

## Final list of acceptance-criteria boxes left UNTICKED, and why

- **This plan's `plan.md`, "Whole plan" section: `go test -race ./... green on Windows and on Linux CI`.** Left unticked. Cannot run `-race` locally (no C compiler, `CGO_ENABLED=0`) in any of the four phases' implementation sessions; the actual CI run that would prove it is currently red, for two pre-existing causes unrelated to this plan (see "Latent bug found" above). Ticking this would be false.
- **This phase's own file, `phase-04-polish-docs-and-manual-checklist.md`, Success criteria: `go test -race ./...` green.** Same reason, left unticked there too.

Everything else in both files' checklists was re-verified by hand this
session (grep output, real test runs, or a real binary) before being
ticked - none was carried over from a prior report without independent
confirmation.

## Anything a human must still do by hand

- Run the 6 items in `docs/verification.md` Section B once real credentials
  are available (a real OpenAI turn, a real Telegram DM + `/whoami`/`/new`/
  `/status`, a real cron fire on a real minute boundary, `onboard` on a
  clean machine, a token-leak log grep with a real credential present, a
  tagged release).
- Investigate and fix the `internal/tools.TestExec_TimeoutKillsWholeProcessTree`
  `-race` nil-pointer panic (Linux/macOS) - a real bug, not a flake, and not
  fixed by this phase.
- Decide whether to normalize line endings (`.gitattributes` + a one-time
  re-checkout) to fix the Windows CI `gofmt` step, or accept CRLF and change
  the CI check instead - either is a repo-wide decision outside this
  phase's file ownership.
