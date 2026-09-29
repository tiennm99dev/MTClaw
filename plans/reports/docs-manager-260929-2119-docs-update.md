# Docs update report

Scope: README.md, docs/**, comment-only edits in Makefile and .github/workflows/release.yml. No .go, go.mod, ci.yml or .golangci.yml changes. Nothing committed.

## Changes and evidence

README.md
- Windows kill mechanism: Job Object, not taskkill (internal/tools/exec_windows.go:12-71; grandchild window per tools fix report).
- Lock path: `storage.dsn + ".lock"`, pointed at the dsn row (internal/gateway/lock.go, config/types.go).
- Onboard allowlist: writes `enabled: false` when no id confirmed (internal/cli/onboard_cmd.go:100-110); refuses overwrite / never overwrites AGENTS.md (audit, onboard_cmd.go:65-69).
- Global flags, `--log-level` override (internal/cli/root.go:258-259); exit codes (main.go, root.go:93-105, doctor anyFailed doctor_cmd.go:82); `prompt --session/--new` (prompt_cmd.go:66-67); `cron run --deliver/--ephemeral` and gateway-lock refusal (cron_cmd.go:95-201). Other flags: pointer to `--help`.
- Upgrading: existing configs keep the deny list; new shapes listed from internal/tools/deny_defaults.go:14-62; adopt via fresh onboard or copy.
- From a release: file naming (Makefile release recipe, per audit) + build-from-source sentence.
- Documentation list: verification.md added; docs/journals/ labeled historical.

docs/configuration.md
- Relative-path rule, multi-document / empty YAML (internal/config/load.go:74-85), `--log-level` precedence.
- Allowlist sentence corrected (onboard disables Telegram).
- Secret files: trim + 64 KiB (load.go:21, 180-196).
- web_fetch pointer repointed to new security.md section (adds multicast, CGNAT, redirects).
- `tools.exec.shell` >=2 elements (internal/config/validate.go:200); allow-pattern warning; auto timeout 10 s (internal/tools/policy.go:29); confirm_on normalization (per agent fix report, not re-read in source).
- Cron: minute evaluation, 10-minute catch-up, warning, no restart replay, DST behavior (internal/cron/scheduler.go:101-171); `enabled` row and `deliver_to` validation for `cron run --deliver` (cron_cmd.go:120-128); timeout bounds manual run (cron_cmd.go:156-160).
- Storage: DSN `#?%` literal, mode 0600 (internal/store/sqlite/dialect.go:31, 213); doctor "Storage dir writable" (doctor_checks.go:39, 80-107); new Retention subsection (from fix report; `sessions rm` "no database yet" at internal/cli/root.go:189). VACUUM advice is documentation only, no feature promised.
- log.file validation (validate.go:364-369).

docs/security.md
- Deny-list summary matches defaults; git force-push rule scope and `git commit -m 'push +1'` not denied (deny_defaults.go:30-40, policy_test.go:274-307); allow-pattern `.` warning; setsid/Job Object limits (exec.go:289, exec_unix.go:18, exec_windows.go).
- New "What web_fetch refuses" section (internal/tools/web_fetch.go:25, 83, 94-118, 161-181, 220-222).
- Classifier 10 s timeout; redaction additions (internal/tools/approver.go:380-413); bounded separators; `refused_too_long` audit label (exec.go:229).

docs/architecture.md: three sqlite wiring sites (cli/root.go, cli/doctor_checks.go, gateway/gateway.go); cron enters the dispatcher not the queue (gateway.go:152, per audit; mermaid edge and step text); `decide` unexported; journals mention; reply-delivery guarantees (ordering dispatch.go:606-621, 5xx/429/incomplete notice telegram/send.go:23-48, 60 s client channel.go:35, /new /stop dispatch.go:311-353, refusal chat.go:157, content_filter agent/loop.go:280).

docs/verification.md: CI description corrected (push on main, test-stable, govulncheck job, release independent, golangci-lint ubuntu-only; ci.yml:4-6, 59-71, 119-155); new "Local checks and lint" subsection with install line; cron check routed to `cron list`; capture.go stale line range replaced by symbol.

docs/journals/2026-08-01-...md: one-line historical note.

Makefile: fmt-check comment (CI inlines gofmt). release.yml: pure-Go rationale now points at internal/store/sqlite/dialect.go; gate comment states it is a subset of CI. Comments only.

## Not documented (and why)
- Cancellation-note wording, malformed tool-call args handling, history-window widening, blank-line fenced-block merging, duplicate migration numbers: internal behavior with no operator decision; code and tests own them.
- Per-command flags `send --thread`, `sessions/approvals list --limit`: covered by the `--help` pointer per the controller decision.
- `onboard` temp-file+hard-link publish: mechanism detail, code-owned (onboard_cmd.go publishExclusive); nothing operator-visible except the existing no-overwrite guarantee.
- `onboard` exit 0 despite doctor FAIL rows: audit claim not re-verified; omitted.
- Release build differences (`-trimpath -s -w`), no local `go mod tidy` target, no draft release: observations, not doc defects.
- confirm_on normalization: taken from the agent report; I did not re-read the classifier source.
- Empty-arguments-as-`""` concern: open code question, not a doc matter.

## Validation
- `go test ./internal/config/ -run TestConfigFieldsAreDocumented -count=1`: PASS.
- All relative markdown links in README.md and docs/*.md resolve (script check).
- Line counts: README 189; architecture 220; configuration 198; security 300; telegram-setup 142; verification 242 (all under 800).
