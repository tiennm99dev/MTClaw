# Final review: cli / config / logging / version / build / CI / docs fix pass

Date: 2026-09-28. Branch `refactor/260928-full-review`, uncommitted tree vs HEAD `cd7806d`.
Decisions in `plans/260928-1041-third-round-review-fixes/plan.md` treated as binding (not re-litigated).

## Scope

- Files: `internal/cli/**`, `internal/config/**`, `internal/logging/**`, `internal/version/**`, `main.go`,
  `Makefile`, `.github/workflows/{ci,release}.yml`, `README.md`, `docs/*.md`; plan-reference grep over all of `internal/`.
- Checks run: `gofmt -l .` (clean), `go vet ./...` (clean), `go test -race -count=1 ./...` (all pass),
  `go mod tidy -diff` (clean), `GOTOOLCHAIN=go1.25.7 go test ./internal/{cli,config,version}` (pass on the go.mod minimum),
  `actionlint` v1.7.12 on both workflows (clean), PyYAML parse (valid).
- Smoke: binary built into a scratch dir, run against a scratch `HOME` (no gateway started).

## Overall assessment

The fix pass is solid. Config gating by annotation works, the workflows are valid, and the release recipe is
reproducible. There are two Medium issues: stricter `log.*` validation breaks configs that used to load, and
`cron run` misses the `PR_SET_DUMPABLE` mitigation. Everything else is Low. Nothing is Critical or High.

## Critical

None.

## High

None.

## Medium

### M1. Stricter `log.level`/`log.format` validation rejects configs that used to work

`internal/config/validate.go:320-331`, `internal/cli/root.go` `isValidLogLevel`, compared with `internal/logging/logger.go:53-64`.
`logging.parseLevel` trims the value, ignores case, and accepts `warning`. `logging.New` compares
`format` with `strings.EqualFold`. So `log.level: warning`, `WARN`, `Info`, and `log.format: JSON` did not fall back to a
default. They worked exactly as intended. The new `validateLog` accepts only the lowercase literals.
- Verified: the same config with `level: warning` or `level: INFO` now fails `mtclaw config validate` (exit 1). At HEAD
  it loaded and logged at the requested level. After an upgrade, `mtclaw gateway` refuses to start on such a config.
  `--log-level WARN` is also newly rejected.
