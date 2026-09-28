# internal/tools third-round review fixes

Executed phase: `internal/tools` (policy, exec, fs, web_fetch, approver, registry, redaction, doctor-adjacent shell resolution). Source: `plans/reports/code-reviewer-260928-0953-tools-policy-review.md`, binding decisions in `plans/260928-1041-third-round-review-fixes/plan.md`.

Status: completed. Every High/Medium finding is fixed with a regression test. Two Low items (part of B11) and one Low test gap (T3) are explicitly skipped with reasons below.

## Files modified

- `internal/tools/approver.go` - redaction tightening, display/audit split, control/bidi escaping, TerminalApprover field unexport, comment cleanup (+~150/-30 lines)
- `internal/tools/exec.go` - process-group kill unconditional, ErrWaitDelay handling, default secret-env fallback, `ResolveShell` export, ask() ctx-check simplification, capForAudit wiring (+~70/-25 lines)
- `internal/tools/exec_linux.go` (new) - `DisableEnvironRead` (PR_SET_DUMPABLE=0)
- `internal/tools/exec_notlinux.go` (new) - no-op counterpart
- `internal/tools/fs.go` - non-regular-file refusal (read/write), offset-past-EOF marker, rune-safe truncation, allocation sizing, dead-branch removal, per-tool ctx-check removal (R2), sort.Slice removal (+~35/-30 lines)
- `internal/tools/policy.go` - `ResolveShell` call site, widened deny-normalization separator class (+8/-6 lines)
- `internal/tools/registry.go` - centralized ctx-ended contract in `Registry.Run`, package doc cleanup (+15/-6 lines)
- `internal/tools/classifier.go` - comment accuracy fixes (phase/finding-code removal)
- `internal/tools/deny_defaults.go` - comment cleanup (plan/history references removed)
- `internal/tools/web_fetch.go` - injectable SSRF predicate, content-type gate moved before body read, HTML-only tag stripping, two residual SSRF ranges added
- Test files: `approver_test.go`, `exec_test.go`, `fs_test.go`, `policy_test.go`, `registry_test.go`, `web_fetch_test.go`, `path_guard_test.go` - new regression tests, removed tests for now-impossible/removed code paths, comment cleanup
- `internal/tools/fifo_unix_test.go` (new), `internal/tools/fifo_windows_test.go` (new) - FIFO test helper, platform-split
- `internal/cli/prompt_cmd_test.go` - **mechanical caller update**, see "Exported API changes" below
- `docs/security.md` - honest B4 wording, B1/B2/B5 documentation

## Findings: fixed / skipped

**B1 (High) - redaction swallowed shell metacharacters.** Fixed. `credentialValue` is now a fixed alphabet (`[A-Za-z0-9._~+/=:@%-]+`); every `redactPatterns` capture group uses it instead of `\S+`/`[^\s"']+`, so a match stops at the first metacharacter instead of consuming it. Test: `TestRedactSecrets_NeverSwallowsShellMetacharacters` (property test over `; | & $ ( ) < > \`` and whitespace, plus the exact probe commands from the report).

**B2 (High) - truncated approval preview / audit truncated at 800 bytes.** Fixed. `RedactSecrets` no longer truncates at all (verified by `TestRedactSecrets_NeverTruncates`). Two new functions: `displayCommand` (escapes, then refuses - `ok=false` - past `maxDisplayCommandLen` = 3500) and `capForAudit` (truncates at `maxAuditCommandLen` = 64 KiB, rune-safe). `execTool.ask` refuses before ever calling the approver when `displayCommand` reports not-ok, writing an `exec_audit` row with decision `refused_too_long`. Tests: `TestDisplayCommand_RefusesOverLongCommand`, `TestExec_ApprovalMode_OverLongCommandRefusedBeforeAsking`, `TestCapForAudit_TruncationIsRuneSafe`.

