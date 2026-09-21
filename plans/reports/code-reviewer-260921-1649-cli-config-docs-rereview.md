# Re-review: CLI / config / logging / docs / CI (uncommitted fix pass)

Slice: `main.go`, `internal/cli/**`, `internal/config/**`, `internal/logging/**`,
`internal/version/**`, `README.md`, `docs/**`, `Makefile`, `.github/workflows/*`.
Base: `7927b6d` + uncommitted fix pass. Advisory only, no files changed.

## Checks run

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test -race -count=2 ./internal/cli/... ./internal/config/... ./internal/logging/...` | all pass |
| `CGO_ENABLED=0 go test ./...` (new CI gate) | all pass |
| `go mod tidy -diff` | clean (the implementer's "go-shellwords removable" note is stale) |
| CLI smoke (`version`, `config validate` bad value, `config show`, `prompt --help`, `cron run --help`) | all behave as documented |
| Differential probe vs. a binary built from `HEAD` (`git archive` into scratchpad) | see C1 |

---

## Critical

### C1. `mtclaw onboard` with a closed stdin is now an unkillable, disk-filling infinite loop

Two parts, one of which is a regression from this pass.

`internal/cli/onboard_cmd.go:184-192`:

```go
var model string
for model == "" {
    model, err = p.Text("Model (e.g. gpt-4o, gpt-4o-mini); there is no built-in default", "")
    if err != nil { return err }
    if model == "" { p.Printf("a model is required.\n") }
}
```

`internal/cli/onboard_prompts.go:103-108` swallows EOF:

```go
func (p *stdioPrompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}
```

At EOF `readLine` returns `("", nil)` forever, so the loop spins. **Measured: `mtclaw onboard < /dev/null` wrote 2.9 GB of "a model is required." to stdout in ~2 minutes.** The loop itself pre-dates this pass.

The regression is that it can no longer be stopped. `internal/cli/root.go:49`:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
```

Registering a handler disables Go's default "die on SIGINT/SIGTERM", and nothing in
the onboard prompt path ever observes `ctx`. Differential probe, same input, same
machine:

```
mtclaw-head (HEAD): SIGTERM stopped it
mtclaw    (fix pass): infinite loop SURVIVES SIGTERM
```

Same for a blocked (not looping) prompt: SIGTERM to `onboard` waiting on a fifo —
`HEAD` exits, the new binary does not. In a terminal, Ctrl-C behaves identically;
the only recovery is `kill -9` from another terminal while the disk fills.

Realistic triggers: `mtclaw onboard` under a systemd unit / CI step / `nohup` /
any `< /dev/null`, or a user pressing Ctrl-D at the model prompt.

Fix (all three are cheap, take at least the first two):

1. `onboard_prompts.go` — propagate EOF: `if err == io.EOF && line == "" { return "", err }`
   (or return `io.EOF` and let `runOnboard` abort with "stdin closed"). This also
   stops the silent "EOF accepts every default" path that otherwise writes a config
   file non-interactively.
2. `root.go Execute` — restore the standard second-signal escape hatch:

   ```go
   ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
   defer stop()
   go func() { <-ctx.Done(); stop() }() // a repeat signal now hard-kills
   ```

   Note this only fully works once `gateway`'s own nested `notifyContext`
   (`internal/gateway/gateway.go:171`, `shutdown.go:25-27`) is removed — it is now
   redundant (`gw.Run(cmd.Context())` already receives a signal-cancelled ctx) and
   it keeps a handler registered that suppresses the hard kill for the `gateway`
   leg. Recommend deleting it in the gateway slice and keeping signal handling in
   exactly one place.
3. Bound the model loop (e.g. 3 attempts, then error).

---

## High

### H1. Docs and a doctor check claim an `exec.cwd` confinement that exists nowhere

`docs/configuration.md:106` (new text):

> Not checked against `tools.filesystem.roots` at load time ... **The real, symlink-aware confinement is enforced at call time by `tools.Resolve`.**

