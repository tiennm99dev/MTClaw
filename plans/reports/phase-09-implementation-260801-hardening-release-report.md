# Phase 9 Implementation Report: Hardening and Release

- Date: 2026-08-01
- Plan: `plans/260731-2219-mtclaw-core-system/`
- Phase file: `plans/260731-2219-mtclaw-core-system/phase-09-hardening-and-release.md`
- Status: **completed**

## Files created

| File | Lines | Purpose |
|---|---|---|
| `internal/cli/doctor_cmd.go` | 133 | `Check`/`Result`/`Row` types, `runDoctor`, table/JSON printer, `mtclaw doctor` command |
| `internal/cli/doctor_checks.go` | 403 | One function per check, in the phase file's table order |
| `internal/cli/doctor_test.go` | 346 | Unit tests per check + full-pipeline FAIL-exits-nonzero + JSON round-trip |
| `internal/cli/onboard_cmd.go` | 439 | `mtclaw onboard`'s full sequence, refuse-to-clobber, config writer |
| `internal/cli/onboard_prompts.go` | 137 | `prompter`/`telegramCapturer` interfaces + real (TTY/network) implementations |
| `internal/cli/onboard_test.go` | 240 | Scripted-prompter tests: no-secret-written, capture branches, refuse-to-overwrite |
| `internal/cli/prompts/AGENTS.md` | 47 | Embedded starter system prompt |
| `internal/channel/telegram/capture.go` | 60 | `CaptureSenders` - the temporary long-poll used by onboard |
| `internal/config/docs_coverage_test.go` | 105 | Reflects over `Config`, asserts every YAML key documented |
| `docs/configuration.md` | 139 | Every config key: type, default, effect, what breaks |
| `docs/security.md` | 193 | Phase-5 threat model verbatim + decision pipeline + everything else required |
| `docs/telegram-setup.md` | 109 | BotFather, privacy mode, IDs, supergroups, `require_mention` |
| `docs/architecture.md` | 147 | Component diagram, package boundaries, request lifecycle, not-built list |
| `.github/workflows/ci.yml` | 85 | ubuntu/macos/windows matrix: vet, build, `-race` test, gofmt |
| `.github/workflows/release.yml` | 78 | Tag-triggered cross-compile matrix, ldflags stamping, SHA256SUMS |

## Files modified

