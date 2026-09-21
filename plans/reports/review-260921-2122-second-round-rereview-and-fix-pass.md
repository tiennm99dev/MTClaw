# MTClaw second-round re-review and fix pass

Date: 2026-09-21. Reviews the uncommitted tree produced by the first pass (`review-260921-1601-full-project-rereview-and-fix-pass.md`) on top of `main` @ 7927b6d.

## Verdict

The first pass held in most places, but four fresh reviewers found 4 blocking regressions it introduced, 3 first-pass fixes that did not actually hold, and a tail of mediums. All confirmed items are now fixed and the tree is green. Two rounds of independent review were needed; a third is not indicated by the finding rate (round 2 found no Critical outside the round-1 diff).

## Process

4 read-only reviewers on the diff (fresh eyes, told to verify implementer claims against code) -> 4 implementers with disjoint ownership -> controller integration. Round-2 implementers were killed once by a rate limit and relaunched; one partial store edit was finished by the controller.

Reports:
- Re-reviews: `code-reviewer-260921-1649-{agent-provider-store,tools-policy,telegram-gateway-cron,cli-config-docs}-rereview.md`
- Fixes: `fullstack-developer-260921-2100-{agent-provider-store,telegram-gateway-cron,cli-config-docs}-fixes-round2.md`, `fullstack-developer-260921-2103-tools-policy-fixes-round2.md`

## Regressions introduced by round 1 (all fixed)

- **Approval bypass, terminal approver.** The single long-lived stdin reader parked a "y" typed after a timed-out prompt and handed it to the next `Ask`, approving a command the user never saw. A round-1 test asserted this as a feature. Now: stale lines are drained before each prompt, the channel is buffered, EOF fails every later `Ask` fast instead of blocking 5 minutes. Tests flipped to assert the safe behavior.
- **Allow-list widened across newlines.** `(?s)` was applied to allow patterns too, so `^npm run .+$` auto-ran an appended second line. Now deny-only.
- **Telegram escaper infinite recursion.** Re-splitting a chunk whose fence info string exceeded the limit made no progress and recursed forever on the worker goroutine (reproduced: hang plus memory growth). Now bails to a hard rune-safe cut.
- **Unkillable `onboard`.** Installing `signal.NotifyContext` removed the default die-on-signal; `onboard` on a closed stdin spun writing "a model is required" to stdout (a reviewer probe wrote gigabytes and survived `timeout`). Now: EOF is an error, prompt reads observe the command context so one SIGTERM ends a blocked prompt (verified with the binary), a second signal falls back to the default kill, and the gateway's own nested handler is gone.

## Round-1 fixes that did not hold (all fixed)

- DB `chmod 0600` covered only the main file; the WAL sidecar holding today's rows stayed 0644. Now all three files.
- Orphan tool-call repair used a global id set, so reused call ids or a tool row before its declaration still poisoned the session. Now a strict sequential pass that runs after trimming; property test generates id reuse and out-of-order rows.
- Migration 002 (drop a redundant index) made every read-only CLI open of an existing database fail until a writer upgraded it. Deleted; read-only up-to-date and behind-schema opens are now pinned by tests.

## Other confirmed fixes

- Deny normalization narrowed to the command word (`grep "rm -rf" file` is no longer permanently refused; `'rm' -rf` still is). `sh -c` / `eval` wrappers remain a documented accepted bypass, now with a test.
- Redaction catches `?token=` and `--api-key=`; base64 catch-all redacts AWS-shaped secrets while long paths survive.
- Approval timeout vs late callback: the approvals row is the arbiter; a decision that landed before `finishExpired` is honored.
- Telegram send retry limited to 5xx (network errors could double-send, including approval prompts).
- Dispatcher `closed` flag set under the mutex before `wg.Wait`, closing the `Add`/`Wait` race for real; callback goroutines tracked and joined in `Start`.
- Cron catch-up walks the most recent missed minutes first.
- Context-length elision no longer destroys persisted tool output; 413 classified as context-length; 401/403/429 keep their existing classes.
- Prompt-file cap requires a regular file and reads through a limit reader (a FIFO or `/dev/zero` symlink no longer hangs or OOMs).
- Read-only CLI commands no longer create the log directory or file; `ensureDirCreatable` dropped its wrong-bit heuristic; empty `storage.path` rejected; interrupted `cron run` still records its row; friendly error when no database exists yet.
- Docs: `tools.exec.cwd` documented as unconfined (doctor row renamed to "exec.cwd exists"); workspace creation, AGENTS.md location, "pure function" claim, Go version wording corrected.
- Phantom tests removed or made real (progress reporter, net-timeout classify case, disabled-tool bounds, storage-dir writability).

## Deliberately not changed (product decisions, unchanged from round 1)

1. Filesystem writes: no audit rows, no approval gate (schema change).
2. `exec_audit` requester attribution columns.
3. `sh -c` / `eval` deny bypass, documented as accepted.
4. telego long-poll cannot be made fatal from outside; a revoked token now logs at Warn but the process stays up.
5. `echo 'git push --force'` is refused by the raw `git push --force` deny rule (pre-existing, unrelated to normalization).
6. `openai.base_url` pointing at non-OpenAI backends: several fixes assume yes; harmless if no.

## Gate (final)

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `CGO_ENABLED=0 go build ./...`, GOOS=windows, GOOS=darwin | clean |
| `go mod tidy -diff` | clean |
| `go test -race -count=2 ./...` | 12/12 ok |
| `CGO_ENABLED=0 go test ./...` | 12/12 ok |
| `mtclaw onboard </dev/null` | exits 1 immediately, writes nothing |
| SIGTERM to a blocked `onboard` | dies on the first signal |

Diff vs `main`: 76 files, +3854 / -608. Non-test Go +1937 / -500. Tests +1834 / -71 (13 new test files).

## Controller integration work

- Finished the interrupted store edit: `chmodOwnerOnly` helper for db, `-wal`, `-shm`; extended permission test writes a row first.
- Prompter reads now select on the command context (one signal ends a blocked prompt); interrupt test added.
- Store interface comments: `Create` requires `ExpiresAt`; `Finish` matches on id only.

Changes are uncommitted on `main`.
