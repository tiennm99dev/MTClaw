# internal/tools review-fix implementation report

Scope: `internal/tools/*.go`, `docs/security.md`, `go.mod`/`go.sum`. Source
review: `plans/reports/code-reviewer-260921-1539-tools-policy-security-review.md`.

## Files changed

- `internal/tools/exec.go` - removed the go-shellwords tokenize gate; added
  `capWriter` (write-time output cap) and `filterEnv` (child env secret
  stripping); rune-safe trim on truncated output.
- `internal/tools/approver.go` - `TerminalApprover` now owns one
  `bufio.Reader` and one long-lived reader goroutine (started in
  `NewTerminalApprover`) instead of one goroutine per `Ask`; fixed
  `RedactSecrets`' env-assignment regex and dropped `/` from the base64
  catch-all; added the shared `runeSafeLen` truncation helper.
- `internal/tools/policy.go` - `compileRules` now compiles with a `(?s)`
  prefix; `Evaluate` also matches a quote/backslash-normalized copy of the
  command against every deny rule (`normalizeForDeny`).
- `internal/tools/deny_defaults.go` - the two `rm` deny patterns' leading
  boundary group now also accepts `(` directly before the command, so a
  subshell wrapper (`(rm -rf ~) &`) cannot slip past deny once the tokenize
  gate is gone.
- `internal/tools/fs.go` - `read_file` refuses instead of panicking when
  `maxReadBytes` is non-positive; `read_file`/`write_file`/`list_dir` now
  accept and check `ctx`, and `walkDir` bails out on cancellation mid-walk.
- `internal/tools/web_fetch.go` - added the residual SSRF ranges (limited
  broadcast, `192.0.0.0/24`, `198.18.0.0/15`, NAT64 `64:ff9b::/96` and 6to4
  `2002::/16` with the embedded IPv4 re-checked through the full blocklist);
  added a Content-Type gate that skips non-text bodies instead of running
  them through `htmlToText`.
- `docs/security.md` - flowchart and text no longer show PARSE ahead of
  DENY; documented that `sh -c` / `eval` wrappers remain an accepted bypass;
  documented the widened `RedactSecrets` env-assignment coverage; documented
  child-process env scrubbing.
- `*_test.go` in the same package - new/updated tests, see below.
- `go.mod` / `go.sum` - `github.com/mattn/go-shellwords` removed via
  `go mod tidy` (verified stable across two consecutive runs).

## Per-task notes

**1 (H1).** `run` now always calls `e.policy.Evaluate(ctx, rawCmd)`; no step
runs ahead of the deny-list. Confirmed `TestExec_TimeoutKillsWholeProcessTree`
and `TestExec_TurnCancellationKillsProcessTree` now exercise the real
allow-list path (they use `Allow: [".*"]`, previously dead due to the gate)
and pass. Added `TestExec_SubshellWrappedDenyCommandIsRefusedNotAsked`
asserting `(rm -rf ~) &` is `VerdictRefuse` and never reaches the approver;
this required extending the `rm` deny patterns' boundary group to accept a
leading `(` (documented inline in `deny_defaults.go`), since the tokenize
gate had been masking that the raw regex never matched a paren-prefixed
command either. Renamed the old tokenize-specific test to
`TestExec_UnusualSyntaxStillGoesThroughDenyThenApprover` (behavior unchanged:
under `approval` mode with no deny/allow match, unusual syntax still asks).
Flowchart in `docs/security.md` edited to start at DENY.

**2 (H2).** `capWriter` claims every write in full (`return len(p), nil`)
and only retains bytes up to `MaxOutputBytes`, so memory is bounded by that
cap regardless of how much the child writes or how long it runs before its
timeout fires. `truncated` is `capWriter.over`. Test
`TestExec_OutputCappedAtWriteTimeNotAfter` runs `yes | head -c 2000000`
against a 100-byte cap and asserts the returned output body length is
`<= 100`.

