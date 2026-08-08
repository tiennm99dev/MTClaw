# Phase 3 Implementation Report: Store Factory and Storage Config

Plan: `plans/260808-1921-portable-store-and-verification/`
Phase file: `phase-03-store-factory-and-storage-config.md`
Date: 2026-08-08

## Status: DONE

Every phase-file success criterion and every plan-level acceptance criterion
for phases 2-3's store seam is met, backed by a test that would fail if the
behavior broke. One design point in the phase file needed resolving rather
than following literally (see "Deviation: the dsn/path ambiguity" below) -
the fix is smaller than the phase file's own defaults.go line, and the
reasoning is recorded in code comments (`internal/config/load.go`'s
`expandStorage`) as well as here.

## Files created

- `internal/store/factory.go` (98 lines) - `Opener`, `Register`, `Open`,
  `ErrUnknownDriver`, the registry.
- `internal/store/factory_test.go` (72 lines) - 4 tests: sqlite via
  `store.Open`, sqlite via the `path` alias, unregistered driver naming the
  supported set, `Register` panicking on a duplicate name.

## Files modified

- `internal/store/sqlite/dialect.go` - added `init()` registering `"sqlite"`.
- `internal/config/types.go` - `StorageConfig{Driver, DSN, Path}` +
  `EffectiveDSN()`.
- `internal/config/defaults.go` - `defaultStorageDSN` const;
  `Default().Storage = {Driver: "sqlite", DSN: defaultStorageDSN}`.
- `internal/config/load.go` - new `expandStorage` (replaces the single old
  `expand(&cfg.Storage.Path)` line) + `warnStoragePathDeprecated`.
- `internal/config/validate.go` - `validateStorage` rewritten: driver
  whitelist, both-set error, sqlite-only parent-dir check via
  `EffectiveDSN()`.
- `internal/config/validate_test.go` - 4 existing sites switched to `DSN`;
  2 new tests (`TestValidate_StorageBothDSNAndPathSet`,
  `TestValidate_StorageUnsupportedDriver`).
- `internal/config/load_test.go` - `captureStderr` helper + 4 new
  Load-pipeline tests (dsn-only, path-only+warns-once+normalizes,
  both-set-fails, unsupported-driver-fails).
- `internal/cli/root.go`, `internal/cli/doctor_checks.go`,
  `internal/gateway/gateway.go` - the three production wiring sites: dropped
  the `sqlite` import, added the blank import, call `store.Open` /
  `store.Open` via `cfg.Storage`.
- `internal/cli/gateway_cmd.go` - `"db_path"` log attr → `"storage_dsn"`,
  value `EffectiveDSN()`.
- `internal/cli/doctor_test.go` - `testConfig` uses `DSN`; `TestCheckDatabase`
  pokes the ledger via a bare `database/sql.Open("sqlite", ...)` instead of
  `sqlite.Open`; `minimalYAML` still emits `path:` (proves the alias) with
  its value now sourced from `EffectiveDSN()`; the other inline YAML doc in
  `TestRunDoctor_FullRunOnLoadableButBrokenConfig_ExitsNonZero` switched to
  `dsn:`.
- `internal/agent/loop_test.go`, `internal/cron/scheduler_test.go`,
  `internal/gateway/gateway_test_helpers_test.go`,
  `internal/tools/exec_test.go`, `internal/tools/registry_test.go` - the six
  test-helper `newTestStore`-shaped functions rewired to
  `store.Open(ctx, config.StorageConfig{Driver:"sqlite", DSN: path}, false)`,
  with a blank import added to whichever file in each package didn't
  already carry one from production code.
- `docs/configuration.md` - `storage` section rewritten: intro paragraph on
  the alias/conflict rule, three rows (`driver`, `dsn`, `path` marked
  deprecated).

### One file beyond the phase file's own list: `internal/gateway/e2e_test.go`