- `internal/provider/openai/client.go` - added `ListModels` (doctor's "Model exists" check) and `NewWithAPIKey` (onboard's transient, never-persisted key verification), plus a `context`/`time` import.
- `internal/channel/telegram/channel.go` - added `GetMe(ctx, token)`, a one-shot helper in `SendOnce`'s shape, used by doctor and onboard.
- `internal/cli/root.go` - registered `onboard`/`doctor` commands; added both to `skipsConfigLoad` (onboard writes a config that doesn't exist yet; doctor loads the config itself so a broken one becomes a diagnosable row, not a crash).
- `README.md` - full rewrite: security warning above install, install, 5-minute quickstart, command table, docs links, Windows honesty note.
- `Makefile` - added `race` and `release` targets; `clean` now also removes `dist/`.
- `go.mod` / `go.sum` - added `golang.org/x/term` (hidden secret input in `onboard`'s real `stdioPrompter`; already in the module cache/proxy, no other change to the dependency set).
- `plans/260731-2219-mtclaw-core-system/phase-09-hardening-and-release.md` - frontmatter `status: completed`.
- `plans/260731-2219-mtclaw-core-system/plan.md` - phase 9 row -> Completed.

**Deviation from the literal file list**: `internal/cli/root.go`, `internal/provider/openai/client.go`, and `internal/channel/telegram/channel.go` are not in the phase file's "Related Code Files" section, but wiring `onboard`/`doctor` into the command tree, listing models, and calling `getMe` are functionally required by the phase's own spec (doctor's "Model exists"/"Telegram getMe" checks, onboard's model/token verification steps) and have no other home. All three are small, additive, non-breaking changes to files owned by completed, non-parallel phases (3, 6, 1) with no concurrent phase in flight.

## Tasks completed

- [x] `[]Check` registry (`struct{Name string; Run func(ctx, *config.Config) Result}`) built before the checks themselves, in table order; `onboard` calls `runDoctor` directly, `doctor` wraps it with a `--json`/table printer and a non-zero exit on any FAIL.
- [x] Every table check implemented: config validity (special-cased - it's what produces the `*Config` every other check needs), file permissions, state dir, DB open/migrate, instance lock (INFO), OpenAI key/reachable/model-exists, Telegram token/getMe, allowlist, workspace, filesystem roots, exec cwd, shell, deny-list sanity (loud WARN on empty), `exec.mode: auto` (always WARN), cron (next-due times via `gronx.NextTickAfter`).
- [x] Network-dependent checks (OpenAI reachable, model exists, Telegram getMe) degrade to an actionable FAIL when credentials are absent, with no network call attempted.
- [x] `onboard`: OS-appropriate shell/deny-list, env-indirection for both secrets with the exact export/setx line printed, transient verification (`ListModels`, `getMe`) that never persists the typed value, the full Telegram ID capture sequence (60s window, collect-all, explicit confirm, multi-sender forces a manual choice, manual fallback with a `/whoami` pointer), workspace creation, `exec.mode: approval` always (never offers `auto`), starter `AGENTS.md` written and referenced, ends by running doctor.
- [x] Refuses to clobber: an existing config file is never touched; onboard prints it (redacted) next to a fresh-defaults preview instead.
- [x] `writeOnboardConfig` uses plain `yaml.Marshal`, not `config.MarshalRedacted` - a discovered-in-testing correctness issue: `MarshalRedacted` always writes a placeholder string into `api_key`/`token`, which is a real, recognized YAML key that `config.Validate` then rejects on reload. Using `MarshalRedacted` for the actual write would have made onboard write a config it could not itself reload.
- [x] Config written 0600 (`os.WriteFile(path, data, 0o600)`), a documented best-effort no-op on Windows.
- [x] `docs_coverage_test.go`: reflects every YAML key path (including slice-of-struct as `path[].field` and map values as `path.*.field`); passes against `docs/configuration.md`, and a second test proves the mechanism itself fails on a real gap using a throwaway fixture type.
- [x] `docs/security.md` carries the phase-5 Security Model section verbatim, plus the decision pipeline diagram, deny-list limits (including the two documented false positives), why `auto` is beta, why cron is allow-list-only, the redaction-is-best-effort statement, and the atomicity/`exec_audit` crash exposure from phase 4.
- [x] CI (`ci.yml`): ubuntu/macos/windows matrix, `go vet`/`go build` at `CGO_ENABLED=0`, `gofmt -l` check, `go test -race ./...` with `CGO_ENABLED=1` (necessarily - `-race` requires cgo, which is why the release build stays CGO-free instead), a mingw-w64 install step for the Windows leg (not preinstalled on `windows-latest`), test output uploaded on failure. Both workflow files parse as valid YAML (verified with PyYAML; `actionlint` not run, per the phase's own note that it isn't required).
- [x] Release (`release.yml`): tag-triggered, the exact 5-target matrix from the phase file, `-X internal/version.{Version,Commit,Date}` (full import paths, as `-ldflags -X` requires), `-trimpath -s -w`, `SHA256SUMS`, attached via `softprops/action-gh-release`.
- [x] `Makefile`: `release` target mirrors `release.yml` exactly (same ldflags, same 5-target loop, same checksum step); `race` target added.
- [x] Version stamping verified locally (see below).

## Discrepancy noted, not silently resolved

The phase file's own prose says "Tagged release produces **six** static binaries" (Success Criteria and Tests/Validation), but its own matrix table and this task's explicit matrix bullet list both enumerate exactly **five** targets: linux amd64/arm64, darwin amd64/arm64, windows amd64. I built and shipped the five-target matrix the table specifies rather than inventing a sixth (e.g. windows/arm64) to match the prose count, since the structured table is the more authoritative artifact and a phantom target would need its own verification story. Flagging this here rather than silently "fixing" the phase file's wording.

## Version stamping - verified locally

Ran the `Makefile release` target's logic directly (no `make` binary in this
environment; ran the equivalent shell commands and confirmed they match the
Makefile verbatim) and produced all 5 targets into `dist/` (removed afterward -
not a build artifact meant to persist in the tree):

```
dist/mtclaw-abffab0-dirty-linux-amd64
dist/mtclaw-abffab0-dirty-linux-arm64
dist/mtclaw-abffab0-dirty-darwin-amd64
dist/mtclaw-abffab0-dirty-darwin-arm64
dist/mtclaw-abffab0-dirty-windows-amd64.exe
dist/SHA256SUMS
```

Ran two of them:
- **Host binary** (windows-amd64): `mtclaw version` -> `mtclaw abffab0-dirty (commit abffab0, built 2026-08-01T10:24:40Z)`.
- **linux/amd64**, via WSL2 Ubuntu: `mtclaw version` -> `mtclaw abffab0-dirty (commit abffab0, built 2026-08-01T10:22:27Z)`.

Both report the `git describe`-derived version, confirming `-ldflags -X` stamping actually reaches `internal/version`'s package vars. darwin (amd64/arm64) and linux/arm64 build cleanly (`CGO_ENABLED=0`, pure-Go sqlite) but were not executed - noted honestly in `release.yml`'s own release-notes body rather than implied as verified.

## Tests status

- Type check / build: `CGO_ENABLED=0 go build ./...` - **pass**.
- `go vet ./...` - **pass**, no findings.
- `gofmt -l .` - **pass**, no output (nothing unformatted).
- `CGO_ENABLED=0 go test ./...` - **pass**, all 10 packages with tests green.
- `go test -race ./...` (CGO enabled locally via mingw-w64/gcc already on this machine) - **pass**, all packages green.
- `internal/config` docs coverage test - **pass**, plus its own demonstrably-fails companion test - **pass**.
- New unit tests added this phase: 17 doctor-check tests + 2 full-`runDoctor`-pipeline tests + 5 onboard tests, all green.

**Hermeticity note (self-caught and fixed):** an early version of the doctor/onboard tests touched the *real* `~/.mtclaw` (state dir, a real `mtclaw.db`, a real `prompts/AGENTS.md`) and a real `~/mtclaw-workspace`, because `config.StateDir()`/`config.Default()`'s `~/...` paths resolve through the actual `os.UserHomeDir()` and nothing pointed them elsewhere. Caught via file timestamps, cleaned up, and fixed by (a) threading `stateDir`/`resolveErr` into `checkStateDirWritable`/`checkInstanceLock` as parameters instead of calling `config.StateDir()` inside them, and (b) a `useFakeHome(t)` test helper (`t.Setenv("HOME"/"USERPROFILE", tmp)`) used by every onboard test and the one doctor integration test that still needs the real check-registry path. Verified clean before and after the final full-suite run. Note: `go test ./...` at the repo level was separately observed once to recreate an *empty* `~/.mtclaw` directory, sourced from a package other than `internal/cli` (not reproduced when running `internal/gateway` or `internal/cron` individually) - this is a pre-existing gap in an earlier phase's tests, outside phase 9's file ownership; flagged here rather than silently patched.

## Security verification checklist

Per the phase file: execute what's automatable, cite the test, record the
result; anything needing a live bot/token is MANUAL PENDING with exact
instructions. No result was fabricated - every "PASS" row below was run in
this session, output captured above or in the referenced test file.

| # | Item | Method | Result |
|---|---|---|---|
| 1 | Deny-listed command from Telegram: refused, no prompt, `exec_audit` row present | `go test ./internal/tools/... -run TestExec_DenyRefusesWithoutRunningAndAudits` | **PASS** (mechanism proven; literal "from Telegram" round trip is MANUAL PENDING - see below) |
| 2 | Same command + a matching allow-list entry: still refused (deny precedence) | `go test ./internal/tools/... -run TestPolicy_Table` (subtest "deny wins over an allow-list match on the same command") | **PASS** |
| 3 | `rm --recursive --force /tmp/probe` (long options): refused | `go test ./internal/tools/... -run TestDenyCorpus_BothBypassesFromRedTeam` | **PASS** |
| 4 | `/bin/rm -rf /tmp/probe` (path-prefixed): refused | same test as #3 | **PASS** |
| 5 | `web_fetch` -> `http://127.0.0.1:<port>`: refused | `go test ./internal/tools/... -run TestWebFetch_RefusesLoopbackTarget` | **PASS** |
| 6 | `web_fetch` -> `http://[::ffff:127.0.0.1]`: refused | `go test ./internal/tools/... -run TestIsBlockedAddr_Matrix` | **PASS** |
| 7 | `web_fetch` -> `http://169.254.169.254`: refused | same test as #6 | **PASS** |
| 8 | `web_fetch` -> public URL 302-ing to `127.0.0.1`: refused | `go test ./internal/tools/... -run TestWebFetch_RedirectToLocalhostRefused` | **PASS** |
| 9 | `read_file` `../../../../etc/passwd`: refused | `go test ./internal/tools/... -run TestResolve_TraversalOutsideRootRejected` | **PASS** |
| 10 | `read_file` via symlink to `/etc/passwd`: refused | `go test ./internal/tools/... -run TestResolve_SymlinkEscapingRootRejected` | **PASS** |
| 11 | Non-allowlisted user messaging the bot: no reply, no session | `go test ./internal/channel/telegram/... -run TestDecide_Matrix` ("private denied") | **PASS** (gating rejects before any session/forward code runs; literal live confirmation is MANUAL PENDING) |
| 12 | Non-allowlisted group member tapping Approve on someone else's prompt: rejected | `go test ./internal/channel/telegram/... -run TestApprover_CallbackFromNonAllowlistedUserIsRejected` | **PASS** |
| 13 | Allowlisted user in a *different* chat tapping the callback: rejected | `go test ./internal/channel/telegram/... -run TestApprover_CallbackFromDifferentChatIsRejected` | **PASS** |
| 14 | `grep -ri` a full session's logs for the bot token/API key: zero hits | Code review (every `Token()`/`APIKey()` call site passes the value only to `option.WithAPIKey`/`telego.NewBot`, never to a logger; `telego.WithDiscardLogger()` is used specifically to keep the token out of the SDK's own logs) | **MANUAL PENDING** - a true full-session grep needs a live gateway run against real Telegram/OpenAI credentials; instructions below |
| 15 | `mtclaw config show` output checked for secret material: zero hits | Ran locally: built a temp config with `OPENAI_API_KEY=sk-THISISASECRETVALUE...` set, `go run . --config <temp> config show \| grep -c THISISASECRETVALUE` | **PASS** - 0 hits; output showed `api_key: <set:env:OPENAI_API_KEY>`, never the value |
| 16 | Inline bearer token: neither the approval message nor `exec_audit` contains it | `go test ./internal/tools/... -run TestExec_RedactSecretsAppliedToAuditAndExecutedCommandUnaltered` | **PASS** |
| 17 | `/stop` during a long-running command: process and children gone | `go test ./internal/gateway/... -run TestDispatch_CancelSession_StopsInFlightTurn_WorkerSurvives` + `go test ./internal/tools/... -run TestExec_TurnCancellationKillsProcessTree` | **PASS** (mechanism proven; literal live `/stop` over a real Telegram chat is MANUAL PENDING) |
| 18 | SIGTERM with an approval prompt unanswered: exits within the drain deadline, not `approval_timeout` | `go test ./internal/gateway/... -run TestDrain_ReturnsAtDeadlineWhenWorkersOutlastIt` | **PASS** |
| 19 | `onboard` second sender during capture window: no ID written automatically, forced choice | `go test ./internal/cli/... -run TestRunOnboard_TelegramEnabled_MultipleSendersWritesNone` (written this phase) | **PASS** (fake-poller unit test, per the phase file's own instruction; a live second-sender confirmation is MANUAL PENDING) |
| 20 | `auto` mode with classifier forced to error: prompts rather than running | `go test ./internal/tools/... -run TestPolicy_Table` (subtest "auto mode, classifier error: fails closed to ask") | **PASS** |
| 21 | Cron turn attempting an unmatched command: refused with the no-approver message | `go test ./internal/gateway/... -run TestCronTurn_ExecApprover_UnmatchedRefused_AllowListedRuns` | **PASS** |

### MANUAL PENDING - exact instructions

These three need a live Telegram bot (and, for the first, a real OpenAI key)
and cannot be faked without one:

1. **Full live-session log grep (#14).** `mtclaw onboard` with a real bot
   token and OpenAI key, `mtclaw gateway --log-level debug`, send a few
   messages including one exec command, `Ctrl-C`, then:
   `grep -ri "$(printenv OPENAI_API_KEY)" -r ~/.mtclaw/ /path/to/log/file`
   and the same for the Telegram token. Expect zero hits in both.
2. **Live Telegram round trip (#1, #11, #17).** DM the bot from an
   allowlisted account (expect a reply) and from a non-allowlisted one
   (expect silence); run a deny-listed command from the chat and confirm
   the inline "refused" reply with no approval buttons; start a long
   command (`sleep 120` behind `approval`-mode approval) and send `/stop`,
   then confirm on the host: `ps aux | grep sleep` (POSIX) shows nothing.
3. **Live second-sender capture (#19).** Run `mtclaw onboard` with a real
   token, and during the 60s capture window have two different Telegram
   accounts message the bot. Confirm onboard prints both usernames/IDs and
   writes neither automatically.

## Issues encountered

- **`MarshalRedacted` is unsafe for writing the real config file** (see
  Tasks Completed above) - caught by the "written config must reload"
  acceptance criterion itself, not a separate review pass. Fixed by writing
  `onboard`'s output with plain `yaml.Marshal` instead.
- **Onboard's own final `doctor` run would almost always report a FAIL**
  (a freshly-exported env var is typically not visible in the *current*
  shell session yet) - initially made `onboard` return a non-zero exit in
  that case, which is wrong UX for something only a shell restart fixes.
  Changed `onboard` to always print the doctor report and return success;
  the FAIL rows are the "next steps," not an onboard failure.
  `onboard_test.go` doesn't need this fix to pass (require.NoError already
  covers it), but it materially changes real-world behavior for the better.
- **A telegram-enabled config with no ID confirmed is otherwise
  unloadable** - `config.Validate` correctly refuses `enabled: true` with an
  empty allowlist, which the multi-sender/no-manual-entry onboard path can
  produce. Added `telegramHasAnyAllowlist` + an automatic
  `enabled: false` fallback with an on-screen explanation, so onboard's own
  acceptance criterion ("writes a config that `config.Load` + `Validate`
  accept") holds in every scripted path, not just the happy one.
- **Hermeticity leak into the real home directory** - see Tests status
  above; fixed for every file this phase owns.
- Phases 1-8's work in this working tree is entirely uncommitted (confirmed
  via `git status` at session start/throughout); per instructions, nothing
  was committed this phase either.

## Unresolved questions

1. The phase file's "six binaries" (prose) vs. its own five-target matrix
   (table) - see "Discrepancy noted" above. I built five; flag if a sixth
   target (most likely windows/arm64) was actually intended.
2. `docs/configuration.md`'s accuracy is enforced only for *presence*, per
   the coverage test's documented limit - a maintainer should still read it
   against `internal/config/validate.go` on future schema changes.
3. The three MANUAL PENDING items above need a live bot token and (for one)
   a real OpenAI key, neither available in this environment; they should be
   run once before the first tagged release.

Status: DONE
Summary: doctor/onboard implemented with a shared `[]Check` registry, docs set complete (coverage-enforced), CI/release workflows and a matching Makefile `release` target added, five release binaries built and version-stamping verified on two of them, and the security checklist executed with 18/21 items passing automated tests and 3 correctly flagged MANUAL PENDING (live bot required).
Concerns/Blockers: none blocking; see Unresolved questions for follow-ups (six-vs-five binary count discrepancy in the phase file's own prose, and the three MANUAL PENDING live-bot checklist items to run before the first real tag).
