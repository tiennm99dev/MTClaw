# internal/tools round-2 review fixes

Slice: `internal/tools/**`, `docs/security.md`. Source: code-reviewer
`plans/reports/code-reviewer-260921-1649-tools-policy-rereview.md`.

## Files Modified

- `internal/tools/approver.go` - C1, H2, L1, M1, M2
- `internal/tools/approver_test.go` - tests for C1, H2, L1, M1, M2
- `internal/tools/policy.go` - H1, H3
- `internal/tools/policy_test.go` - tests for H1, H3
- `internal/tools/exec.go` - L2 (comment), M4
- `internal/tools/exec_test.go` - test for M4's non-Windows regression guard
- `internal/tools/registry_test.go` - M3 test (through `tools.New`)
- `internal/tools/web_fetch.go` - L4
- `internal/tools/web_fetch_test.go` - test for L4

`docs/security.md` was read but not modified: every sentence the report
flagged (the "lightly normalized copy... quotes and a backslash directly
before the command word stripped" line, and the `sh -c`/`eval` "accepted,
documented bypass" line) already matches the fixed behavior once
`normalizeForDeny` is narrowed - restoring the code's scope makes the
existing doc text true again, no edit needed. Verified by
`TestDenyCorpus_ShellWrapperRemainsAnAcceptedBypass`.

## Fixes

**C1 (approval bypass, critical).** `TerminalApprover.lines` is now buffered
(cap 8) so `readLines` is never parked mid-send. `Ask` drains any
already-queued line non-blockingly *before* printing its own prompt, so a
"y" typed for a prompt that already timed out can never be silently applied
to the next, different prompt. Rewrote
`TestTerminalApprover_TimedOutAskDoesNotStealTheNextAnswer` to assert the
safe outcome (second Ask still times out); added
`TestTerminalApprover_AnswerTypedAfterPromptShownApproves` for the normal
case.

**H2 (EOF hang).** `readLines` now stores the terminal error under a mutex
and closes `t.lines` once, instead of sending once on an unbuffered `errs`
channel. Every `Ask` from then on - checked up front, and again if the
drain/select observes the closed channel - returns the same error
immediately. Fixed the stale comment claiming this was already true. Test:
`TestTerminalApprover_EOFFailsFastOnEveryAsk` asserts two consecutive Asks
after EOF both return in well under the (1-minute) timeout.

**H1 (allow-list widened across newlines).** `compileRules` now takes a
`deny bool` and only prefixes deny patterns with `(?s)`; allow patterns
compile as written. Test: `TestAllowRule_DoesNotSpanNewlines` proves
`^npm run .+$` no longer matches `"npm run build\ncat ..."`.

**H3 (deny normalization over-matching quoted arguments).**
`normalizeForDeny` now only unquotes/unescapes the command word at the
start of the string or right after a `;`/`&`/`|` separator
(`denyCommandWordQuote` regex), not quotes anywhere else in the command.
`'rm' -rf x`, `"rm" -rf x`, `\rm -rf x` still match; `grep "rm -rf" file`,
`grep -n 'rm -r' *.md`, `cat "my 'rm -rf' notes.txt"`,
`ls "/data/rm -rf backups"`, `python3 -c "print('rm -rf')"` no longer do -
all five added to `mustNotCatchPOSIX()`. As a side effect this also restores
the `sh -c '...'`/`eval "..."` documented bypass (pinned by
`TestDenyCorpus_ShellWrapperRemainsAnAcceptedBypass`), since the wrapper's
own quotes are outside the command-word position and are no longer touched.

**M1 (env-assignment redaction).** The boundary before the `KEY|TOKEN|...`
run is now "any non-identifier character or start of string"
(`(?i)(^|[^A-Za-z0-9_])...`) instead of a closed punctuation set, so
`?token=`/`&token=` inside a URL query string and `--api-key=` are caught
again. Tests: `TestRedactSecrets_URLQueryTokenAssignment`,
`TestRedactSecrets_APIKeyFlagAssignment`.

**M2 (base64 catch-all dropped `/`).** Replaced the single `\b[A-Za-z0-9+]
{32,}={0,2}\b` rule with `base64Like` + `redactBase64Like`: `/` is back in
the character class, but a candidate is skipped (left alone) if it starts
with `/`, is immediately preceded by `/`, or doesn't contain at least one
uppercase letter, one lowercase letter, and one digit. This redacts
`wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY` while a filesystem path (at the
start of the command or mid-command) still survives. Tests:
`TestRedactSecrets_AWSStyleSecretWithSlashIsRedacted`,
`TestRedactSecrets_PathAtStartOfCommandSurvivesRedaction`, plus the existing
`TestRedactSecrets_LongPathSurvivesRedaction` still passes.

**M3 (untested config-to-field wiring).**
`TestNew_ExecToolStripsConfiguredSecretEnvNamesFromChild` builds the exec
tool through `tools.New` (which calls `registerExecTool`) with
`openai.api_key_env`/`channels.telegram.token_env` naming real env vars set
via `t.Setenv`, then runs `exec` with an allow-all policy and asserts the
child's `echo $VAR` for both comes back empty. This exercises the real
`cfg.OpenAI.APIKeyEnv`/`cfg.Channels.Telegram.TokenEnv` -> `secretEnvNames`
wiring, not a hand-set field.

**M4 (`filterEnv` case-sensitivity).** Both the strip set and the
environment's variable names are folded to uppercase when
`runtime.GOOS == "windows"` before comparison; POSIX stays exact-match
(unchanged, still covered by the existing `TestExec_StripsSecretEnvVarsFromChild`
and the new `TestFilterEnv_ExactNameMatchStrippedOnAllPlatforms`, which also
pins that an unrelated variable containing the stripped name as a substring
survives). The Windows-specific branch cannot be exercised on this
Linux ARM64 runner; no build-tag test file was added since M4 did not
request one and it would need a signature change to be host-independent.

**L1 (`runeSafeLen` unbounded).** Backtracking is now capped at
`utf8.UTFMax-1` (3) bytes via a `limit` floor, so it can no longer erase an
entire non-UTF-8 buffer. Test: `TestRuneSafeLen_BoundedBacktrackOnInvalidUTF8`
with 900 `0xff` bytes asserts a non-empty result within 3 bytes of the
original length.

**L2 (`capWriter` goroutine-safety).** Added a comment at the `cmd.Stdout =
out; cmd.Stderr = out` call site in `exec.go` stating the os/exec
`interfaceEqual` dedup invariant this relies on, so a future edit that gives
`Stdout`/`Stderr` two separate `*capWriter` values (reintroducing a
concurrent-write race) is a visible contract break, not a silent one.

**L4 (Content-Type gate).** `isTextualContentType` now also accepts
`application/xml`, `application/javascript`, `application/xhtml+xml`
(already there), and any `application/*` subtype ending in `+json` or
`+xml` (RFC 6839, e.g. `application/vnd.api+json`, `application/atom+xml`).
Also documented that the gate runs after the body is fully downloaded (context
savings, not bandwidth/fetch-time savings). Test:
`TestWebFetch_AllowsXMLAndSuffixedAndJavaScriptContentTypes` covers all four
new shapes.

## Deviations / Judgment Calls

- The task text listed `echo 'git push --force'` alongside the H3
  must-not-catch strings. Verified this string already matches the
  `git push --force` deny rule via a **raw**, unmodified `cmd` substring
  match (`\bgit\s+push\b.*(--force...)\b` matches `"echo 'git push
  --force'"` directly, confirmed with a standalone `go run` probe) -
  independent of `normalizeForDeny` entirely, since that rule isn't anchored
  to the command-word position at all. Narrowing normalization (the only
  mechanism H3's fix touches) cannot change this outcome without widening
  scope into rewriting the git-push deny rule itself, which the fix
  description does not ask for and the review report never raised as a
  finding. Left as-is; flagging here rather than silently reinterpreting
  scope.

## Tests Status

- `gofmt -l internal/tools`: clean
- `go vet ./internal/tools/...`: clean
- `go build ./...`: clean (repo-wide)
- `go test -race -count=2 ./internal/tools/...`: **ok** (~17s combined)
- `git status --short internal/tools`: only the files listed above; no
  leftover probe files

## Unresolved Questions

1. Confirm the `echo 'git push --force'` deviation above is acceptable, or
   if the git-push deny rule itself should be anchored to the command-word
   position too (separate from this H3 fix's scope).

Status: DONE
Summary: Fixed C1/H2/H1/H3/M1/M2/M3/M4/L1/L2/L4 in internal/tools with targeted regex/state-machine changes and matching tests; docs/security.md needed no edits since its existing wording already matches the restored behavior. gofmt/vet/build clean, `go test -race -count=2 ./internal/tools/...` green, no probe files left behind.
Concerns/Blockers: One judgment call on an ambiguous corpus string (`echo 'git push --force'`) documented above for confirmation; otherwise no blockers.
