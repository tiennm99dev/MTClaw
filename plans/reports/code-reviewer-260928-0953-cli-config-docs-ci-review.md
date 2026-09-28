# Third-round review: CLI / config / logging / version / build / CI / docs

Date: 2026-09-28. Branch `refactor/260928-full-review` @ `cd7806d`. Read-only review; no source files changed.
Slice: `main.go`, `internal/cli`, `internal/config`, `internal/logging`, `internal/version`, `Makefile`,
`.github/workflows/{ci,release}.yml`, `go.mod`, `README.md`, `docs/*.md`.
Items fixed in rounds 1-2 are not re-reported. Accepted product decisions (fs writes not gated, no exec_audit
requester columns, `sh -c`/`eval` bypass, non-OpenAI `base_url`) are not re-argued.

## Checks run

| Check | Result |
|---|---|
| `go vet ./...` | clean |
| `go test -count=1 ./internal/cli/... ./internal/config/... ./internal/logging/...` | pass |
| Coverage | cli 58.7%, config 87.5%, logging 95.7% |
| `govulncheck ./...` with `GOTOOLCHAIN=go1.25.7` (what CI/release use) | **14 reachable stdlib vulns** |
| `govulncheck ./...` with go1.27.1 | 0 reachable |
| Binary probes (`CGO_ENABLED=0` build, scratch `HOME`) | see B2, B3, B4 |
| gronx probe (scratch module) | 6-field and 7-field expressions accepted, see B3 |

---

## Bugs

### High

**B1. Release and CI binaries are built with Go 1.25.7, which has 14 reachable stdlib vulnerabilities**
`.github/workflows/release.yml:28-30`, `ci.yml:24-27` (`go-version-file: go.mod`), `go.mod:3` (`go 1.25.7`).
`setup-go` with `go-version-file` installs exactly the `go` directive's version. The `go` directive is a
minimum, but here it silently works as a pin on a patch release that is now far behind. Go 1.27 is out, so the
1.25 line gets no more support. govulncheck run with that toolchain found reachable vulns in `crypto/tls`
(GO-2026-6090, -5856, -4870), `crypto/x509` (-5037, -4947, -4946), `net/http` (-5026, -4918), `net/url`
(-6218, -4601), `net`, `net/textproto`, `encoding/asn1`, and `os` (GO-2026-4602, Root escape, reached from
`tools.walkDir`). Every one is reachable from the Telegram, OpenAI, or web_fetch paths. The same tree built with
go1.27.1 reports 0.
Why this matters: every published binary ships known TLS/HTTP DoS and parsing bugs. The process talks to
untrusted web servers (web_fetch) all day.
Fix: in `release.yml`, use `go-version: stable` with `check-latest: true`. That is a moving tag, which fits the
workspace's version-pinning preference. Add a `govulncheck` step to CI (`golang/govulncheck-action@v1`). CI can
keep the `go.mod` minimum on one leg to prove the minimum still builds.

### Medium

**B2. `mtclaw help`, `mtclaw completion <shell>`, and shell TAB completion all fail without a valid config**
`internal/cli/root.go:227-234`. `skipsConfigLoad` whitelists four hard-coded `CommandPath()` strings. Cobra's
built-in `help`, `completion *`, and `__complete` commands inherit the root's `PersistentPreRunE`. Verified on a
fresh `HOME`:
```
$ mtclaw help               -> Error: config file not found: .../.mtclaw/config.yaml
$ mtclaw completion bash    -> Error: config file not found ...
$ mtclaw __complete sess    -> Error: config file not found ...
```
On a fresh install, the first `mtclaw help` fails, and completion scripts can't even be generated. With a valid
config, every TAB press loads and validates the config and opens `log.file`, because `__complete` is not in
`isReadOnlyCommand`.
Fix: see refactor R1. At minimum, also skip when `cmd.Name()` is `help`, `cobra.ShellCompRequestCmd`, or
`cobra.ShellCompNoDescRequestCmd`, or when the parent is `completion`. Add a test that runs `help` and
`completion bash` with no config.