`internal/cli/doctor_checks.go:294-297` repeats it:

> config.Validate does not check this at all - exec.cwd is documented as "not a jail", and the real confinement to tools.filesystem.roots is enforced at call time by tools.Resolve, symlink-aware, when the exec tool actually runs.

This is false. `internal/tools/exec.go:210` is the only consumer:

```go
cmd.Dir = e.cfg.CWD
```

No `Resolve`, no root comparison, at any point. `tools.Resolve` guards
*filesystem-tool* path arguments, not exec's working directory. Removing the
load-time rule (prior M2, an accepted decision) means `tools.exec.cwd` is now
compared against `tools.filesystem.roots` **nowhere in the product** — the fix
pass replaced a real rule with a claim about a mechanism that does not exist.

Also `doctor_checks.go:46` still names the row `"exec.cwd inside a root"` while
`checkExecCWD` only stats the directory — user-visible output that asserts a
check it does not perform.

Fix: drop the "enforced at call time by tools.Resolve" sentence from both places
(say plainly: exec's cwd is unconstrained by design, the deny-list is the only
boundary — consistent with `docs/security.md`), and rename the doctor row to
`"exec.cwd exists"`.

### H2. The H2 signal fix ships with no test, and the store-close path is the only thing it was reasoned about

`internal/cli/root.go:49-56` is untested: `root_test.go` covers command
registration and `version`, never signals. Nothing pins "SIGINT cancels the turn
so `agent.Loop` flushes the partial transcript", which was the entire motivation.
Both C1 and the `cron run` bookkeeping gap (M2 below) got through because of this.

Suggested minimal test: a fake command whose `RunE` blocks on `cmd.Context()`,
driven through `ExecuteContext` with a manually-cancelled context, plus one
`syscall.Kill(os.Getpid(), syscall.SIGTERM)` test behind a `testing.Short()` guard.

---

## Medium

### M1. "Read-only commands create nothing on disk" is only half true — `logging.New` still writes

`internal/cli/root.go prepare()` calls `logging.New` for every non-exempt command,
and `internal/logging/logger.go:23,26` creates the log directory **and an empty log
file**. Verified with the built binary on a config whose `log.file` points at a
non-existent tree:

```
$ mtclaw --config probe/config.yaml config validate
OK: probe/config.yaml
drwxr-xr-x probe/logs                 <- created
-rw------- probe/logs/mtclaw.log      <- created (empty)
(probe/state/nested/ was NOT created - the M1 storage fix does hold)
```

So the prior review's concrete complaint ("pointing `--config` at someone else's
config to inspect it silently creates directories wherever that file points")
still reproduces, just through `log.file` instead of `storage.path`. Fix: open the
log file lazily on first write, or skip logger construction for the pure-inspection
commands (`config show`, `config validate`, `config path`).

### M2. An interrupted `cron run` now silently loses its `cron_runs` row

`internal/cli/cron_cmd.go:145` passes the (now cancellable) command ctx:

```go
recordManualRun(ctx, cmd, st.CronRuns(), job.Name, sess.ID, result.Err)
```

and `cron_cmd.go:200-203` still asserts the old world:

> A store failure is a warning, not a command failure: the turn itself already ran to completion by the time this is called.

After Ctrl-C, `ctx` is done, `runs.Append(ctx, …)` fails immediately, and the run
is recorded nowhere — the user gets `warning: record cron run: context canceled`.
The adjacent ephemeral-session cleanup already got this right
(`cron_cmd.go:136`, `context.Background()`). Fix: `context.WithoutCancel(ctx)` in
`recordManualRun` (and update the comment).

### M3. `ensureDirCreatable`'s writability test reads the wrong bit

`internal/config/load.go:204`:

```go
if runtime.GOOS != "windows" && info.Mode().Perm()&0o200 == 0 {
    return fmt.Errorf("%s is not writable", d)
}
```

It tests the **owner** write bit irrespective of who owns the directory:

