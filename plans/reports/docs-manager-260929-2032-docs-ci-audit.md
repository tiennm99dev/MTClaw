# Docs vs source vs CI audit (read-only)

Date: 2026-09-29. Scope: README.md, docs/*.md, Makefile, .github/workflows/*.yml
against internal/**. No docs or code were edited. Items already fixed in
`plans/reports/fullstack-developer-260928-1041-final-review-fixes.md`
(log.level aliasing, doctor check rename, lock-location upgrade note, version
"committed", `cron run` sanitizing) were re-verified as fixed and are not re-reported.

Severity: H = would mislead an operator or installer; M = wrong but low blast radius; L = cosmetic/stale coordinate.

## 1. Mismatches (doc says X, source does Y)

| # | Sev | Doc says | Source does |
|---|---|---|---|
| 1 | M | README.md:129-131: Windows runaway-process kill is "via `taskkill /F /T`". | internal/tools/exec_windows.go:12-51 assigns the child to a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` and kills by closing the handle; the file comment (line 22) explicitly contrasts itself with `taskkill /F /T`. Only README still carries the old mechanism (grep of README/docs for `taskkill` hits only README:130). |
| 2 | H | README.md:95-97: onboard captures the Telegram ID "so the bot's allowlist is never left empty". Same idea at docs/configuration.md:78 ("you are never left with a bot that ignores you"). | internal/cli/onboard_cmd.go:106-109: when no id is confirmed (no sender, multiple senders, user declines, blank/unparseable manual entry: lines 330-379) onboard writes `channels.telegram.enabled: false` and leaves `allow_from` empty. The allowlist CAN be empty; the channel is switched off instead, and `mtclaw gateway` then refuses to start (internal/cli/gateway_cmd.go:26-28). Neither doc says this outcome exists. |
| 3 | M | README.md:67-69: lock lives at "`storage.path + ".lock"`, see `docs/configuration.md`'s `storage.path` row". | internal/gateway/lock.go:26-28: `cfg.Storage.EffectiveDSN() + ".lock"`; `storage.dsn` is canonical, `storage.path` is the deprecated alias (internal/config/types.go:248-278). The `storage.dsn` row (docs/configuration.md:169) is the one that describes the lock; the `storage.path` row (line 170) does not. |
| 4 | M | docs/configuration.md:101-103 ends "see `docs/security.md`" for web_fetch's blocked address space, and lists only "loopback, link-local, unspecified, private, and cloud-metadata". | docs/security.md has no web_fetch address-restriction section (only line 25 mentions web_fetch, as an injection source). Dangling cross-reference. Source (internal/tools/web_fetch.go:25-26, 83, 94-100, 109-118, 206, 220-222) also blocks multicast and CGNAT, re-checks at every dial (so a redirect to a blocked host fails), caps redirects at 3, restricts redirect schemes to http/https, is GET-only, and skips binary content types. |
| 5 | M | docs/architecture.md:71-73: only "internal/cli's two production wiring sites (root.go, doctor_checks.go)" blank-import the sqlite driver. | Three sites: internal/cli/root.go:31, internal/cli/doctor_checks.go:24, and internal/gateway/gateway.go:23. |
| 6 | M | docs/architecture.md:15 (mermaid `CRON -->|scheduled prompt| Q`) and :177-178 ("enters at the queue (step 3) via the scheduler"). | internal/gateway/gateway.go:152 hands `disp.dispatch` straight to `cron.New`; only Telegram traffic goes through the bounded `inbound` channel (gateway.go:212, 243). Cron enters at the dispatcher, bypassing the global inbound queue. |
| 7 | L | docs/architecture.md:146: "`telegram.Decide`". | Function is unexported `decide` (internal/channel/telegram/gating.go:18). |
| 8 | M | docs/verification.md:81-82: ci.yml runs "`govulncheck` and a release build gated on the whole suite passing"; :79 "on every push and pull request". | .github/workflows/ci.yml has no release-build step; the vet+test gate lives inside release.yml (lines 194-201) and does not depend on CI. ci.yml:4-6 triggers on push to `main` only (plus all PRs). The description also omits the separate `test-stable` job (ci.yml:111-139). |
| 9 | L | docs/verification.md:179: capture logic at `internal/channel/telegram/capture.go:26-40`. | Lines 26-40 are now the `captureWindowGrace`/`captureWindowStart` comments; `captureWindowStart` is at capture.go:49 and `CaptureSenders` at :53. Stale line coordinates. |
| 10 | L | docs/verification.md:172-174: `mtclaw approvals list` / a `sessions` inspection shows the cron run. | `approvals list` reads `exec_audit` (internal/cli/approvals_cmd.go:30-66), not `cron_runs`. `mtclaw cron list` prints LAST STATUS / LAST RUN from `cron_runs` (internal/cli/cron_cmd.go:56-84) and is the right CLI route. |
| 11 | L | release.yml:167-168: pure-Go sqlite rationale "(see docs/architecture.md)". Makefile:255 comment: `fmt-check` is "what CI runs". | docs/architecture.md never mentions modernc.org/sqlite or CGO (no hits); the rationale lives only in internal/store/sqlite/dialect.go:1-5. CI does not call `make fmt-check`; it inlines gofmt (ci.yml:63-82). |
| 12 | L | release.yml:194-195: release is gated on "the same checks CI runs". | Release runs only `go vet` + `go test` (CGO off); it skips gofmt, `go mod tidy -diff`, `-race`, and govulncheck (ci.yml:59-101, 141-151). Comment overstates. |

## 2. Omissions (user-visible behavior with no doc)

Source of truth: internal/cli/*.go flag registration.

- **Global flags** `--config` and `--log-level` (internal/cli/root.go:206-207) are absent from README's Commands table. `--config`/`MTCLAW_CONFIG` appear only in docs/configuration.md:3-11; `--log-level` only incidentally in verification.md:117,205. configuration.md:19 says precedence is "config file value > built-in default" and never says `--log-level` overrides `log.level`.
- **Per-command flags not in README or any doc**: `prompt --session <id>` / `--new` (mutually exclusive; prompt_cmd.go:66-68); `send --thread` (send_cmd.go:37); `sessions list --limit` (sessions_cmd.go:67) and `approvals list --limit` (approvals_cmd.go:69), default 50, 0 = unlimited; `cron run --deliver` (print-only is the default) and `--ephemeral` (cron_cmd.go:183-184). `--chat` and `doctor --json` are documented.
- **Exit codes**: 130 on signal interruption (main.go:21-26, root.go:68-108); 1 on any other error; `doctor` exits non-zero if any row is FAIL (doctor_cmd.go:81-90, 124-127; WARN does not fail); `onboard` exits 1 when a config already exists (onboard_cmd.go:29, 65-69) but exits 0 even if its embedded doctor run shows FAIL rows (lines 149-156). None documented.
- **`cron run` refuses a persistent job while the gateway holds the lock** (cron_cmd.go:95-105, 125-131). README describes it only as "fires one manually"; configuration.md:32 mentions the contrast obliquely under `prompt`.
- **`onboard` refuses to overwrite an existing config** and never overwrites `prompts/AGENTS.md` (onboard_cmd.go:65-69, 393-412); README says only "writes a working config". It also disables Telegram when no id was confirmed (mismatch 2).
- **Relative-path rule**: `ExpandPath` resolves relative config paths against the config file's directory, not CWD (internal/config/paths.go:103-109). Mentioned only inside the `storage` intro of configuration.md; not stated for `agent.workspace`, `roots`, `cwd`, `*_file`, `log.file`.
- **Audit decision `refused_too_long`** (internal/tools/exec.go:230; approvals_cmd.go:87-88 maps it to DECIDER `policy`) is missing from the pipeline diagram (security.md:49-66) and from the enumerated list in internal/store/types.go:115. security.md:185-188 describes the refusal but not the audit label.
- **Auto-mode classifier has a fixed 10 s timeout** (internal/tools/policy.go:22-24); absent from configuration.md `tools.exec.auto` rows and security.md.
- **Release targets and artifact names** appear only in docs/verification.md:227-228 (Section B, future tense). README's "download the binary for your OS/arch" does not list the five targets (linux/darwin x amd64/arm64, windows/amd64 only; Makefile:282), the naming pattern `mtclaw-<tag>-<os>-<arch>[.exe]` (Makefile:293), that the file must be renamed to `mtclaw` and made executable for the quickstart commands to work, or a checksum command.
- **`git push` deny pattern may be broader than documented** (deny_defaults.go:26, `-f\b` anywhere after `git push`, e.g. an argument ending in `-f`). security.md:100-107 lists only `docker rm -f` and `npm rm -f` as false positives. Low confidence; confirm with a corpus test before documenting.

## 3. Stale / contradictory statements

- README.md:118-123 "Documentation" list omits `docs/verification.md`, which architecture.md:106-107 and plans/260731-2219-mtclaw-core-system/plan.md:208-210 treat as current. README is the only navigation entry point; there is no docs index.
- architecture.md:106-107 docs listing omits `docs/journals/`. **docs/journals is referenced from nowhere** (grep across README, docs, plan.md, Makefile, .github: zero hits). Its single file is a dated stateful record ("Status: Resolved", "the Linux/macOS workflows are untested", "work is currently uncommitted on `docs/mtclaw-core-system-plan`") that contradicts current state; harmless only because nothing routes to it. Label it historical in place or leave it unlinked.
- README "From a release" path (lines 30-33) vs repo reality: `gh release list -R tiennm99dev/MTClaw` returns nothing and `git tag` is empty, so no release binaries or `SHA256SUMS` exist yet; only verification.md Section B.6 admits it. A first-time installer hits an empty releases page.
- configuration.md:12-17 says the docs-coverage test checks field-name presence only. True (`internal/config/docs_coverage_test.go` exists); it would not have caught #4-style errors, which is why hand verification mattered.

## 4. Broken links / paths

- Markdown links: the only relative links are README.md:120-123 (all four resolve) and verification.md:10 (`../plans/260731-2219-mtclaw-core-system/plan.md` resolves). No anchors are used.
- Every `internal/...` file path and every `Test*` name cited in docs/*.md and README resolves (existence check against the tree and `func Name(` in `*_test.go`). Only stale coordinate: verification.md:179 (mismatch 9).
- Dangling prose pointers (not file paths): configuration.md:101-103 to security.md (mismatch 4); release.yml:167-168 to architecture.md (mismatch 11).
- External: README.md:31,38 use `github.com/tiennm99dev/MTClaw` (matches `git remote`). `go install github.com/tiennm99/MTClaw@latest` (README:46): `gh api repos/tiennm99/MTClaw` resolves through a redirect to `tiennm99dev/MTClaw` today, as README:49-54 states. All workflow action tags exist upstream (checkout v7.0.1, setup-go v7.0.0, upload-artifact v7.0.1, action-gh-release v3.0.3, govulncheck-action v1.1.0).

## 5. CI / release observations

- Go version: go.mod `go 1.25.7` (go.mod:3); README:24-28 matches. ci.yml `test` job pins to it via `go-version-file` (ci.yml:31-35); `test-stable` and release use `stable` + `check-latest`.
- CGO: Makefile `build`/`test`/`install`/`release` all set `CGO_ENABLED=0`; `race` does not (needs cgo). ci.yml runs vet/compile/test CGO-free then a CGO-on `-race` pass; release.yml sets `CGO_ENABLED=0` at job level. README:60-63 matches.
- Matrix: CI is ubuntu/macos/windows; release cross-compiles all five targets on one ubuntu runner and never executes them (release.yml:222-227 says so). Windows and arm64 binaries are unexercised at runtime.
- Artifact names / checksums: `dist/mtclaw-<VERSION>-<os>-<arch>[.exe]` plus `dist/SHA256SUMS` (Makefile:288-297); release.yml uploads `dist/*`. verification.md:227-228 is accurate. `make release` depends on `clean`, so the checksum glob cannot pick up stale files. Release builds add `-trimpath -s -w`; `make build` does not, so a local build and a release binary are not byte-comparable (undocumented).
- Windows CI leg: LF pinned via .gitattributes, gofmt under pwsh (ci.yml:73-82). verification.md:92-94 matches.
- Release has no draft/prerelease step and no post-build smoke run; a bad tag publishes immediately.
- `go mod tidy -diff` runs on ubuntu only (ci.yml:59-61); the Makefile has no matching target, so no documented local one-liner.

## 6. Verified accurate (no action)

- **Config schema**: every key in internal/config/types.go appears in docs/configuration.md with matching type and default (defaults.go): agent.max_iterations 20, max_history_turns 40, workspace, openai timeout 120s / max_retries 3 / base_url, tool defaults (262144 / 1048576 / 30s / 1048576 / 120s / 65536 / 5m), `confirm_on` default list, cron.timezone `Local`, storage `sqlite` + `~/.mtclaw/mtclaw.db`, log info/text. Every rule in internal/config/validate.go (1-100, >=2, temp 0-2, absolute roots, negative group ids, 5-field cron, session enum, deliver_to reachability only for fireable jobs, dsn/path conflict, log aliases) is stated correctly.
- **CLI surface**: README:105-116 matches the commands registered in root.go:209-218; doctor check names cited in docs match internal/cli/doctor_checks.go:39-55; `cron list` ENABLED / `-` next-due, `config show` redaction match.
- **security.md**: deny -> allow -> mode order and audit labels (policy.go:150-216, exec.go:260-281); `off` = tool not registered; classifier sees command, cwd, shell only (policy.go:189-200); 3500-char prompt cap and 64 KiB audit cap (approver.go:210,218); cron always `DenyAllApprover` (cron_cmd.go:182-190); `DisableEnvironRead` from gateway_cmd.go:35-42 and `state.newLoop` (root.go:139-141), no-op off Linux (exec_notlinux.go:8); filterEnv defaults (exec.go:101-102); `docker rm -f` false positive follows from deny_defaults.go:16.
- **Telegram**: DM `allow_from`, group lookup with `"*"` fallback, empty group list inherits channel list, `require_mention` default true per entry, reply-to / `@bot` / `/cmd@bot` detection, silent rejection (gating.go:18-165); command set /start /help /new /status /whoami /stop (commands.go:19-26); 4096 chunk limit (chunk.go:13).
- **Config/env resolution**: `--config` > `MTCLAW_CONFIG` > `~/.mtclaw/config.{yaml,yml}` with one warning when both exist (paths.go:12-67); shell defaults (exec.go:163-171); workspace mode 0755 on first write (fs.go:220); log dir 0700 (logger.go:29).
- **Upgrade/downgrade notes** (README:65-82, configuration.md storage section) are consistent with EffectiveDSN/lock behavior and unknown-key rejection.
- **verification.md Section A**: every named test function exists; Section B commands use valid current flags.

## 7. Suggested fix order (not applied)

1. README.md:95-97 and configuration.md:78 (#2).
2. README.md:129-131 (#1) and README.md:67-69 (#3).
3. Add a web_fetch restrictions paragraph to docs/security.md, or repoint configuration.md:101-103 (#4).
4. Point README at `mtclaw --help` for flags/exit codes rather than hand-copying (Omissions).
5. architecture.md #5-#7; verification.md #8-#10; release.yml/Makefile comment nits #11-#12.
6. Add `docs/verification.md` to README's list; decide journals' status.
7. Cut a first tag so the "From a release" path exists, or state none is published yet.

Unresolved questions:
- Hand-document exit codes and flags, or rely on `mtclaw --help` (docs rule prefers pointers)?
- Is the `git push -f\b` false positive worth documenting (needs a corpus check)?
