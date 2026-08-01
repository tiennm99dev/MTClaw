# Phase 5 Implementation Report — Tools and Policy Engine

Date: 2026-08-01
Plan: `plans/260731-2219-mtclaw-core-system/`

## Status: Completed

`mtclaw prompt` is wired to a real tool registry (fs, web_fetch, exec) with `TerminalApprover`,
making it the first *useful* milestone per plan.md. Full deny corpus, path guard matrix, SSRF
matrix, process-tree-kill (timeout and turn cancellation), redaction, approver, and classifier
tests all pass. `go build`, `CGO_ENABLED=0 go build`, `gofmt -l`, `go vet`, and `go test ./...`
(including phases 1-4) are all clean. `go test -race ./internal/tools/...` also clean.

## Files created

- `internal/tools/registry.go` — `Registry` (`Tool`, `Register`, `Specs`, `Run`, implements
  `agent.ToolRunner`), and `New(cfg, store, approver, log)` which wires fs/web_fetch/exec from
  config.
- `internal/tools/spec.go` — `objectSchema`/`stringProp`/`integerProp`/`enumProp` JSON-schema helpers.
- `internal/tools/path_guard.go` — `Resolve(roots, path)`: NUL/empty rejection, `~` expansion,
  deepest-existing-ancestor `EvalSymlinks` + tail rejoin, `filepath.Rel`-based containment,
  case-insensitive on windows/darwin.