- False reject: a group-writable shared state dir (`root:mtclaw`, mode `0070`) is
  writable by this process but fails validation, so the config will not load at all.
- False accept: a root-owned `0755` parent passes validation, then `sqlite.Open`'s
  `MkdirAll` fails at run time anyway.

Since the fallible case is handled properly downstream either way, the simplest
fix is to drop the permission heuristic entirely (keep only the "exists and is not
a directory" check) or use `golang.org/x/sys/unix.Access(d, unix.W_OK)`.
`TestValidate_StorageParentDirNotWritable` passes under either the old `MkdirAll`
or the new stat walk, so it does not pin this behavior.

### M4. `storage.path: ""` still passes validation and silently discards all data

`internal/config/validate.go:288-293` only checks the parent dir. Verified:

```
$ mtclaw --config empty.yaml config validate
OK: .../empty.yaml
$ mtclaw --config empty.yaml sessions rm nope
Error: session nope not found      # opened a throwaway anonymous SQLite db
```

`filepath.Dir("")` is `"."`, which always exists, so the empty path sails through;
SQLite then opens a private temporary database and every session/message/audit row
is lost on exit with no error anywhere. Pre-existing, but `validateStorage` is the
exact function this bounds pass rewrote. Fix: `if strings.TrimSpace(cfg.Storage.Path) == "" { errs.add("storage.path", "must not be empty") }`.

### M5. New `agent.workspace` doc claim is false in both halves

`docs/configuration.md:46`:

> Only `mtclaw onboard` creates it; nothing else does. | ... filesystem-tool calls into it will fail at call time.

`internal/tools/path_guard.go:88-115` (`resolveDeepestSymlinks`) deliberately
resolves the deepest *existing* ancestor and rejoins the missing tail, and
`internal/tools/fs.go:197` then runs `os.MkdirAll(filepath.Dir(resolved), 0o755)`.
So a single `write_file` into a missing workspace creates the workspace (mode 0755),
and does not fail. Fix the row, or make `write_file` refuse when the root itself is
missing.

### M6. README's Go requirement is stated as an exact pin

`README.md:24-26`:

> Requires Go 1.25.7 (the exact version pinned in `go.mod`, since it has no `toolchain` directive) to build from source

The `go` directive is a *minimum*, not a pin: this review built the tree with
`go1.27.1` (`go build ./...`, full `go test ./...`). With the default
`GOTOOLCHAIN=auto`, an older 1.25.x also works by fetching 1.25.7. Only
`GOTOOLCHAIN=local` on < 1.25.7 fails. Say "Go 1.25.7 or newer (with
`GOTOOLCHAIN=local`, exactly ≥ 1.25.7)".

---

## Low

| # | Location | Issue |
|---|---|---|
| L1 | `internal/logging/logger.go:23` | Log **directory** still `0755` while the log file moved to `0600` and every other created dir moved to `0700`. Also, `O_CREATE` mode applies only at creation: an existing `0644` log file from a pre-fix run keeps `0644` forever. Mirror `sqlite.Open`'s best-effort `os.Chmod(path, 0o600)`. |
| L2 | `internal/cli/onboard_cmd.go:107` | Workspace created `0755` (declared out of scope). Defensible, but it is now the only dir onboard creates that is world-readable; worth one line in `docs/security.md` if intentional. |
| L3 | `internal/cli/doctor_checks.go:78-81` | Stale comment: "the database, the instance lock, and (after onboard) the starter AGENTS.md all live there [~/.mtclaw]" — onboard now writes AGENTS.md next to `--config`. |
| L4 | `docs/configuration.md:47` | Same drift: "`onboard` writes one entry here pointing at the starter `~/.mtclaw/prompts/AGENTS.md`" — only true for the default config path now. |
| L5 | `docs/architecture.md:50-53` | "`Load(data, baseDir, env)` is a pure function of its three inputs" is still false: `resolveSecret` reads `*_file` from disk (`load.go:121-127`) and `warnIfWorldReadable` prints to `os.Stderr` (`load.go:143`); `Validate` stats the filesystem. The rewrite fixed the write claim but kept the purity claim. |
| L6 | `internal/cli/root.go:73-77` | With the storage dir no longer pre-created, a first-run `sessions list` now fails with the raw driver text `open store: open sqlite database: unable to open database file (14)`. Worth wrapping the read-only-open failure with "no database yet — run `mtclaw prompt` or `mtclaw gateway` first". |
| L7 | `.github/workflows/ci.yml:50-51` | `go mod tidy -diff` runs on all three OS legs; one leg is enough. The new CGO-free `go test ./...` roughly doubles each leg's wall clock (it was requested, just noting the cost). Both steps are correct as written on `pwsh` (the Actions wrapper propagates `$LASTEXITCODE`). |
| L8 | `main.go:10-12` | Exits `1` on interrupt rather than `130`; trivial, but once C1's escape hatch lands, `130` is the conventional signal exit. |