**B3. A 6- or 7-field cron schedule passes validation but never fires (or fires years late)**
`internal/config/validate.go:225` (`gronx.IsValid`), `internal/cron/scheduler.go:194` (per-minute `IsDue` at
second 0). gronx treats a 6-field expression as seconds-first. `schedule: "30 0 9 * * *"` passes
`config validate` (verified), and `cron list`/`doctor` print a "next due" time. The scheduler only evaluates
`:00` seconds, so the job never runs and nothing reports why. A 7-field year-suffixed expression is also
accepted. Docs (`configuration.md:124`, `types.go:201`) promise "5-field".
Fix: `validateCron` should require `len(strings.Fields(s)) == 5` or an `@`-macro, then call `gronx.IsValid`.
Add table cases.

**B4. Adding any `channels.telegram.groups` entry silently removes the `"*"` default and turns mention gating off for that group**
`internal/config/defaults.go:127-129`, `types.go:143`, `internal/channel/telegram/gating.go:40-57`. Decoding a
user `groups:` map replaces the default map, so `"*"` is gone. A listed group without `require_mention` gets
Go's zero value, `false`. Verified with `config show`:
```yaml
groups:
  "-100123":
    require_mention: false   # user wrote only allow_from: [42]
```
Result: the bot sends every message from allowlisted members in that group to the model (cost, unwanted
replies), and every unlisted group is now refused ("no group entry"). The docs say the opposite:
`configuration.md:71-72` ("`*` is a default applied to any group not otherwise listed") and
`telegram-setup.md:92-93` ("applied via the `*` key that every group not otherwise listed inherits").
Fix: make `RequireMention` a `*bool` with a default of true when unset, or merge each listed entry over `"*"`
after decode. Keep `"*"` unless the user explicitly overrides it. Then fix both docs.

**B5. On Windows release binaries and minimal containers, any IANA `cron.timezone` makes the whole config fail to load**
`internal/config/validate.go:198`, `main.go`. On Windows, Go's `platformZoneSources` is empty
(`$GOROOT/src/time/zoneinfo_windows.go:13`). Without `time/tzdata` embedded, `LoadLocation` only works if
`ZONEINFO` is set or a GOROOT exists at the build path. No file imports `time/tzdata`. So
`cron.timezone: Asia/Saigon` on a release `.exe` makes `Validate` fail for every command, including
`config validate` and `gateway`. Scratch or distroless Linux images without `/usr/share/zoneinfo` behave the
same way.
Fix: add `import _ "time/tzdata"` to `main.go` (about 450 KB). Mention it in the README's Windows note.

**B6. `onboard` silently overwrites a user-edited `prompts/AGENTS.md`**
`internal/cli/onboard_cmd.go:375-384`. `os.WriteFile` truncates. A user who deletes or renames `config.yaml`
to re-onboard, which is exactly what the refusal message tells them to do (`onboard_cmd.go:154`), loses their
customized system prompt with no warning. That contradicts onboard's own refuse-to-clobber rule.
Fix: open with `O_CREATE|O_EXCL`. On `ErrExist`, keep and reference the existing file and print a note. Also use
`O_EXCL` for the config write (`onboard_cmd.go:406`) to close the stat-then-write race.

**B7. Ctrl-C at an onboard secret prompt is ignored the first time, and the second Ctrl-C leaves the terminal with echo off**
`internal/cli/onboard_prompts.go:79-85`. `term.ReadPassword` clears ECHO and keeps ISIG (verified in the x/term
source). It blocks outside the ctx-aware `readLine` goroutine. The first SIGINT cancels ctx, but nobody observes
it. The second SIGINT kills the process through the restored default disposition, and `ReadPassword`'s deferred
termios restore never runs. The user's shell is left with no echo until `stty sane`.
Fix: call `term.GetState` before reading, run `ReadPassword` in a goroutine, `select` on `p.ctx.Done()`, and call
`term.Restore(fd, state)` on cancel.

**B8. Untrusted content is printed raw to the operator's terminal (escape-sequence injection)**
`internal/cli/sessions_cmd.go:91-97` (message `Content`, `ToolCalls`), `approvals_cmd.go:60-63` (`Command`),
`prompt_cmd.go:57`. Stored tool results include web_fetch page bodies. Neither the tools nor the CLI strip
control characters (grep found none). A fetched page can plant ESC/OSC sequences that run when the operator
runs `mtclaw sessions show`: title spoofing, cursor movement that hides lines, and OSC 52 clipboard writes on
terminals that allow it. The last is the audit-review path the security docs tell users to rely on after a
crash. Embedded newlines in `Command` also break the `approvals list` table.
Fix: pass display fields through one `sanitizeForTerminal` helper that replaces C0/C1 controls except `\n` and
`\t` (and `\n` too for table cells) with `\xNN`.