- `internal/tools/fs.go` — `read_file` (offset/limit, truncation marker, NUL-sniff binary refusal),
  `write_file` (overwrite/append/create_new, max_write_bytes, parent-dir creation inside root),
  `list_dir` (depth cap 8, entry cap 2000, never follows any symlink — stricter than "never leaves
  the root": never descends into one at all, which is also cycle-safe).
- `internal/tools/web_fetch.go` — GET-only, http/https-only, `net.Dialer.Control`-based SSRF guard
  (`isBlockedAddr` using `netip` predicates + explicit 0.0.0.0/8 and 100.64/10 CIDRs, IPv4-mapped
  IPv6 handled via `Addr.Unmap()`), 3-redirect cap, `max_bytes` cap, regex-based HTML→text stripping
  script/style, untrusted-content note.
- `internal/tools/policy.go` — `Verdict`, `Decision`, `Policy.Evaluate` (deny → allow → mode),
  `evaluateAuto` (classifier-gated, 10s bound, fail-closed on any error/nil-classifier).
- `internal/tools/deny_defaults.go` — `DefaultDenyPOSIX`, `DefaultDenyWindows`, copied verbatim
  from the phase file.
- `internal/tools/classifier.go` — `Classifier` interface, `ClassifyResult`, `LLMClassifier`
  (`NewLLMClassifier` builds its own `openai.Client` with `MaxRetries: 0`), forced-JSON prompt,
  strict unmarshal + risk-value validation, markdown-fence stripping.
- `internal/tools/approver.go` — `Request`, `Approver` interface, `DenyAllApprover` (+`ErrNoApprover`),
  `TerminalApprover` (stdin y/N, `context.WithTimeout(ctx, ...)` so a single derived context
  distinguishes outer-cancel (`context.Canceled`) from its own timeout (`DeadlineExceeded`)),
  `RedactSecrets` (Bearer/Authorization/--token/--password/--secret*/-p<val>/KEY=|TOKEN=|SECRET=|
  PASSWORD=, sk-/ghp_/AKIA/base64-hex-run patterns, 800-char truncation).
- `internal/tools/exec.go` — the exec tool: shell resolution (config or OS default), go-shellwords
  tokenize-fail-closed, deny/refuse/run/ask dispatch, `execute()` using `exec.CommandContext` with
  `ctx, cancel := context.WithTimeout(turnCtx, cfg.Timeout)`, `cmd.Cancel = killProcessTree`,
  `cmd.WaitDelay`, output cap + truncation marker, audit write on every path (including refusals
  and cancellations), `ctx.Err() != nil` checked *before* `errors.As(&exitErr)` (a SIGKILLed/
  taskkilled process still surfaces as a normal-looking `*exec.ExitError` — see Deviations).
- `internal/tools/exec_unix.go` / `exec_windows.go` — `setProcessGroup`/`killProcessTree`:
  POSIX uses `Setpgid` + `syscall.Kill(-pgid, SIGKILL)`; Windows uses `taskkill /F /T /PID` (chose
  taskkill over a Job Object per the phase file's "pick one, document it" — see Deviations).
- `internal/cli/approvals_cmd.go` — read-only `approvals list` (built on `AuditStore.List`, see
  Deviations), with `--limit`, and a doc comment on why deciding from the CLI is absent.
- Tests: `path_guard_test.go`, `fs_test.go`, `web_fetch_test.go`, `policy_test.go` (incl. full POSIX
  and Windows deny corpora, the two historical bypasses, the policy table), `classifier_test.go`,
  `approver_test.go`, `exec_test.go` (incl. real-sqlite-backed audit assertions, timeout and turn-
  cancellation process-tree-kill proofs using a `Start-Job`/backgrounded-subshell marker-file
  technique), `registry_test.go`.

## Files modified

- `internal/cli/prompt_cmd.go` — removed `noopToolRunner`; builds `tools.NewTerminalApprover` and
  `tools.New(*s.cfg, st, approver, slog.Default())`, wires the resulting registry into `agent.New`.
- `internal/cli/root.go` — added `root.AddCommand(newApprovalsCmd(s))` (see Deviations).
- `go.mod` / `go.sum` — added `github.com/mattn/go-shellwords v1.0.14` (only new dependency, as
  the task specified). `golang.org/x/sys` was **not** touched (Windows kill uses `taskkill`, not a
  Job Object, so no new usage of that module).
- `plans/260731-2219-mtclaw-core-system/phase-05-tools-and-policy-engine.md` — `status: completed`.
- `plans/260731-2219-mtclaw-core-system/plan.md` — phase 5 row → Completed.

## Deviations from the literal file list (with rationale)

1. **`internal/cli/root.go` modified**, though not listed in "Related Code Files". A new cobra
   subcommand is dead code unless registered on the root command (exactly how `newPromptCmd` was
   already wired in phase 4); leaving it unregistered would be worse than a one-line, non-conflicting
   addition to a file no other phase in this run owns.
2. **`docs/security.md` not created.** The task's explicit instructions overrode the phase file here:
   docs/ is a phase 9 deliverable and creating it now was expressly disallowed. The threat-model text
   stays in the phase file, which is what the task asked for.
3. **`approvals list` reads `store.AuditStore`, not a `store.ApprovalStore.List`.** `ApprovalStore`
   (phase 2, out of my file ownership) has no `List` method — by design, per the phase file's own
   note that pending approvals live only in the gateway's in-memory waiting goroutine. `exec_audit`
   is the durable decision log (covers `denied_rule`/`allowed_rule`/`approved`/`denied_user`/
   `expired`/`auto_allowed`), so `approvals list` surfaces that, with a derived (not stored) "decider"
   column. Documented in the command's help text and in this report rather than silently reinterpreting
   the command's purpose.
4. **Windows process-tree kill uses `taskkill /F /T /PID`, not a Job Object.** The phase file
   explicitly allows either ("pick one, document it"). `taskkill /T` walks the OS-tracked parent-PID
   tree, which I verified empirically (see Tests section) kills a `Start-Job`-spawned grandchild
   process, matching what PowerShell-spawned children need. This avoids a new `golang.org/x/sys/windows`
   dependency for Job Object syscalls.
5. **`list_dir` never follows *any* symlink**, not just ones escaping the root. This is stricter than
   the letter of "never follow symlinks out of the root" but simpler and additionally cycle-safe
   (a symlink loop inside the root can't cause unbounded recursion). Entries are still listed (typed
   `symlink`), just never recursed into.
6. **Ordering bug caught and fixed during implementation**: my first draft of `execute()` checked
   `errors.As(runErr, &exitErr)` before `ctx.Err() != nil`. A SIGKILLed (POSIX) or taskkilled (Windows)
   process still returns a normal-looking `*exec.ExitError`, so that ordering would have silently
   swallowed every turn-cancellation-during-exec case as if it were an ordinary non-zero exit,
   never propagating `ctx.Err()` to the agent loop. Reordered so `ctx.Err() != nil` is checked first;
   `TestExec_TurnCancellationKillsProcessTree` is the regression test for this.

## Tests / Validation performed

- Policy table: deny (both modes, classifier never invoked), deny-wins-over-allow, allow-match,
  approval-mode-ask, auto-mode low/high/none-with-confirm_on-category/error/timeout/nil-classifier.
- Deny corpus: every must-catch/must-not-catch/accepted-false-positive from the phase file, for both
  `DefaultDenyPOSIX` and `DefaultDenyWindows` (including `Remove-Item foo.txt` and
  `del build\out.txt` passing through), plus a dedicated regression test pinning the two historical
  bypasses (`rm --recursive --force /`, `/bin/rm -rf /`).
- Path guard: empty, NUL byte, traversal, absolute-outside-root, prefix confusion (`/data-evil` vs
  `/data`), symlink escape, symlink-inside-root, non-existent file/nested path in an existing dir,
  multi-root first-match, case-insensitivity (skipped on non-windows/darwin).
- SSRF: unit matrix over `isBlockedAddr` (loopback, unspecified, private, link-local, CGNAT,
  multicast, IPv4-mapped IPv6, plus two public IPs asserted *not* blocked); integration tests via
  `httptest` for loopback refusal, hostname-resolving-to-loopback (`localhost`) refusal, redirect-to-
  loopback refusal, too-many-redirects, HTML stripping + untrusted-content note, max-bytes truncation;
  a real-internet fetch is gated behind `MTCLAW_E2E=1` and skipped by default.
- Exec: non-zero exit is a result not an error; `tools.exec.timeout` firing kills the whole process
  tree (verified via a backgrounded-subshell/`Start-Job` marker file that must never be written);
  turn-context cancellation independently kills the tree and propagates `context.Canceled`; output
  cap + truncation marker; `exec_audit` row on every path (deny, allow, approved, denied_user,
  expired, tokenize-failure) verified against a real (temp-file) sqlite store.
- Redaction: Bearer/Authorization, env-assignment, --token/--secret, AWS/GitHub key shapes,
  800-char truncation, no-op on an unrelated command; end-to-end exec test confirms the audited
  command is redacted while the actually-executed command still ran byte-identical (proven by the
  `echo start && curl ...` command's `echo` half still producing output).
- Approver: `DenyAllApprover` never blocks and always errors `ErrNoApprover`; `TerminalApprover`
  approves/denies/times-out with the right label, and specifically distinguishes an outer-context
  cancel (`context.Canceled`) from its own timeout (`context.DeadlineExceeded`) via one derived
  context rather than a second select arm.
- Classifier: fake-provider-backed `LLMClassifier.Classify` tests for valid response, markdown-fence
  stripping, malformed JSON, empty body, invalid risk value, provider error, and an already-expired
  context — all produce an error (never a risk value), which `Policy.evaluateAuto` always turns into
  `VerdictAsk`.
- `mtclaw prompt` smoke test: ran the real CLI against a temp config with a fake API key; confirmed
  config load → session → registry construction (fs/web_fetch/exec all registered) → agent loop
  iteration → real OpenAI call attempt (rejected only because the key is fake) — i.e. every piece
  of this phase's wiring executes end-to-end without a panic or wiring error. `mtclaw approvals list`
  also smoke-tested against the same store.

## Unresolved questions

None blocking. One judgment call worth flagging: `RedactSecrets`'s `-p<value>` pattern
(`(\s-p)(\S+)`) is intentionally broad enough to also redact unrelated `-p...` flags (e.g. `-parents`,
`-port`) on tools other than mysql/psql — accepted per the phase file's own "over-redaction is the
safe failure direction" guidance, and it only affects what is *displayed/stored*, never the executed
command.