### Test quality

- Genuinely pinning (fail if the fix is reverted): `TestValidate_ToolBounds`,
  `TestValidate_OpenAIMaxRetriesNegative`, `TestValidate_StorageParentDirCreatable`
  (asserts the dir is *not* created), `TestOpenStore_WriteModeCreatesStorageDir_ReadOnlyDoesNot`,
  `TestRunOnboard_WritesAgentsMDNextToConfigPath` (config path is a temp dir distinct
  from the fake home, so it really discriminates), `TestNew_LogFileIsCreatedWithMode0600`,
  `TestRunOnboard_TelegramEnabled_NonPositiveManualIDWarns`.
- Would pass with the fix reverted: `TestValidate_StorageParentDirNotWritable`
  (`MkdirAll` into a `0500` dir also errors), `TestValidate_ToolBoundsSkippedWhenDisabled`
  (guard, not a proof), both `root_test.go` cobra-tree tests w.r.t. H2.
- Missing from prior M8: nothing asserts that `prompt` wires `TerminalApprover` and
  `cron run` wires `DenyAllApprover`. That was the one behavioral invariant the
  `newLoop` refactor could have silently broken (it did not — verified by reading
  `prompt_cmd.go:45-48` and `cron_cmd.go:118-122`), and it is still unpinned.
- No test anywhere covers `Execute`'s signal handling (H2 above).

---

## Docs drift still open

| Location | Claim | Reality |
|---|---|---|
| `docs/configuration.md:106`, `internal/cli/doctor_checks.go:294-297` | exec.cwd confinement "enforced at call time by `tools.Resolve`, symlink-aware" | `internal/tools/exec.go:210` sets `cmd.Dir = e.cfg.CWD` with no resolution or root check. Nothing checks it, ever. (H1) |
| `internal/cli/doctor_checks.go:46` | Row named "exec.cwd inside a root" | Checks existence only. (H1) |
| `docs/configuration.md:46` | "Only `mtclaw onboard` creates it; nothing else does" / "filesystem-tool calls into it will fail at call time" | `write_file` creates it via `tools/fs.go:197` + `path_guard.go:88-115`. (M5) |
| `docs/configuration.md:47` | starter prompt at `~/.mtclaw/prompts/AGENTS.md` | Now `<dir of --config>/prompts/AGENTS.md`. (L4) |
| `docs/architecture.md:50-53` | `Load` "a pure function of its three inputs" | Reads `*_file` secrets from disk, writes warnings to stderr, stats dirs. (L5) |
| `README.md:24-26` | "Requires Go 1.25.7 (the exact version pinned)" | Minimum, not a pin; built here with go1.27.1. (M6) |
| `internal/cli/doctor_checks.go:78-81` | state dir holds "the starter AGENTS.md" | No longer true. (L3) |
| `internal/cli/cron_cmd.go:200-203` | "the turn itself already ran to completion by the time this is called" | Not after a signal. (M2) |
| `internal/cli/root.go:42-44` | gateway's nested NotifyContext is "redundant but harmless" | It is redundant, but not harmless: it blocks the second-signal hard kill for the gateway leg. (C1 fix note) |

