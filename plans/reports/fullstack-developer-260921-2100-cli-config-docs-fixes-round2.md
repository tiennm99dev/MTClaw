# Round-2 review fixes: CLI / config / logging / docs / CI

Slice: `main.go`, `internal/cli/**`, `internal/config/**`, `internal/logging/**`,
`internal/version/**`, `README.md`, `docs/configuration.md`, `docs/architecture.md`,
`Makefile`, `.github/workflows/*`. Source: `plans/reports/code-reviewer-260921-1649-cli-config-docs-rereview.md`.

## Files Modified

- `internal/cli/onboard_prompts.go` - `readLine` EOF handling (C1a)
- `internal/cli/root.go` - `newRootContext` second-signal escape hatch (C1b), `isReadOnlyCommand` + logger fix (M1), friendly no-DB error (L6)
- `internal/cli/cron_cmd.go` - `newCronRunApprover` seam (H2), `recordManualRun` uses `context.WithoutCancel` + comment (M2)
- `internal/cli/prompt_cmd.go` - `newPromptApprover` seam (H2)
- `internal/cli/doctor_checks.go` - doctor row rename + comment fixes (H1), state-dir comment fix (L3)
- `internal/config/load.go` - `ensureDirCreatable` drops permission-bit heuristic (M3)
- `internal/config/validate.go` - `storage.path` empty rejected (M4)
- `internal/logging/logger.go` - log dir `0700`, best-effort chmod existing log file to `0600` (L1)
- `.github/workflows/ci.yml` - `go mod tidy -diff` on ubuntu leg only (L7)
- `docs/configuration.md` - `agent.workspace`/`agent.system_prompt_files` rows (M5/L4), `tools.exec.cwd` row (H1)
- `docs/architecture.md` - `Load`/`Validate` purity claim corrected (L5)
- `README.md` - Go version requirement corrected (M6)
- `internal/config/load_test.go`, `internal/config/validate_test.go` - stale `tools.Resolve` comments fixed, `TestValidate_ToolBoundsSkippedWhenDisabled` made discriminating (task 10), `TestValidate_StorageParentDirNotWritable` removed (M3, no longer holds), `TestValidate_StoragePathEmpty` added (M4)
- `internal/cli/onboard_test.go` - `TestRunOnboard_ClosedStdinAbortsWithoutWritingConfig` (C1c)
- `internal/cli/root_test.go` - `TestPrepare_ReadOnlyCommandsDoNotCreateLogFile` (M1)
- `internal/logging/logger_test.go` - log-dir-mode and existing-file-chmod tests (L1)

New files:
- `internal/cli/onboard_prompts_test.go` - `readLine` unit tests (C1c)
- `internal/cli/root_unix_test.go` (`//go:build unix`) - `TestNewRootContext_SIGINTCancelsContext` (C1d)
- `internal/cli/prompt_cmd_test.go` - `TestNewPromptApprover_WiresTerminalApprover` (H2)
- `internal/cli/cron_cmd_test.go` - `TestNewCronRunApprover_WiresDenyAllApprover` (H2)

## Tasks Completed