- The docs row (`docs/configuration.md:143-144`) says an unrecognized value "used to load silently as `info`". That is
  wrong for these spellings, which were recognized. The `logging.New` doc comment (`logger.go:20-22`: "a typo'd
  --log-level flag degrades gracefully instead of blocking startup") is now stale as well.
- Fix: normalize before checking. Apply `strings.ToLower(strings.TrimSpace(...))` in `validateLog` and `isValidLogLevel`,
  and accept `warning` as an alias. Or normalize `cfg.Log.Level`/`Format` in `Load`, as `normalizeExecMode` does. Keep
  rejecting real typos. Update the doc row and the `logging.New` comment.

### M2. `mtclaw cron run` never calls `tools.DisableEnvironRead`

`internal/cli/cron_cmd.go:110-150`. Only `gateway_cmd.go:41` and `prompt_cmd.go:37` call it.
`cron run` builds the same loop and registry through `state.newLoop`. `DenyAllApprover` only covers `VerdictAsk`.
Allow-list matches (`policy.go:166`, `VerdictRun`) and auto-mode classifier passes (`policy.go:217`) still execute.
- Failure: a manual `cron run` whose job triggers an allow-listed or auto-allowed command runs a child as the same uid,
  and the parent is still dumpable. That child can read `OPENAI_API_KEY` or `TELEGRAM_BOT_TOKEN` from
  `/proc/<mtclaw-pid>/environ`. This is the exact vector the plan's secrets decision set out to close.
- Fix: move the call into `state.newLoop` (root.go), which both `prompt` and `cron run` go through, and drop the
  separate call in `prompt_cmd.go`. Then reword `docs/security.md:206-207` ("`mtclaw gateway` and `mtclaw prompt` both
  call...") to cover every exec-capable entry point.

## Low

- **L1. `cron run` prints model output unescaped.** `cron_cmd.go:170` prints `result.Text` raw. `prompt_cmd.go:63` passes
  the same kind of text (possibly shaped by web_fetch) through `sanitizeForTerminal`. Fix: use `sanitizeForTerminal`
  there too. Do not sanitize the `--deliver` path (line 173), which goes to Telegram.
- **L2. Four comments still cite the plan, a phase, or a finding code** (plan acceptance says "no plan IDs, phase
  numbers, or finding codes in code, test names, or comments"):
  - `internal/cli/sessions_cmd_test.go:107`: "mitigates B23"
  - `internal/channel/telegram/gating_test.go:279`: "Phase 1 validation is responsible..."
  - `internal/cron/scheduler_test.go:89`: "the phase plan got wrong once already"
  - `internal/provider/openai/chat_test.go:77`: "called out by the phase spec"
  - No test *names* carry codes. `RoundTrip` hits are ordinary words.
- **L3. The version package over-claims VCS stamping, and one of its tests can never run.**
  - `internal/version/version.go:5-6` says `go run` and `go install .../MTClaw@vX` fill in Commit/Date. They do not.
    `go run . version` prints `commit none, built unknown` (verified). Module-cache installs have no `vcs.*` settings,
    and README.md:54-58 already says so correctly.
  - `TestString_ResolvesFromBuildInfoWhenLdflagsLeftDefaults` always SKIPs, because `go test` binaries are never
    VCS-stamped. It is a phantom test. Either delete it or make it build a binary.
  - `String()` still prints "built <Date>", but Date is now the commit time. "committed" would be accurate.
- **L4. Release workflow hardening.**
  - `release.yml` interpolates `${{ github.ref_name }}` straight into `run:`. Git allows `$`, `(`, `)` and `;` in tag
    names. Pass it through `env: VERSION: ${{ github.ref_name }}` and use `make release VERSION="$VERSION"`.
  - Separately, `go test ./...` runs with a checkout that persists a `contents: write` token in `.git/config`, so any
    test or dependency code can read it. Set `persist-credentials: false` on the checkout step; the release action gets
    its token as an input.
  - Only someone with tag-push rights can reach either path.
- **L5. The doctor check name "DB opens and migrates" (`doctor_checks.go:37`, also cited in `docs/configuration.md:137`)
  is now wrong.** The check deliberately never migrates an existing database. Rename it to something like
  "DB opens at current schema" and update the doc.
- **L6. Moving the lock has no upgrade note.** A gateway still running the old binary holds `~/.mtclaw/gateway.lock`. A
  newer `cron run`, `doctor`, or a second new gateway checks `storage.path + ".lock"` instead, does not see it, and can
  run against the same database at the same time. The old lock file is also left behind. Add one sentence to the
  README or `docs/configuration.md`: stop the old gateway before starting the upgraded one, and the stale
  `~/.mtclaw/gateway.lock` can be deleted.
- **L7. Echo can still be left off after Ctrl-C at the secret prompt, in a very narrow window.** In
  `onboard_prompts.go` `Secret`, `term.GetState` runs first and `term.ReadPassword` (which turns echo off) runs on a
  goroutine. If the context is already done when `Secret` is entered, or a signal lands before that goroutine has
  turned echo off, then `Restore` runs first. The goroutine turns echo off afterwards and the process exits with echo
  off. In the normal case (Ctrl-C while the prompt is waiting) echo is restored correctly. My reading of the Ctrl-C and
  restore logic is otherwise correct: `ReadPassword` keeps ISIG, NotifyContext cancels, `Restore` runs, and the
  command exits 130. Fix: return early if `p.ctx.Err() != nil` before starting the goroutine. That leaves only a
  window of microseconds; accept it.
- **L8. `config show` hides the effective `require_mention` for a group that omits it.** With
  `require_mention,omitempty`, such a group prints no `require_mention` line at all, although the effective value is
  `true` (verified with group `-1002`). A reader may assume `false`. Either render `MentionRequired()` in `config show`,
  or accept this and note it.
- **L9. One stale comment:** `internal/channel/telegram/channel.go:195` still says "a MarkdownV2 400". Output now uses
  HTML parse mode.
- **Pre-existing, not introduced here:** the `docs/configuration.md` row for `agent.workspace` says it is "the default
  for `tools.filesystem.roots` and `tools.exec.cwd`". That is false. `defaults.go:42,54` are independent
  `~/mtclaw-workspace` literals: setting `agent.workspace` alone leaves `exec.cwd` at the old default (verified with
  `config show`).

## Verified OK (focus items)

1. **Regressions:** none found beyond M1. All packages pass `-race`, and cli/config/version pass on Go 1.25.7.
2. **Annotation gating:** every runnable command declares `Annotations["config"]`, and the declarations match the old
   `skipsConfigLoad`/`isReadOnlyCommand` lists. The one intended change is that `sessions rm` is now `full`.
   `TestEveryRunnableCommandDeclaresAConfigAnnotation` enforces this. Smoke test with no config present:
   - exit 0: `help`, `--help`, `completion bash`, `sessions --help`, `sessions list --help`, `version`, `config path`,
     `__complete sessions ''`
   - fail with the onboard pointer: `config show`, `sessions list`, `cron list`
   - no files created under `HOME`
   - `--help` on a `full` command short-circuits in cobra before PersistentPreRunE.
3. **Workflows:**
   - Every action major tag exists and is the current major: `checkout@v7` (v7.0.1), `setup-go@v7` (v7.0.0),
     `upload-artifact@v7` (v7.0.1), `action-gh-release@v3` (v3.0.3), `govulncheck-action@v1` (v1.1.0).
     `setup-go@v7` still has the `go-version-file`/`check-latest`/`cache` inputs used here.
   - Release is gated on `go vet` and `go test` for the tagged commit.
   - The test matrix is pinned via `go-version-file: go.mod`, and there is no `toolchain` line in go.mod.
   - The govulncheck job's inputs are valid.
   - The release recipe is reproducible: two linux/amd64 builds with the exact `make -n release` flags gave identical
     SHA256s, and Date comes from `vcs.time`. `go test` leaves the working tree clean, so released binaries are not
     marked `-dirty`. `dist/` and `bin/` are gitignored.
   - The checksum step picks `sha256sum` or `shasum -a 256`. `SHA256SUMS` is not included in its own sum, because glob
     expansion happens before the redirect.
4. **Config compatibility (smoke):**
   - `agent.temperature: 0.7`, `1`, and `0` load as a set pointer. `null` or absent means unset.
   - `require_mention: false` and `true` are kept. A listed group without the key has an effective value of `true`.
   - A user-written `groups:` map entirely replaces the default `"*"` entry, as the docs say.
   - `exec.enabled: true` with `mode: "off"` normalizes to `enabled: false`.
   - The only compatibility break is M1.
5. **Docs claims checked against source:**
   - 256 KiB history budget (`loop.go:278`)
   - 3500 display / 64 KiB audit caps (`approver.go:210,218`)
   - credential alphabet (`approver.go:230`)
   - default-env stripping (`exec.go:89-92`)
   - `/bin/bash -lc` default shell (`exec.go:133`)
   - `taskkill /F /T` and Setpgid+SIGKILL
   - approval `<pre><code>` escaping (`telegram/approver.go:441`)
   - bot command table (`commands.go`)
   - `/new` runs cancel-then-reset on the worker (`dispatch.go:417-449`)
   - lock path (`gateway.LockPath`)
   - doctor's auto-mode warning
   - cron 5-field rule and the `deliver_to` skip for disabled jobs
   - All accurate, except M1's doc row, M2's docs sentence, L5, and the pre-existing workspace row.
6. **Onboard Ctrl-C and echo restore:** correct apart from L7.
7. **Plan references:** see L2.

## Recommended actions

1. M1: normalize case and whitespace and accept `warning` in `log.*` validation; fix the doc row and the `logging.New` comment.
2. M2: move `DisableEnvironRead` into `state.newLoop`; fix the docs sentence.
3. L1, L2, L3, L5, L9: small edits of one or two lines each.
4. L4, L6, L7, L8: optional hardening or doc notes.

## Metrics

- Type coverage: N/A (Go, statically typed).
- Test coverage: not measured. One test in scope always skips (L3).
- Lint: `go vet` 0, `gofmt` 0, `actionlint` 0.

## Unresolved questions

- M1: should `warning` and mixed-case spellings stay accepted as aliases (recommended, since they worked at HEAD), or is
  the break intended and needing only a changelog note? This is a product call.

Status: DONE_WITH_CONCERNS
Summary: No Critical or High issues. Gating, workflows, reproducibility, and most docs claims check out. Two Medium
issues: `log.level: warning`/`INFO` configs that used to work now fail to load, and `cron run` skips `DisableEnvironRead`.
