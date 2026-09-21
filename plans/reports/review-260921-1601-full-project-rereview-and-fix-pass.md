# MTClaw full re-review and fix pass

Date: 2026-09-21. Baseline: `main` @ 7927b6d. Scope: whole repo, "refactor or rewrite acceptable".

## Verdict

No package warranted a rewrite. All four reviewers independently rated every package Keep or small Refactor; the only rewrite taken was the instance lock (PID-file heuristic -> kernel `flock`, ~40 lines). The architecture (config -> store -> provider -> agent -> tools -> channel -> gateway, strictly inward deps, single atomic flush per turn, deny -> allow -> mode policy) is sound. Findings were seam bugs, missing bounds, and untested recovery paths.

## Process

4 read-only reviewers in parallel (one per slice), then 4 implementers with disjoint file ownership, then one integration gate. Per-slice reports:

- Reviews: `code-reviewer-260921-1539-{agent-provider-store,tools-policy-security,telegram-gateway-cron,cli-config-docs-ci}-review.md`
- Fixes: `fullstack-developer-260921-1601-{agent-provider-store,tools-policy,telegram-gateway-cron,cli-config-docs}-fixes.md`

## Baseline state found

- `go test ./internal/tools` red on `main`: two process-tree-kill tests panicked (nil approver in the test harness). Root cause was a real bug, not just a harness gap: the shellwords tokenize gate ran before the deny list and rejected subshell syntax, so `(rm -rf ~) &` became an approvable prompt instead of a hard refusal.
- Everything else built and passed.

## Fixed (by severity)

Critical
- Agent loop dispatched on `finish_reason == "tool_calls"` alone; a `tool_calls` payload under any other reason (common with non-OpenAI `base_url` backends) was persisted without tool results and poisoned the session for up to 40 turns. Now dispatches on payload; terminal assistant rows never carry tool_calls; history loading repairs already-orphaned rows.

High
- Deny list bypass via `(`: tokenize gate removed, `go-shellwords` dependency dropped, `rm` deny patterns accept a leading `(`.
- Unbounded exec output buffer (accidental `yes` OOM-kills the gateway): capped at write time.
- Secret redaction missed `API_KEY=` / `GITHUB_TOKEN=` forms and destroyed ordinary paths in audit rows: both regexes fixed.
- TerminalApprover leaked a stdin reader per timeout and could swallow the next answer: single long-lived reader.
- Long Telegram replies dropped deterministically (MarkdownV2 escaping pushed a 4096-byte chunk over the limit, fallback only matched "parse"): chunks re-split by escaped size, "too long" falls back to plain text.
- getUpdates failures (revoked token, 409 second poller) were logged to a discarded logger and retried forever: redacting slog adapter installed.
- Worker exiting on shutdown stayed in the dispatch map (lost messages, `WaitGroup` Add/Wait race): dispatch refuses after root cancel; exit branch mirrors the reap branch.
- Context-length retry trimmed history but never the in-turn buffer (deterministic repeat failure): tool results elided to placeholders on retry.
- Cancellation flush had no deadline against a one-connection pool: 5s bound.
- No config bounds on any duration/byte cap except `openai.timeout`; `max_read_bytes: -5` panicked the daemon via `make([]byte, n)`: bounds added, plus a clamp in `read_file`.
- No signal handling for `prompt` / `cron run`: root command uses `signal.NotifyContext`.

Medium (all applied)
- Deny regexes compiled with `(?s)` (line continuations); quote/backslash-normalized second deny match; child exec env stripped of the configured API key and bot token variables; rune-safe truncation.
- Non-text Telegram messages no longer start an LLM turn; captions are read.
- Approval timeout vs late callback race: wait channel drained first, no contradictory edits, "no longer waiting" outcome.
- Callback queries handled off the update pump; `Ensure` bounded by a 10s ctx; drain-deadline breach now skips `store.Close` and exits non-zero.
- `cron_runs` rows stuck in `started` are swept to `interrupted` at startup; missed ticks (>90s) logged and caught up (bounded to 10 minutes); cron job timeout delivers a notice to its chat.
- `Validate` no longer writes to disk; `sqlite.Open` is the single owner of storage-dir creation (0700). `exec.cwd`-inside-roots rule moved out of `Validate` (doctor keeps the usability check).
- 4xx provider errors (404 model_not_found) classified permanent, not transient; context-length fallback on message when the API returns no error code; `ToolCall` gets JSON tags (legacy rows still decode).
- Truncation notice no longer persisted as the model's words; `system_prompt_files` capped at 256 KiB each; approvals `Create` rejects zero `ExpiresAt`.
- CLI wiring deduplicated (`newLoop`, `sendTelegram` helpers); `onboard` writes `AGENTS.md` next to the config; state dir 0700, log 0600, DB 0600.

Low (applied)
- Extra SSRF ranges (broadcast, 192.0.0.0/24, 198.18/15, NAT64, 6to4 with embedded IPv4 rechecked); non-text Content-Type skipped.
- Bounded retry on 5xx/network for Telegram sends; reply timeout scales with chunk count; `\` and backtick escaped inside code spans; progress timer hygiene; global-queue drop replies to the user.
- Dead branches removed; `NewWithAPIKey` sets retries; `Append` bumps `updated_at`; migration 002 drops a redundant index; usage reported on provider error.
- Docs drift fixed (workspace creation, web_fetch timeout, pure-function claim, Go version, `go install` stamping, bare-binary releases, coverage test is presence-only); CI adds `CGO_ENABLED=0 go test` and `go mod tidy -diff` steps.

## Deliberately not changed (need a product decision)

1. Filesystem writes have no audit rows and no approval gate. Adding rows needs a schema change (`tool` column on `exec_audit` or a new table). Gating is a UX trade-off.
2. `exec_audit` cannot attribute a command to a requester (`channel`, `chat_id`, `message_id` dropped). Additive migration, but decide together with (1).
3. `sh -c '...'` / `eval` wrappers still bypass the deny list. Documented as accepted in `docs/security.md`; closing it needs interpreter-aware parsing.
4. telego's long-poll retry loop cannot be made fatal from outside via its Logger (no success signal to reset a failure counter). A revoked token is now visible in logs but the process stays up. Fix needs a cancelable child context around the poller.
5. Concurrent `mtclaw prompt` runs share the `cli/local` session unserialized. Documented; a `prompt.lock` would close it.
6. `Load` still writes its world-readable warning to stderr directly; returning warnings changes the signature used by the openai e2e test.

## Gate (final, after integration)

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `CGO_ENABLED=0 go build ./...` | clean |
| `GOOS=windows` / `GOOS=darwin` builds | clean |
| `go mod tidy -diff` | clean, `go-shellwords` removed |
| `go test -race -count=1 ./...` | 12/12 packages ok |
| `CGO_ENABLED=0 go test ./...` | 12/12 packages ok |
| CLI smoke: `config validate` with bad bounds | rejects both, aggregates errors, creates no directories |

Diff: 73 files, +2672 / -507. Non-test Go: +1326 / -414. Tests: +1270 / -57 (9 new test files).

## Integration fixes made by the controller

- `internal/cli/doctor_test.go`: lock test simulated "held" by writing a PID file; under `flock` it now acquires the real lock.
- Unified storage/state directory creation to `sqlite.Open` + `gateway.New` at 0700; removed the duplicate in `cli.openStore`.

## Unresolved questions

See "Deliberately not changed" 1-6. Also: is `openai.base_url` officially expected to point at non-OpenAI backends? The Critical fix holds either way, but it decides whether the message-based context-length fallback should stay.

Changes are uncommitted on `main`.