**3 (H3).** New env-assignment regex
`(?i)(^|[\s;&|"'=])([A-Za-z0-9_]*(?:KEY|TOKEN|SECRET|PASSWD|PASSWORD))=(\S+)`
matches by suffix so `API_KEY=`, `GITHUB_TOKEN=`, `DB_PASSWORD=`,
`AWS_SECRET_ACCESS_KEY=`, `PASSWD=` are all caught, not just the bare
keyword. Base64 catch-all class dropped `/`, so
`ls /workspace/tiennm99dev/MTClaw/internal/tools` survives unredacted (no
path segment reaches 32 chars without a `/`). All 4 existing `RedactSecrets`
tests plus the pre-existing `PASSWORD=` one still pass unmodified.

**4 (H4).** `TerminalApprover` starts its single reader goroutine in
`NewTerminalApprover`; `Ask` selects on `t.lines`/`t.errs` (populated by that
one goroutine for the approver's whole life) instead of spawning a new
reader per call. Test `TestTerminalApprover_TimedOutAskDoesNotStealTheNextAnswer`
uses `io.Pipe`: first `Ask` times out, then `"y\n"` is written, then a
second `Ask` on the same approver receives and approves it.

**5/6 (M1/M2).** `compileRules` prefixes every pattern with `(?s)`. `Evaluate`
also matches `normalizeForDeny(cmd)` (quotes stripped, backslash-before-
word-char stripped) against every deny rule. Added to the must-catch corpus:
the three line-continuation strings from the report, plus `'rm' -rf
/home/me` and `\rm -rf /home/me`. `docs/security.md` states `sh -c '...'` /
`eval` wrappers remain an accepted, undocumented-no-longer bypass.

**7 (M3).** `registerExecTool` collects `cfg.OpenAI.APIKeyEnv` and
`cfg.Channels.Telegram.TokenEnv` (whichever are non-empty) into
`execTool.secretEnvNames`; `execute` sets `cmd.Env =
filterEnv(os.Environ(), e.secretEnvNames)`. Test
`TestExec_StripsSecretEnvVarsFromChild` sets a var via `t.Setenv`, points
`secretEnvNames` at it directly (same-package field access, no need to
route through full config), and asserts `echo $VAR` returns empty inside the
child while the process's own env is untouched. One sentence added to
`docs/security.md`.

**8 (M6).** Single generic-free helper `runeSafeLen(b []byte) int` in
`approver.go` (backward `utf8.DecodeLastRune` trim, bounded to at most 3
iterations) used both for the `RedactSecrets` display cap and for exec's
capped output. Does not import `internal/channel`; independently duplicated
from `chunk.go`'s intent, not its exact algorithm, because the exec-output
site has no lookahead byte available after `capWriter` discards overflow.
Tests: `TestRedactSecrets_TruncationIsRuneSafe`,
`TestExec_OutputCapIsRuneSafe` (cap of 10 against a 3-byte-per-rune string).

**9 (fs.go).** `read_file` returns a result string (not a panic) when
`maxReadBytes <= 0` after the existing clamp. `read_file`/`write_file`/
`list_dir` accept `ctx` and check `ctx.Err()` at entry (cheap, matches the
tool's own single-syscall-scale cost); `walkDir` additionally checks
`ctx.Err()` at the top of its recursion and inside its entry loop so a large
recursive listing can be aborted mid-walk, not just at the top-level call.
Tests: `TestReadFile_NonPositiveMaxReadBytesRefusesInsteadOfPanicking`,
`TestReadFile_CanceledContextReturnsErrorImmediately`,
`TestListDir_CanceledContextReturnsErrorImmediately`.

**10 (L1/L2).** `isBlockedAddr` now covers `255.255.255.255`,
`192.0.0.0/24`, `198.18.0.0/15` directly (`isBlocked4`), and recurses on the
embedded IPv4 for NAT64 `64:ff9b::/96` and 6to4 `2002::/16` addresses
through the *full* `isBlockedAddr` (not just `isBlocked4`), so an embedded
loopback/private address is also caught, not only the IETF-reserved ranges.
`isTextualContentType` allows `text/*`, `application/json`,
`application/xhtml+xml` (absent header still treated as text, unchanged
prior behavior); anything else returns `web_fetch: binary content skipped
(content-type: "...", N bytes)` instead of running through `htmlToText`.
Tests: `TestIsBlockedAddr_ResidualRanges` (9 cases including two "public
IPv4 embedded in v6 must NOT be blocked" negatives),
`TestWebFetch_SkipsNonTextContentType`, `TestWebFetch_AllowsJSONContentType`.

**11 (M4/M5).** Left untouched as instructed - fs write auditing and
`exec_audit` requester columns are product/schema decisions, not implemented.

## Tests added (new test functions)

`exec_test.go`: `TestExec_UnusualSyntaxStillGoesThroughDenyThenApprover`
(renamed from tokenize-specific), `TestExec_SubshellWrappedDenyCommandIsRefusedNotAsked`,
`TestExec_OutputCappedAtWriteTimeNotAfter`, `TestExec_OutputCapIsRuneSafe`,
`TestExec_StripsSecretEnvVarsFromChild`.

`approver_test.go`: `TestRedactSecrets_APIKeyAssignment`,
`TestRedactSecrets_SuffixedEnvAssignmentForms`,
`TestRedactSecrets_LongPathSurvivesRedaction`,
`TestRedactSecrets_TruncationIsRuneSafe`,
`TestTerminalApprover_TimedOutAskDoesNotStealTheNextAnswer`.

`policy_test.go`: 5 new entries in `mustCatchPOSIX()` (H1 subshell case,
3 line-continuation cases, 2 quote/backslash cases).

`fs_test.go`: `TestReadFile_NonPositiveMaxReadBytesRefusesInsteadOfPanicking`,
`TestReadFile_CanceledContextReturnsErrorImmediately`,
`TestListDir_CanceledContextReturnsErrorImmediately`.

`web_fetch_test.go`: `TestIsBlockedAddr_ResidualRanges`,
`TestWebFetch_SkipsNonTextContentType`, `TestWebFetch_AllowsJSONContentType`.

## Verification

- `gofmt -l internal/tools` - empty.
- `go vet ./internal/tools/...` - clean.
- `go build ./...` (whole repo) - clean at the time of this run.
- `go test -race -count=1 ./internal/tools/...` - all pass (including the
  two previously-dead process-tree kill tests, now genuinely exercised).
- `go mod tidy` - stable across two consecutive runs; `go-shellwords` fully
  removed from `go.mod`/`go.sum`, no other diff.

## Skipped / out of scope

- M4 (fs write audit trail) and M5 (`exec_audit` requester columns) - not
  implemented, per explicit instruction; both are schema/product decisions.
- L3 (web_fetch echoes credentials in the URL verbatim) - not in the
  assigned task list (only L1/L2 were), left untouched.
- Windows-specific findings (L7 taskkill grandchild gap) - not in scope,
  untouched.

## Unresolved questions

None blocking. One judgment call worth flagging: the H1 regression test
required widening the two `rm` deny patterns' leading-boundary character
class to also accept `(` (a subshell open-paren), which is a small change to
`deny_defaults.go` beyond exactly what the H1/M1/M2 task text enumerated by
name - it was necessary because, absent it, `(rm -rf ~) &` was never caught
by the deny-list even with the tokenize gate removed (verified by direct
regex testing before touching the file). Documented inline; full corpus
(must-catch, must-not-catch, accepted-false-positives) re-verified green
after the change.

Status: DONE
Summary: All 10 assigned tasks implemented with tests; build/vet/gofmt/race clean; go-shellwords removed via stable go mod tidy; M4/M5 correctly left untouched.
Concerns/Blockers: None. One documented judgment call: extended the rm deny patterns' boundary class to accept a leading "(" so the H1 regression test (`(rm -rf ~) &` must be REFUSED) actually holds once the tokenize gate is gone - see "Unresolved questions" above.