**B3 (Medium) - backgrounded child misreported as failure, orphaned.** Fixed both halves of the binding decision. `execute()` now calls `killProcessTree(cmd)` unconditionally right after `cmd.Run()` returns (previously only on ctx cancellation/timeout via `cmd.Cancel`), so a backgrounded child never outlives the call. `errors.Is(runErr, exec.ErrWaitDelay)` is now a normal-completion case (verified against Go's own `os/exec` source: ErrWaitDelay only replaces a nil/success result, never an `*ExitError`, so `cmd.ProcessState.ExitCode()` is always 0 there) - it reports `exit_code: 0` with the captured output and a `status:` note, instead of `"exec: command failed to run: ... WaitDelay expired"` with the output thrown away. Test: `TestExec_BackgroundedChildDoesNotOutliveCallAndExitStatusIsReported` (uses a 4s child sleep against the 3s `execWaitDelay`, asserts exit code, captured "started" output, elapsed time actually reached WaitDelay, and the marker file the child would have written never appears).

**B4 (Medium) - secret-env stripping weaker than documented.** Fixed both requested parts. `registerExecTool` now strips `OPENAI_API_KEY`/`TELEGRAM_BOT_TOKEN` even when `api_key_env`/`token_env` are explicitly empty (`envNameOrDefault`, mirroring `config`'s own fallback). Added `tools.DisableEnvironRead()` (Linux-only, no-op elsewhere per `exec_linux.go`/`exec_notlinux.go`) which sets `PR_SET_DUMPABLE=0` via `syscall.Syscall(syscall.SYS_PRCTL, ...)` - no new dependency, `syscall.SYS_PRCTL`/`syscall.PR_SET_DUMPABLE` are already defined on linux/amd64 and linux/arm64. **Not wired up anywhere yet** - per the plan, "the later cli agent will call it at gateway/prompt startup"; I only export it. `docs/security.md` rewritten to state plainly that `filterEnv` only governs the direct child's own environment (a same-uid process can still read `/proc/<pid>/environ` on Linux; `DisableEnvironRead` is what closes that, and only once actually invoked at startup) and that the default `bash -lc` login shell re-sources profile files that may re-export the same variable. Test: `TestNew_ExecToolStripsDefaultSecretEnvNamesWhenConfigLeavesThemEmpty`.

**B5 (Medium) - control/bidi characters shown as-is.** Fixed. `escapeControlAndBidi` (called from `displayCommand`) escapes C0 controls (except `\n`/`\t`), DEL, C1 controls, and U+202A-202E/U+2066-2069 as `\xNN`/`\uNNNN`. Applied only to the display copy (`Request.Command`), never to the executed command or the audit-stored copy. Tests: `TestEscapeControlAndBidi_EscapesControlCharsButKeepsNewlineAndTab`, `TestEscapeControlAndBidi_EscapesBidiOverrides`.

**B6 (Medium) - web_fetch stripped HTML from non-HTML text.** Fixed. New `isHTMLContentType` gates `htmlToText`: only `text/html`/`application/xhtml+xml` go through it now; everything else (including an absent header) is returned verbatim. Tests: `TestWebFetch_NonHTMLTextualTypesAreReturnedVerbatim` (asserts the literal body substring survives, not just a loose word), `TestWebFetch_HTMLContentTypeStillStripsTags`.

**B7 (Medium) - read_file/write_file hang on FIFO/device.** Fixed for both tools. `read_file` now `os.Stat`s before `os.Open` and refuses anything not `Mode().IsRegular()`. `write_file` stats the resolved path before `os.MkdirAll`/`os.OpenFile` and refuses an existing non-regular target. Windows reserved device names (CON, COM1, ...) are not separately special-cased - I rely on `os.Stat`'s own mode reporting, which the report itself only partially trusts ("on older Windows versions also NUL" per the source report); not independently verified since there is no Windows runtime available here. Tests: `TestReadFile_RefusesNonRegularFile`, `TestWriteFile_RefusesExistingNonRegularTarget` (both build a real FIFO via `syscall.Mkfifo`, POSIX-only via `fifo_unix_test.go`/`fifo_windows_test.go` split, and fail the test if the call blocks more than 3s).

**B8 (Low) - deny normalization missed newline/`(`/`{`/`$(` positions.** Fixed. `denyCommandWordQuote`'s separator class widened from `(^|[;&|]\s*)` to `(^|[;&|({\n]\s*|\$\()`. All four must-catch strings from the report added to `mustCatchPOSIX()` in `policy_test.go`.

**B9 (Low) - ask() classified cancellation by askErr instead of ctx.Err().** Fixed, but by the R2 refactor rather than a local branch: `ask()` no longer special-cases `context.Canceled` at all; `Registry.Run` now uniformly turns any `(result, nil)` return into `(result, ctx.Err())` when ctx has already ended, which correctly covers both `context.Canceled` and `context.DeadlineExceeded` regardless of what the approver's own error says. Test: `TestRegistry_Run_TurnDeadlineDuringApprovalSurfacesAsGoError` (a `blockingApprover` returns `ctx.Err()` when a turn's own `context.WithTimeout` - not `WithCancel` - expires; asserts the error surfaces as `context.DeadlineExceeded` through `Registry.Run`).

**B10 (Low) - read_file edge cases.** All three fixed. Offset past EOF (or at EOF for a nonzero-size file) now returns `"read_file: offset N is past end of file (size M)"` instead of `""`; offset 0 against a genuinely empty file still returns `""` (not treated as an error). Truncation now runs the same `runeSafeLen` exec output already uses. Allocation now uses `min(limit, size-offset)` instead of always allocating the full `max_read_bytes`. The `limit <= 0` defense-in-depth branch was deleted along with its test (see R6/T4). Tests: `TestReadFile_OffsetPastEOFReturnsExplicitMarker`, `TestReadFile_OffsetAtExactEOFReturnsExplicitMarker`, `TestReadFile_EmptyFileAtOffsetZeroReturnsEmptyNotAMarker`, `TestReadFile_TruncationIsRuneSafe`.

**B11 (Low) - residual web_fetch SSRF gaps.** Partially fixed. Added `240.0.0.0/4` (folds in the existing `255.255.255.255` case) and IPv4-compatible IPv6 (`::a.b.c.d`) embedding, both one-line-cost additions per the report. **Skipped: Teredo (`2001::/32`) and `64:ff9b:1::/48`.** Teredo embeds the client IPv4 XOR'd with `0xFFFFFFFF` plus a port field, not a plain embed like NAT64/6to4/IPv4-compatible; `64:ff9b:1::/48`'s embedding per RFC 6052 inserts a reserved "u" octet at a fixed offset for any prefix length other than `/96`, so it isn't a simple byte-slice recurse either. Both are non-trivial compared to the "one line" ranges, and the source report itself frames this whole item as "these are practically unroutable on Linux; listed only so the gap is recorded rather than forgotten" - recording, not requiring, the fix. Tests: added cases to `TestIsBlockedAddr_ResidualRanges`.

**B12 (Low) - minor inconsistencies.** All fixed. `list_dir`'s dual message-and-error return on cancel removed (subsumed by R2). `docs/security.md`'s "long base64/hex runs" now says the mix-of-cases requirement explicitly (an all-lowercase hex string, e.g. a git SHA, is deliberately left alone). `classifier.go`'s "New in registry.go resolves..." now correctly says `registerExecTool` in `exec.go`. `classifier.go`'s "Classify applies classifierTimeout" now correctly attributes that to `Policy.evaluateAuto`. `approver.go`'s stray `approvalTimeoutMessage` comment now matches the actual const name `approvalPromptFooter`.

**T1 (Medium test gap) - phantom redirect tests.** Fixed via R3. `newSafeHTTPClient` now takes an injectable `func(netip.AddrPort) bool` predicate; production passes `productionBlockedAddr` (wraps `isBlockedAddr`, ignoring the port). `TestWebFetch_FollowsRedirectThenRefusesDifferentTarget` (renamed from `TestWebFetch_RedirectToLocalhostRefused`) now uses the real client with a predicate that allows only the redirecting server's port, so `CheckRedirect` and `Control` both actually run against real loopback servers; hit counters on both servers prove the first hop was dialed and the second one was not. `TestWebFetch_TooManyRedirectsRefused` similarly uses the real client (predicate: block nothing) and asserts the exact hit count (`maxRedirects`, not merely a substring match).

**T2 - missing tests for B1/B2/B3/B7/B6.** All added, listed under their respective finding above.

**T3 (Low, unverified on Windows) - `childSurvivalScript`'s `Start-Job` doesn't prove tree-kill on Windows.** Skipped. The suggested fix (a detached `Start-Process` child) is a behavior change to a Windows-specific code path I cannot run or verify on this Linux ARM64 host; blindly changing it risks silently breaking the one signal that test currently gives (a passing run) without being able to confirm the replacement actually discriminates correctly. Left as documented, pre-existing, Low-severity gap.

**T4 - dead `limit <= 0` branch tests unreachable code.** Fixed by deletion, per R6: the branch (and `TestReadFile_NonPositiveMaxReadBytesRefusesInsteadOfPanicking`) is removed. `config.Validate` already rejects `max_read_bytes < 1` whenever `tools.filesystem.enabled`, and `fsTools` is only ever constructed via `registerFilesystemTools` from a validated config in production; the branch could only ever be reached by a test bypassing that construction path, exactly what the deleted test did.

**R1-R7 refactors.** All implemented:
- R1: `RedactSecrets`/`displayCommand`/`capForAudit` split (see B1/B2/B5).
- R2: `Registry.Run` centralizes the ctx-ended contract; removed the now-redundant per-tool checks at `fs.go` (readFile/writeFile entry checks, `list_dir`'s dual return) and `exec.go`'s `ask()` cancellation branch. `web_fetch.go` needed no code change - its existing `(msg, nil)` returns on a canceled `client.Do` are now caught by the same centralized rule.
- R3: injectable SSRF predicate (see T1).
- R4: Content-Type is now checked before `io.ReadAll`, so a binary response never spends `max_bytes` of bandwidth/memory before being discarded.
- R5: `resolveShell` exported as `tools.ResolveShell`. **Not yet used by `internal/cli/doctor_checks.go`** - the plan states that switch is the later cli agent's job ("the cli agent will switch doctor later"); I did not touch `doctor_checks.go`.
- R6: `sort.Slice` removed from `walkDir` (comment explains `os.ReadDir` is already sorted); the redundant `context.WithoutCancel(ctx)` wrap at the one `execute()` call site removed (`writeAudit` already detaches internally); the dead `limit <= 0` branch and its test removed (T4); `TerminalApprover.In`/`Out`/`Timeout` unexported to `in`/`out`/`timeout`.
- R7: removed plan-file/phase/finding-code references and historical narration from `registry.go` (package doc), `deny_defaults.go`, `classifier.go`, `approver.go`, and matching cleanup in test file comments/names across `exec_test.go`, `approver_test.go`, `policy_test.go`, `path_guard_test.go`, `web_fetch_test.go` (e.g. `TestDenyCorpus_BothBypassesFromRedTeam` -> `TestDenyCorpus_RmRulesCatchLongFlagsAndPathPrefixedForms`).

## Exported API changes

- `tools.RedactSecrets(cmd string) string` - unchanged signature, changed behavior: no longer truncates (see B2/R1). No caller outside `internal/tools` invokes it directly (checked via grep); the Telegram/gateway approvers only ever read `Request.Command`, which is already the fully-processed display string.
- `tools.Request.Command` - contract unchanged (still "a display-safe command string plus reason", per the task's instruction to keep this clear for the Telegram HTML rewrite), but the string it now carries is redacted *and* control/bidi-escaped, and is refused (never delivered) past ~3500 chars instead of silently truncated at 800. The Telegram approver's `<pre>`-block rendering (being written concurrently) should be unaffected: it still receives one opaque, already-safe-to-display string.
- `tools.ResolveShell(configured []string) []string` - new export (was unexported `resolveShell`). Additive; nothing broke.
- `tools.DisableEnvironRead() error` - new export (Linux: sets `PR_SET_DUMPABLE=0`; elsewhere: no-op). Additive; not yet called from anywhere.
- `tools.TerminalApprover.In`/`Out`/`Timeout` -> unexported `in`/`out`/`timeout`. **Breaking.** The only outside reference was `internal/cli/prompt_cmd_test.go`, which read these three fields directly. Per my delegation's explicit allowance ("minimal mechanical caller updates in internal/cli... where your exported API changes break compilation"), I rewrote that one test to assert the same wiring behaviorally (feed `"y\n"` into the command's stdin, call `Ask`, assert it approves and that the prompt landed on the command's stderr) instead of reading private fields. This is the only edit I made outside `internal/tools`/`docs/security.md`.
- New exec_audit decision label `"refused_too_long"` (see B2). `internal/cli/approvals_cmd.go`'s `decider()` switch has no case for it and falls through to `"unknown"` - functionally harmless (a label, not a bug), but the cli owner may want to add a case mapping it to `"policy"` in phase 2. I left that file untouched since it's not a compilation break and is outside my ownership.

## Tests status

- `gofmt -l .`: clean.
- `go vet ./...`: clean (whole repo).
- `go test -race -count=1 -cover ./internal/tools/...`: **ok, 89.4% coverage** (up from 87.9%).
- `go test -race -count=1 ./internal/cli/...`: ok (covers the one mechanical caller-update file).
- `go test -count=1 ./...` (whole repo, no race flag, for a quick full-repo sanity check): all packages pass, including `internal/agent`, `internal/channel/telegram`, `internal/gateway`, `internal/cron` - the other agents' concurrent work is not broken by anything here as of this run.
- `CGO_ENABLED=0 GOOS=windows go build ./...`: clean.
- `CGO_ENABLED=0 GOOS=darwin go build ./...`: clean.
- `GOOS=windows go vet ./internal/tools/...` / `GOOS=darwin go vet ./internal/tools/...`: clean (extra check since I added `exec_linux.go`/`exec_notlinux.go`/`fifo_windows_test.go` build-tag-split files).
- `go mod tidy -diff`: clean (no new dependencies added; `PR_SET_DUMPABLE` used via the plain `syscall` package, not `golang.org/x/sys/unix`).

## Notes on the B1 test-writing process

While drafting the bidi-escape test I discovered the tool-call JSON transport interprets a literal `\uXXXX` sequence in a parameter string as the actual Unicode codepoint before Go ever sees it as source text (so writing `"‮"` in a tool call embeds the raw RTL-override character into the file, not the 6-character text). Worked around it by constructing the test's expected/actual runes via `string(rune(0x202E))` etc. in Go source instead of literal escape sequences in the edit payload - flagging this here in case another agent hits the same surprise.

## Unresolved questions / follow-ups for later phases

1. `tools.DisableEnvironRead()` is exported but not called anywhere yet - needs to be wired into gateway/prompt startup by the cli-owning phase, per the plan.
2. `internal/cli/doctor_checks.go`'s `defaultShellArgv()` still duplicates the shell-default logic now available as `tools.ResolveShell` - the plan defers this switch to the cli phase.
3. `internal/cli/approvals_cmd.go`'s `decider()` has no case for the new `"refused_too_long"` exec_audit decision label (falls through to `"unknown"`, which is not a bug, just less informative than it could be).

Status: DONE
Summary: All High/Medium findings (B1-B7, T1) fixed with regression tests; all Low findings fixed except two documented SSRF sub-gaps (Teredo, 64:ff9b:1::/48) and the Windows-only test-gap T3, both skipped for stated reasons; all seven refactors (R1-R7) implemented; `gofmt`/`go vet`/`go test -race`/windows+darwin cross-builds all clean.