Not in the phase file's Files table, but its `waitForPendingApproval` helper
called `sqlite.Open` directly to inspect the approvals table - a real,
non-blank import of `internal/store/sqlite` from outside `internal/store/`.
Requirement 4 ("No package outside internal/store imports
internal/store/sqlite except as a blank import") and the plan-level grep
acceptance criterion apply to the whole tree, not just the three named
production sites, so this would have failed both if left alone. Fixed by
switching to a bare `database/sql.Open("sqlite", "file:...?mode=ro")` - the
driver name `"sqlite"` is already registered process-wide once
`gateway.go`'s own blank import loads `modernc.org/sqlite` transitively, so
no import of the sqlite backend package is needed here at all. Added an
explicit `db.PingContext` to preserve the original's fail-fast behavior
(`database/sql.Open` itself never dials). Behavior is identical - same file,
same read-only semantics, same query - so none of the 7 e2e assertions
changed; all 7 still pass. Not a "fix your change, don't edit the test"
violation: nothing about what the test asserts changed, only how it opens a
raw handle for its own inspection.

## Deviation: the dsn/path ambiguity

The phase file's `defaults.go` line says `DSN: "~/.mtclaw/mtclaw.db"`
literally. Followed exactly, this breaks the compatibility contract it
exists to serve:

`Load` does `cfg := Default(); dec.Decode(cfg)` - decoding *merges* onto the
already-populated struct (confirmed by this file's own doc comment on
`Default()`, and by existing tests where omitted fields keep their default
after `Load`). If `Default()` prefills `DSN` to a concrete non-empty string,
then a real user config that sets only `storage.path` ends up, after
decode, with **both** fields non-empty: `DSN` still holds the untouched
default, `Path` holds the user's value. A naive "both non-empty → error"
check (which is what `validateStorage`'s simple, unambiguous form actually
is) would then reject every pre-existing path-only config - exactly the
population this alias exists to keep working.

This is not a hypothetical: `internal/gateway/e2e_test.go`'s
`buildE2EConfig` and `internal/cli/prompt_e2e_test.go`'s
`writeE2EConfigFile` (both phase-1 files, both in the "must keep working"
list in the phase file's own Context section) build a `Config` via
`config.Default()`, set only `.Storage.Path`, `yaml.Marshal` it, and
`Load()` it back - reproducing this exact shape. Before the fix below, all 7
e2e tests failed with `storage.dsn: set either storage.dsn or the
deprecated storage.path, not both`.

**Resolution** (`internal/config/load.go`'s `expandStorage`, `defaults.go`'s
`defaultStorageDSN` comment): the fold-and-warn step runs in
`expandConfigPaths` (before `Validate`), and it treats `DSN` as "not really
set by the user" when it still holds the literal, unexpanded
`defaultStorageDSN` string. That is enough to disambiguate all four cells of
the compatibility table without changing `validateStorage` itself, which
stays the simple, heuristic-free check the phase file specifies (verified by
`TestValidate_StorageBothDSNAndPathSet`, which hand-builds a `Config` with
two explicit non-default values and confirms the direct-`Validate()` path
needs no heuristic at all).

The one known imprecision (documented in the `expandStorage` doc comment,
and already called out in `plan.md`'s risk table as an accepted trade-off):
a user who sets `storage.dsn` to *exactly* its own default value while also
setting `storage.path` is treated as path-only, not as a conflict. Accepted
because the alternative - treating every pre-existing path-only config as a
hard error at upgrade time - is a materially worse outcome for a
vanishingly rarer edge case.

`Default()` itself still sets `DSN: defaultStorageDSN` (matching the phase
file literally) so that `onboard` and any hand-built `Config` that skips
`Load` entirely render a concrete, non-empty DSN rather than an empty
string - confirmed by marshaling `Default()` directly: `storage: {driver:
sqlite, dsn: ~/.mtclaw/mtclaw.db}`, no `path` key.

## Definition-of-done commands, literal output

```
$ grep -rn 'store/sqlite' internal/ --include=*.go | grep -v '^internal/store/'
internal/agent/loop_test.go:22:	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
internal/channel/telegram/approver_test.go:23:// for internal/store/sqlite so these tests never touch a real database or
internal/cli/doctor_checks.go:23:	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
internal/cli/root.go:23:	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
internal/cron/scheduler_test.go:26:	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
internal/gateway/gateway.go:23:	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
internal/tools/exec_test.go:25:	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
```

Every match outside `internal/store/` is a blank import except one:
`internal/channel/telegram/approver_test.go:23`, a **pre-existing comment**
(phase 6, not touched by this phase, not in this phase's file ownership)
mentioning the package name in prose - not an import. Confirmed by `git
blame`-equivalent reasoning: it was already present before this phase
started and is outside phase 3's file ownership, so it was left alone. The
actual invariant (no real, non-blank import of the backend package outside
`internal/store/`) holds.

```
$ grep -rn 'modernc.org/sqlite' internal/ --include=*.go | grep -v '^internal/store/sqlite/'
(no output)
```

Clean - no mentions of the concrete driver package outside its own
directory.

```
$ go test ./internal/store/... ./internal/config/... -v
```
All green (factory_test.go's 4 new tests, sqlite's pre-existing suite
unchanged, config's full suite including 6 new storage tests).

```
$ go test -run TestE2E ./internal/gateway/ ./internal/cli/ -v
```
All 7 e2e tests pass (`TestE2E_DMRoundTrip...`, `TestE2E_ChunkedReply...`,
`TestE2E_ApprovalApprove...`, `TestE2E_ApprovalDeny...`,
`TestE2E_CronDelivery...`, `TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled`,
`TestE2E_PromptCompletesToolUsingTurn`). Each prints the one-line deprecation
warning exactly once (their config-building helpers all go through the
path-only shape described above), confirming the fold-before-validate
ordering is correct end to end, not just in the unit tests that target it
directly.

```
$ go test ./...
```
Every package green except `internal/tools`, which fails with exactly the 3
pre-existing, pre-documented Windows-symlink-privilege failures
(`TestListDir_NeverFollowsSymlinks`, `TestResolve_SymlinkEscapingRootRejected`,
`TestResolve_SymlinkInsideRootAccepted`) - unrelated to storage, unrelated to
this phase, not touched. No new failures anywhere.

```
$ CGO_ENABLED=0 go build ./...
```
Clean, no output.

```
$ git diff go.mod go.sum
```
Empty - no module dependency added or removed.

### -race compensation (can't run `-race` locally: CGO_ENABLED=0, no C
compiler, as stated in the task's environment facts)

Ran the timing-sensitive suites 5x with `-count=1`:

```
for i in 1 2 3 4 5; do go test -count=1 ./internal/gateway/... ./internal/cli/... ./internal/cron/...; done
```
5/5 clean (run 3 was slower - 17.3s vs ~2.7s for `cli` - but still passed;
no flake, no failure, across any of the 5 runs).

### Manual behavioral spot checks (built a real binary, ran real commands)

```
$ mtclaw --config testcfg.yaml config validate            # dsn form
OK: ...\testcfg.yaml

$ mtclaw --config testcfg-path.yaml config validate        # path form
warning: storage.path is deprecated; use storage.dsn instead (storage.path keeps working, with no removal planned)
OK: ...\testcfg-path.yaml

$ mtclaw --config testcfg-both.yaml config validate         # both
Error: storage.dsn: set either storage.dsn or the deprecated storage.path, not both

$ mtclaw --config testcfg-pg.yaml config validate            # driver: postgres
Error: storage.driver: must be one of: sqlite, got "postgres"

$ mtclaw --config testcfg-path.yaml config show | grep -A2 storage:
storage:
  driver: sqlite
  dsn: "<absolute path>"
                                                              # no path: key

$ mtclaw --config testcfg.yaml doctor | grep "DB opens"
OK      DB opens and migrates          <absolute dsn path> opens and is at the current schema version

$ mtclaw --config testcfg-path.yaml doctor  # then:
$ mtclaw --config testcfg-path.yaml sessions list
warning: storage.path is deprecated; use storage.dsn instead (storage.path keeps working, with no removal planned)
ID  CHANNEL  CHAT  TITLE  MESSAGES  TOKENS  UPDATED
                                                              # opens fine, same db doctor created
```

All six manual checks in the task's "verify by hand" list confirmed
(`config validate` rejects both-keys and non-sqlite; path-only loads, warns
once, `EffectiveDSN()` equals the path - shown indirectly via `sessions
list` opening the same db `doctor` created; `doctor` names the DSN through
the factory).

One artifact in the manual test's output: the absolute DSN paths shown
above are doubled (a `/c/Users/...` git-bash path interpolated into a
Windows-absolute-path YAML value, then joined again by `ExpandPath` because
`filepath.IsAbs` doesn't recognize the POSIX-style prefix on Windows) - this
is purely how I built the throwaway `.yaml` fixtures from a bash shell, not
a code defect; the *shape* of every check (which key renders, which errors,
which check passes) is what was verified and is correct.

### `internal/config/docs_coverage_test.go`

Passes (part of the `./internal/config/...` run above). `storage.driver`,
`storage.dsn`, and `storage.path` (marked deprecated in prose) all have rows
in `docs/configuration.md`.

## Phase-file success criteria checklist

- [x] `store.Open(ctx, config.StorageConfig{Driver:"sqlite", DSN: tmp}, false)` returns a usable `store.Store` - `TestOpen_SQLiteDriverReturnsUsableStore`
- [x] `Driver:"postgres"` returns an error naming the supported drivers - `TestOpen_UnregisteredDriverNamesSupportedSet` (also covers "postgres" specifically, since it's simply an unregistered name; no second backend exists to name differently)
- [x] `rg -n "store/sqlite" internal/ --glob '*.go'` outside `internal/store/` matches only blank imports - true except one pre-existing, out-of-scope comment (see above)
- [x] `rg -n "modernc.org/sqlite" internal/ --glob '*.go'` matches only `internal/store/sqlite/` - clean, zero matches outside
- [x] A config with only `storage.path` loads, warns once on stderr, and opens the same database file as before - `TestLoad_StoragePathAliasWarnsOnceAndNormalizes` + manual `sessions list` check
- [x] A config with both `storage.path` and `storage.dsn` fails validation naming both keys - `TestValidate_StorageBothDSNAndPathSet`, `TestLoad_StorageBothDSNAndPathSetFails`, manual check
- [x] A config with `storage.driver: postgres` fails validation - `TestValidate_StorageUnsupportedDriver`, `TestLoad_StorageUnsupportedDriverFails`, manual check
- [x] `mtclaw config show` on a `path`-only config renders `dsn` - manual check (`path,omitempty` + normalization)
- [x] `mtclaw onboard` writes a config containing `driver`/`dsn` and no `path` key - verified via `yaml.Marshal(config.Default())` (onboard never touches `Storage`, so this is exactly what it writes)
- [x] `docs/configuration.md` documents all three keys; `go test ./internal/config/...` green
- [x] `mtclaw doctor`'s DB check still tries read-only first and reports the DSN - `checkDatabase` preserves the retry, `TestCheckDatabase` unchanged in behavior, manual check confirms the message
- [x] Phase 1 e2e suites unchanged and green; `go test ./...` green (module-wide, modulo the 3 pre-existing symlink failures); `go.mod`/`go.sum` unchanged

(Task brief said `go test -race ./...`; not runnable locally per the stated environment fact - ran plain `go test ./...` plus 5x `-count=1` on timing-sensitive suites instead, as instructed.)

## R15 (verbatim, as required)

`yaml.DisallowUnknownField` (`internal/config/load.go:58`) means a v1 binary
REJECTS a config containing `driver`/`dsn`. So after a user re-runs
`onboard` with the new binary, "just revert the commit" does not restore a
working setup - the config file blocks rollback even though the data is
safe. This is NOT fixable in-plan (v1 already shipped and cannot learn to
ignore future keys). Put it in your report's rollback section verbatim and
flag that it needs a release note.

This is real and unmitigated in this phase. Confirmed mechanically: `Load`
decodes with `yaml.DisallowUnknownField()`; a config containing `driver:`
or `dsn:` under `storage:` would be rejected outright by any pre-phase-3
binary. Rollback path for anyone who ran the new `onboard`: manually rename
`dsn:` back to `path:` and delete the `driver:` line before reverting the
binary. **Needs a release note** - not something phase 3 or phase 4 can fix
in code, since the old binary already shipped.

## Other latent items found (both prior phases found one; reporting per instructions)

1. **The dsn/path ambiguity above** - reported as a deviation, not a latent
   bug in existing code, but worth flagging since it's the one place this
   phase's own defaults.go instruction, read literally, would have silently
   broken the compatibility contract it exists to implement. No production
   code shipped with the bug; caught before merging by the e2e suite
   actually failing when I first ran it.
2. **No new bugs found in phases 1/2's code.** I did not find anything else
   worth flagging while reading `store.go`, `migrate.go`, `dialect.go`, or
   the CLI/gateway wiring.

## Scope discipline confirmation

- No `.yml` extension alias added (phase 4).
- No `docs/verification.md` written (phase 4).
- No second driver, no speculative `postgres`/`mysql` type or constant.
- No Go module dependency added (`git diff go.mod go.sum` empty).
- `store.Store` interface shape unchanged (`internal/store/store.go` only
  touched by phase 2's move, not by this phase - confirmed via `git status`:
  it shows as modified from phase 2's work, not by any edit I made).
- WAL / busy_timeout / foreign_keys / BEGIN IMMEDIATE / `MaxOpenConns(1)` /
  READONLY_RECOVERY fallback / downgrade guard / `user_version` sync: all
  untouched inside `internal/store/sqlite/dialect.go` - the only change
  there is the added `init()`.

## Confirmation: all 3 production wiring sites have the blank import

- `internal/cli/root.go:23` - `_ "github.com/tiennm99/MTClaw/internal/store/sqlite"`
- `internal/cli/doctor_checks.go:23` - same
- `internal/gateway/gateway.go:23` - same

## What phase 4 needs to know

- `storage.driver`, `storage.dsn`, `storage.path` are fully wired,
  validated, documented, and covered by `docs_coverage_test.go`. Phase 4's
  `.yml` alias work and `docs/verification.md` are untouched by this phase
  and can proceed independently - no shared lines in `docs/configuration.md`
  beyond the `storage` section this phase already finalized.
- `internal/config/types.go`, `internal/config/defaults.go`, and
  `internal/config/validate.go` now have the shapes phase 4 should build on
  top of, not restructure - in particular, `EffectiveDSN()` is the one read
  path; nothing new should read `.Storage.Path` or `.Storage.DSN` directly.
- The `expandStorage`/`defaultStorageDSN` heuristic (see "Deviation" above)
  is load-bearing for the e2e suites specifically because they build configs
  via `config.Default()` + direct field mutation, not via a real YAML
  document a user would hand-write. If phase 4 changes how `Default()`
  populates `Storage`, re-run `go test -run TestE2E ./internal/gateway/
  ./internal/cli/` to catch a regression immediately - it will fail loudly
  with the "both set" error, not silently.
- R15 (downgrade/rollback config incompatibility) still needs a release
  note; not addressed by any phase in this plan.

## Unresolved questions

None. Everything ambiguous in the phase file (the dsn/path defaulting
mechanics) was resolved empirically against the existing e2e test suite
rather than left as an open question.