1. **C1** - `readLine` now errors on EOF-with-nothing-pending (still accepts a final unterminated line as real input); `newRootContext` installs the second-signal hard-kill (`go func(){ <-ctx.Done(); stop() }()`); EOF test added at both the `readLine` unit level and the `runOnboard` integration level (asserts no config file written); unix-only SIGINT test added for the root context. Verified live: `mtclaw onboard < /dev/null` now exits 1 immediately (no timeout/kill needed).
2. **H1** - Dropped the false "enforced at call time by `tools.Resolve`" claim from `docs/configuration.md` and `checkExecCWD`'s comment; doctor row renamed `"exec.cwd inside a root"` -> `"exec.cwd exists"`. No test referenced the old row name string, so no test change was required there (`TestCheckExecCWD` calls the function directly).
3. **H2** - Extracted `newPromptApprover` (returns `*tools.TerminalApprover`) and `newCronRunApprover` (returns `tools.DenyAllApprover`) as the seam; added tests pinning both wirings without needing a live OpenAI client.
4. **M1** - `isReadOnlyCommand` (config show/validate, sessions list/show/rm, approvals list, cron list) makes `prepare` build the logger with `Log.File` cleared for those commands, while `s.cfg.Log.File` itself (used by `config show`) stays untouched. Test: `config validate` against a config whose `log.file` dir doesn't exist creates nothing.
5. **M2** - `recordManualRun` now calls `runs.Append(context.WithoutCancel(ctx), run)`; comment updated to explain why (mirrors the ephemeral-session cleanup already using `context.Background()`).
6. **M3** - `ensureDirCreatable` no longer checks the owner-write bit; only requires the nearest existing ancestor to be a directory. `TestValidate_StorageParentDirNotWritable` removed (it asserted the reversed behavior); `TestValidate_StorageParentDirCreatable`/`NotCreatable` still cover the remaining rule.
7. **M4** - `validateStorage` rejects an empty `storage.path` before the parent-dir check; `TestValidate_StoragePathEmpty` added.
8. **Docs** - `agent.workspace` row now says onboard-or-first-`write_file` creates it; `agent.system_prompt_files` row says AGENTS.md is written next to `--config`; README says "Go 1.25.7 or newer" with the `GOTOOLCHAIN` nuance; doctor's state-dir comment and `docs/configuration.md`'s AGENTS.md row both corrected; `docs/architecture.md`'s "pure function" claim replaced with an accurate one (secret-file reads, storage-dir stat).
9. **L1/L6/L7** - log dir now `0700`; existing log file best-effort `chmod 0600` before reopen (tests added); `openStore(ctx, true)` returns `"no database yet at %s; run `mtclaw gateway` or `mtclaw prompt` first"` when the file doesn't exist yet (verified via the existing `TestOpenStore_...` test, which already tolerated a read-only-open failure and still passes); `go mod tidy -diff` CI step gated to `ubuntu-latest`.
10. `TestValidate_ToolBoundsSkippedWhenDisabled` split into three subtests (filesystem/web_fetch/exec), each asserting both the disabled-passes half and the enabled-fails half.

## Tests Status

- gofmt: clean (`gofmt -l .`)
- `go vet ./...`: clean
- `go build ./...`: clean
- `go test -race -count=2 ./internal/cli/... ./internal/config/... ./internal/logging/...`: all pass
- `go mod tidy -diff`: clean
- Live smoke: `HOME=<tmp> timeout -s KILL 20 mtclaw onboard < /dev/null` exits 1 immediately, no hang, no output growth

## Issues Encountered

- Mid-session, a concurrent agent's in-progress edit to `internal/tools/approver.go` caused one transient `go build ./...` failure (`undefined: redactBase64Like`) outside my ownership; it resolved itself moments later and all subsequent builds/tests were clean. Not a regression from this slice.
- `docs/security.md`, `internal/tools/*`, `internal/gateway/*` etc. appear modified/untracked in `git status` from the pre-existing uncommitted pass and other agents' concurrent work; none of it was touched by me.
- The review's C1 fix suggestion #3 (bound the onboard model-prompt loop to N attempts) was not applied: it's now unreachable dead weight, since EOF is caught by `readLine` before the loop can spin, and the task list only asked for readLine + the signal escape hatch.

## Next Steps

- Gateway-owning agent still needs to remove `internal/gateway`'s nested `signal.NotifyContext` (per the review's C1 note) for the second-signal hard-kill to also cover the `gateway` leg; that is out of this slice's ownership and was left untouched.

Status: DONE
Summary: All ten tasks (C1, H1, H2, M1-M4, docs, L1/L6/L7, test-10) implemented within file-ownership boundaries; build/vet/gofmt/race-tests/mod-tidy all green, and the onboard-hang regression was verified fixed live.
Concerns/Blockers: None from this slice; the cross-slice gateway `notifyContext` removal (owned elsewhere) is still needed for the second-signal kill to reach the `gateway` command specifically.
