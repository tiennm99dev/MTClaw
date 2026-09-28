# Third-round review fixes

Status: complete (uncommitted on branch)
Branch: `refactor/260928-full-review`

## Outcome

Fix the bugs and apply the complexity-reducing refactors found by the
third-round review, without changing accepted product decisions.

Source reports (`plans/reports/`):

- `code-reviewer-260928-0953-agent-provider-store-review.md`
- `code-reviewer-260928-0953-tools-policy-review.md`
- `code-reviewer-260928-0953-telegram-gateway-cron-review.md`
- `code-reviewer-260928-0953-cli-config-docs-ci-review.md`

## Decisions

User-decided:

- Telegram output: whichever gives the best UX, HTML allowed. The choice is
  `parse_mode: HTML`, with model Markdown converted to Telegram HTML (bold,
  italic, code, pre, links) and plain-text fallback. Approval prompts are
  built as HTML with the command in an escaped `<pre>`.
- `agent.temperature` is optional and sent only when set.
- `require_mention` defaults to true for every group, listed or `"*"`.
- The gateway instance lock lives next to the database (one gateway per db).

Controller defaults (reversible, flagged in the final report):

- A provider error mid-turn persists the tool activity that already ran, as
  the other abort paths do.
- History loading gets a fixed internal byte budget, not a new config key.
- Exec kills the whole process group when the command returns; background
  children do not outlive the call. Exit is reported correctly with output.
- Approval display cap is raised to about 3500 characters; longer commands
  are refused before prompting. The audit row keeps the full command up to a
  much larger cap.
- Secrets: fix the docs, strip default env names even when `*_env` is empty,
  and set `PR_SET_DUMPABLE=0` on Linux so children cannot read the parent's
  `/proc/<pid>/environ`.
- `/new` during a running turn cancels the turn, then resets.
- Queued messages are lost on restart; documented, not persisted.
- Module path stays `github.com/tiennm99/MTClaw` (no breaking import change).
- CI keeps one leg on the `go.mod` minimum; release builds with `stable`.

Unchanged accepted decisions: fs writes have no audit/approval gate; no
exec_audit requester columns; `sh -c`/`eval` deny bypass accepted; telego
long-poll not fatal; `openai.base_url` may point at non-OpenAI backends.

## Phases

| Phase | Scope | Owner |
|---|---|---|
| 1a | agent, provider, store, `agent.temperature` config field | dev agent |
| 1b | tools (redaction, approval limits, exec, fs, web_fetch) | dev agent |
| 1c | channel/telegram (HTML rendering), gateway, cron | dev agent |
| 2 | cli, config, logging, CI, docs, lock location, groups default, dumpable | dev agent, after 1a-1c |
| 3 | full gate + final review | tester, code-reviewer |

Phase 1 agents run in parallel with disjoint package ownership. Mechanical
caller updates in `internal/cli` for their own API changes are allowed.

## Acceptance

- Every High and Medium finding is fixed with a regression test, or is
  listed in the final report with a reason.
- `gofmt -l .`, `go vet ./...`, `go test -race ./...`, `CGO_ENABLED=0 go
  build` for linux/windows/darwin, and `go mod tidy -diff` are clean.
- No plan IDs, phase numbers, or finding codes in code, test names, or
  comments.
- Docs match the changed behavior.