**B9. The instance lock is keyed to `$HOME`, not to the database it protects**
`internal/cli/cron_cmd.go:189-203`, `doctor_checks.go:277`, `internal/gateway/gateway.go:51-59`. The lock is
always `~/.mtclaw/gateway.lock`, no matter what `--config` or `storage.path` says. Two failure modes:
- The gateway runs as a service with `HOME=/var/lib/mtclaw`, and the operator runs
  `mtclaw --config /etc/mtclaw.yaml cron run job` from a login shell. The lock check sees no gateway, and both
  processes append to the same persistent session. That is the exact interleaving `cron run` was meant to refuse.
- Two independent installs (two configs, two DBs) under one user can't run two gateways, and `cron run` on one
  refuses because of the other.
The lock location is not documented anywhere (grep of README/docs).
Fix (touches the gateway code, which is outside this review's scope): derive the lock from the DB,
`storage.path + ".lock"`. Export one `gateway.LockPath(cfg)` and use it from all three call sites.

**B10. Every workflow action is on a major that targets Node 20, which GitHub removed from runners on 2026-09-23**
`ci.yml:21,24,98`, `release.yml:20,26,64`. These are `actions/checkout@v4`, `actions/setup-go@v5`,
`actions/upload-artifact@v4`, and `softprops/action-gh-release@v2`. Moving major tags are the right style, so the
pinning policy is met and there are no exact pins. But all four majors are superseded (checkout v7, setup-go v7,
upload-artifact v7, action-gh-release v3; each newer major runs on Node 24). The last green CI run
(2026-09-21) predates the removal. From here on, these depend on the runner forcing Node 24 onto Node 20
actions.
Fix: bump to current majors. softprops v3's release notes say v2.6.2 is the last Node 20 line.

**B11. Pushing a tag publishes a release with no test gate**
`release.yml`. There are no `go test`/`go vet` steps and no `needs:` on CI. A tag on a commit that never passed
CI (for example a tag pushed from a local branch) ships immediately.
Fix: add `go vet ./... && go test ./...` before the build step, or add a `workflow_call` to `ci.yml` with
`needs: test`.

### Low

| # | Location | Issue | Fix |
|---|---|---|---|
| B12 | `validate.go` (no rule), `logging/logger.go:43-62` | `log.level: debgu` / `log.format: jsn` load silently as info/text (verified), while every other enum is strict and unknown keys are rejected. | Validate both enums. Keep the lenient fallback only for the `--log-level` flag, or validate that too. |
| B13 | `cron_cmd.go:139-141,219-220` | `recordManualRun` captures `now` after the turn, so `StartedAt == FinishedAt` and `cron list` shows the finish time as "LAST RUN". | Capture `started := time.Now()` before `loop.Run`. |
| B14 | `doctor_checks.go:256-260` | Falls back to a read-write open on any read-only failure. A behind-schema DB gets migrated by `doctor`. On upgrade (new binary, old gateway still running), `doctor` migrates the live gateway's DB. The comment says "never disturbing a live gateway". | Fall back to read-write only on `ErrNotExist`. Report behind-schema as WARN with "restart the gateway to migrate". |
| B15 | `config/load.go:15-35`, `root.go:191-194` | `FileNotFoundError` is never checked with `errors.As` anywhere. A first-run `mtclaw gateway` prints only "config file not found" with no pointer to `onboard`. | In `prepare`, `errors.As` it and append "run `mtclaw onboard` to create one". |
| B16 | `root.go:244-246` | `sessions rm`, the only destructive CLI command, is classified "read-only", so it never writes to `log.file`. | Remove it from the list (it opens the store read-write anyway). |
| B17 | `cron_cmd.go:48-51` | `cron list` fails with "no database yet" instead of listing configured jobs with `-` for last run. | Treat a missing DB as "no runs". |
| B18 | `cron_cmd.go:185-203` | TOCTOU: the lock check happens before the store open and turn, so a gateway starting in between races the persistent session. | Accept and document, or hold a shared lock for the turn's duration. |
| B19 | `prompt_cmd.go:65-66` | `--session X --new` silently ignores `--new`. | `cmd.MarkFlagsMutuallyExclusive("session", "new")`. |
| B20 | `validate.go:241,249-267` | `deliver_to` reachability is enforced even when `cron.enabled: false` or the job is disabled. Temporarily setting `channels.telegram.enabled: false` makes the whole config unloadable if any cron job exists. | Skip or downgrade the reachability rule for disabled jobs or when cron is disabled. |
| B21 | `onboard_cmd.go:55-56,150-157` | The refuse-to-overwrite path exits 0, so scripts can't tell "did nothing" from success. | Return a sentinel error after printing. |
| B22 | `config/load.go:123,127` | The secret file is read unbounded (`/dev/zero` hangs, same class as the round-2 prompt-file fix). An empty secret file reports source `file:...`, which shows as `<set:file:...>` in `config show`. | Use `io.LimitReader` (e.g. 64 KiB) plus a regular-file check. Treat an empty value as `unset`. |
| B23 | `sessions_cmd.go:39-47` | N+1: one `CountBySession` query per session, unbounded `List(ctx, 0)`. | Return the message count from the list query (store change) or add `--limit`. |
| B24 | `main.go:10-12` | Still exits 1 on interrupt (prior L8, not fixed). | Return 130 when `ctx.Err()` is set. |
| B25 | `Makefile:51`, `Makefile:28-29` | `make release` uses `sha256sum`, which macOS lacks (`shasum -a 256`). `make fmt` only lists files and always exits 0. | `command -v sha256sum \|\| alias`; make `fmt` run `gofmt -w .` and add a separate failing `fmt-check`. |
| B26 | `release.yml:31-33`, `Makefile:5` | `Date` is build wall-clock time, so two builds of one tag differ and the published SHA256SUMS can't be reproduced. `fetch-depth: 0` is unneeded because only `rev-parse HEAD` is used; the comment cites `git describe`, which isn't run. | Use the commit time: `git log -1 --format=%cI` (or R2). Drop `fetch-depth: 0`. |

---

## Refactors (each clearly reduces complexity)

**R1. Replace the `CommandPath()` string lists with per-command annotations.** `root.go:221-251`
(`skipsConfigLoad`, `isReadOnlyCommand`). Put `Annotations: {"config": "none"|"inspect"|"full"}` on each command.
`prepare` reads `cmd.Annotations["config"]` and treats unannotated built-ins (`help`, `completion`, `__complete`)
as `none`.
- Benefit: fixes B2 and B16. A renamed command can't silently fall out of a list. The two overlapping
  "read-only" notions (logging vs. store) become one visible declaration per command.
- Risk: low. A new command that forgets its annotation defaults to `full`, which is today's behavior. Add a test
  that walks `root.Commands()` recursively and asserts every runnable command has an annotation.

**R2. Derive version from `runtime/debug.ReadBuildInfo`, and let the release workflow call `make release`.**
`internal/version/version.go`, `Makefile:1-10,37-51`, `release.yml:31-54`. `vcs.revision` and `vcs.time`
(commit time) are stamped automatically in a git checkout, and `BuildInfo.Main.Version` is set by
`go install ...@vX`. Keep only `-X ...Version=` for the tag name. `release.yml` becomes
`make release VERSION=$GITHUB_REF_NAME`.
- Benefit: removes the build matrix, ldflags recipe, and checksum logic that are duplicated in two places
  (already drifting: the comment references a plan file). Fixes B26 reproducibility. `go install` users get a
  real version and commit instead of `dev (commit none)`, so the README caveat at lines 49-51 can go.
- Risk: low. `-trimpath` does not suppress VCS stamping. A tarball build without `.git` falls back to the
  current defaults.

**R3. Collapse `tools.exec.mode: off` into `tools.exec.enabled: false`.** `validate.go:172`,
`tools/registry.go:109`, `doctor_checks.go:438-508`. Two switches mean the same thing, and they have already
drifted. With `enabled: true, mode: off`, the tool is unregistered, but doctor still runs the shell, cwd, and
deny checks and warns "deny is EMPTY while tools.exec.enabled is true". The exec bounds are still validated.
`configuration.md:103` says `enabled: false` "is the only way" while line 104 says `off` is equivalent.
- Fix: at load, normalize `mode: off` to `Enabled = false` (keep accepting the value for compatibility) and have
  one predicate everywhere.
- Benefit: one source of truth. Risk: none for existing configs.

**R4. Delete doctor checks the code itself calls dead, and add the one that is missing.**
`doctor_checks.go:382-404` (`checkAllowlistNonEmpty`: its comment says "defensive duplicate, not
load-bearing"), `checkCron`'s `gronx.IsValid` and timezone re-check (`:521-537`; Validate already guarantees
both). Replace them with a "system_prompt_files readable" check. Today a missing AGENTS.md is only a log Warn
(`agent/prompt.go:89-101`), and `configuration.md:47` itself admits doctor doesn't check it.
- Benefit: about 40 fewer lines and one real diagnostic gained. Risk: none, because `runDoctor` always validates
  first (`doctor_cmd.go:59-70`).

**R5. Remove literals duplicated across packages.**
- `"gateway.lock"` + `config.StateDir()` appears 3 times (`cron_cmd.go:194`, `doctor_checks.go:277`, gateway
  const): export `gateway.LockPath`, which also fixes B9.
- `defaultShellArgv` (`doctor_checks.go:471-479`) mirrors an unexported tools default: export
  `tools.DefaultShell()`.
- The audit decision strings in `approvals_cmd.go:77-90` duplicate literals in `tools/policy.go` and
  `tools/exec.go`: export typed constants.
- Benefit: drift becomes a compile error. Risk: trivial.

**R6. Trim history-narrating comments and plan references.** The workspace rule forbids plan IDs, phase numbers,
and audit labels in code comments and test comments. Current violations:
- `onboard_cmd.go:30,72`, `approvals_cmd.go:13`, `sessions_cmd.go:105`, `root.go:223`, `doctor_checks.go:28,340`
- `validate.go:43`, `types.go:48,190,201`, `defaults.go:104`, `load.go:16,102`
- tests: `root_test.go:54,125`, `root_unix_test.go:15`, `onboard_test.go:272`, `onboard_prompts_test.go:14`
  ("pins the C1/M1 fix"), `validate_test.go:13`, `load_test.go:14`
- `release.yml:48,77`

Several comments also narrate past bugs ("used to spin forever", `onboard_prompts.go:111-112`) instead of
stating the invariant. `doctor_checks.go` and `root.go` average more comment lines than code lines per function.
- Benefit: comments stop rotting (see D9/D10). Risk: none.

Not proposed: a generic "open store and run" wrapper for the seven `ctx := cmd.Context(); st, err :=
s.openStore(...)` sites. Each is 4 lines with a different read-only flag, and a wrapper would add indirection
without removing a real failure mode. File and function sizes are fine: the largest function is `runOnboard` at
about 90 lines, which is sequential and readable.

---

## Docs drift (each checked against source)

| # | Location | Claim | Reality |
|---|---|---|---|
| D1 | `configuration.md:71-72`, `telegram-setup.md:92-93` | `"*"` applies to every unlisted group; `require_mention` defaults true | Any user `groups:` map replaces `"*"`. Listed entries default to `false` (B4). |
| D2 | `configuration.md:124`, `types.go:201` | "5-field cron expression" | 6- and 7-field expressions accepted, and seconds-based ones never fire (B3). |
| D3 | `configuration.md:47` | A missing `system_prompt_files` entry "fails when the agent loop first tries to read it" | Logged at Warn and skipped; the turn runs without it (`agent/prompt.go:89-101`). |
| D4 | `configuration.md:41` | `agent.name` is "cosmetic ... used in log lines and the startup banner" | Not in the banner (`gateway_cmd.go:49-55`). It is interpolated into the system prompt (`agent/prompt.go:41`), so it is not display-only. |
| D5 | `configuration.md:103` vs `:104` | "`enabled: false` is the only way to remove shell access" vs "`off` never registers the exec tool" | They contradict each other (R3). |
| D6 | `configuration.md:126` | A disabled job "shown as 'disabled' in `cron list`" | `cron list` prints `ENABLED false` and next due `-`. Only doctor prints "disabled". |
| D7 | `configuration.md:144` | `log.format` "If wrong: N/A" | Any typo silently becomes text (B12). |
| D8 | `architecture.md:169-171` | Only "cli's wiring and main.go" import sqlite | `cli` and `gateway` import it (`gateway.go:67`). `main.go` does not. |
| D9 | `config/load.go:27-29,51-54`, `types.go:1-2` | Load is "a pure, table-testable function" / "pure load/validate pipeline" | Reads `*_file` from disk, writes warnings to `os.Stderr`, stats the filesystem. `architecture.md` was corrected in round 2, but these code comments were not. |
| D10 | `types.go:189-191`, `defaults.go:104`, `load.go:16-17,101-103` | "gronx is not yet a dependency", "onboard, in a later phase", "a future onboard command" | gronx is in `go.mod` and validates expressions; onboard exists. |
| D11 | `release.yml:70-78` | Release notes: "only linux/amd64 and the release runner's own host binary are actually executed as part of this workflow" | The workflow executes no binary at all. It also cites a "phase 9 implementation report" that users can't see. |
| D12 | `docs/security.md:9` | "the phase 5 design document's Security Model, verbatim" | Refers to an internal plan file readers can't reach. State it as the project's model. |
| D13 | `README.md:31,38,46`, `go.mod:1` | Clone, releases, and `go install` use `github.com/tiennm99/MTClaw` | The repo is `tiennm99dev/MTClaw` (`git remote`). It works today only through GitHub's rename redirect (verified 301, and the proxy resolves). It breaks if `tiennm99/MTClaw` is ever recreated. Changing the module path is a user decision (it is a breaking import-path change); at least point the README links at the real owner. |
| D14 | `README.md:53-54` | "`CGO_ENABLED=0` is used everywhere, so every build ... is a single static binary" | The README's own `go install ...@latest` line does not set it, so that binary may link libc dynamically (net resolver). |
| D15 | none | Instance-lock location | `~/.mtclaw/gateway.lock` regardless of `--config`/`storage.path`; documented nowhere (B9). |
| D16 | `doctor_checks.go:249-255` | DB check "never disturbing a live gateway's connection" | Falls back to a read-write open and migration (B14). |

Re-verified correct: README Go-version wording, exec.cwd "not a jail" rows, workspace creation row,
AGENTS.md location row, `prompt` concurrency note, web_fetch/filesystem/exec bounds rows, storage.path rows,
`/whoami` references (the handler exists at `channel/telegram/commands.go:112`).

---

## Test gaps

- Zero or near-zero coverage: `cron list` (3.6%), `cron run` (20.7%; `recordManualRun`, `deliverCronResult`,
  `refuseIfGatewayLocked` all 0%), `sessions show` (4.3%), `approvals list` (25%), `decider` (0%),
  `resolveCLISession` (0%), `Execute` (0%), `Secret`/`Confirm` (0%). All of these can be tested hermetically
  with a temp-dir SQLite store and `cmd.SetArgs`.
- `TestNewCronRunApprover_WiresDenyAllApprover` and `TestNewPromptApprover_WiresTerminalApprover` test helpers
  that return constants. Neither proves `RunE` calls them, so a `RunE` that stopped using the helper would still
  pass. Better: drive the command through `newRootCmd` with a fake provider seam, or assert on the registry the
  loop was built with.
- No test that `help`/`completion` work without a config (B2), and none that every registered command has an
  explicit config classification (R1).
- No validation tests for 6-field schedules (B3), groups-map default merge (B4), or log enum values (B12).
- `TestRunOnboard_*` never covers a pre-existing `prompts/AGENTS.md` (B6).

## Unresolved questions

1. B4: should listed groups inherit `"*"` field by field (a merge), or should `require_mention` just default to
   true? The first is more intuitive; the second is a smaller change.
2. B9: is one gateway per OS user intended? If so, document it and keep the lock in `$HOME`. If the intent is
   one gateway per database, move the lock next to `storage.path`. This is owned by the gateway code.
3. D13: keep the `github.com/tiennm99/MTClaw` module path (it depends on the redirect) or migrate to
   `tiennm99dev`? This is a breaking import-path change, so it needs the user's call.
4. B1: should CI keep one leg on the `go.mod` minimum (1.25.7) to prove the minimum builds, while release uses
   `stable`? Recommended, but it doubles one leg.