Correct and re-verified: `openai.max_retries` row, `web_fetch.timeout`/`max_bytes`,
`filesystem.max_*`, `exec.timeout`/`approval_timeout`/`max_output_bytes` rows
(each matches a real `validate.go` rule, each gated on the tool's `enabled`);
`prompt` concurrency note (matches `resolveCLISession`'s `cli`/`local` Ensure and
the absence of any lock); README's "bare binaries, not archives" (matches
`release.yml:55-64`); README's `go install` stamping note (`mtclaw version` on a
plain `go build` prints exactly `mtclaw dev (commit none, built unknown)`);
docs-coverage-test caveat.

---

## Prior fixes: verified / not verified

| Item | Status | Evidence |
|---|---|---|
| H1 tool/openai bounds | **Verified** | All seven rules present, all gated on `Enabled`; `validConfig` derives from `Default()` so `TestValidate_ValidConfigPasses` proves defaults pass. Behavior change: `web_fetch.timeout: 0s` (previously "no timeout") and zero byte caps now fail to load — intentional and documented. |
| H2 `signal.NotifyContext` + `ExecuteContext` | **Regressed** | Installed correctly, `stop()` deferred on every path, store still closed after the tree runs — but it removed default signal death with nothing to replace it. See C1, H2. |
| M1 `Validate` stops writing | **Partial** | Storage dir: verified (binary probe + `root_test.go`). Log dir/file: still created by every command. See M1. |
| M2 drop `exec.cwd` confinement | **Verified (code) / wrong docs** | `isWithinRoot` gone, no stale references, `load_test.go` case flipped. Docs and comments now claim a runtime control that does not exist (H1). |
| M4 `newLoop` / `sendTelegram` | **Verified** | Identical wiring and error strings; approver choice preserved (`TerminalApprover` for `prompt`, `DenyAllApprover` for `cron run`); `send` path unchanged. Not pinned by a test. |
| M5 prompt concurrency note | **Verified** | Present in `prompt --help` output and `docs/configuration.md:23-27`. |
| M6 `0700`/`0600` modes | **Mostly** | openStore (via `sqlite.Open`), doctor state dir, onboard prompts+config dirs, log file all correct. Log dir `0755`, workspace `0755` (declared out of scope); existing log files keep `0644` (L1). |
| M7 AGENTS.md next to config | **Verified** | Real discriminating test; doc row and doctor comment not updated (L3, L4). |
| M8 new tests | **Partial** | paths/logger/root tests added and green; approver-wiring test and any signal test missing. |
| L1/L2/L8/L9 comment+UX items | **Verified** | `checkAllowlistNonEmpty` comment now matches `runDoctor`'s real call order; `config show` Short updated; non-positive id warns without rejecting; refusal message trimmed to three lines. |
| CI steps | **Verified** | Both new steps correct on all three legs; `CGO_ENABLED=0 go test ./...` is green repo-wide here; `go mod tidy -diff` is clean (implementer's go-shellwords note is stale). |

---

## Unresolved questions

1. C1's fix crosses slices: the second-signal escape hatch is only complete if
   `internal/gateway`'s nested `notifyContext` is deleted. Who owns that change?
2. Is `mtclaw onboard` intended to be usable non-interactively at all? Today EOF
   means "accept every default", which is how the infinite loop is reachable. If
   not, aborting on EOF is strictly better; if yes, it needs a real `--non-interactive`
   flag with required values, not silent defaults.
3. `tools.exec.cwd` now has no root relationship enforced anywhere. Accept that
   (and fix the docs per H1), or restore a load-time check built on `filepath.EvalSymlinks`
   for the roots that exist? The prior review offered both; the docs were written as
   if the second was chosen.
4. `storage.path: ""` (M4) — reject at validation, or is an anonymous throwaway
   database a deliberate testing affordance?
