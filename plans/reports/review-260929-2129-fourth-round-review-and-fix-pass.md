# Fourth-round review and fix pass

Date: 2026-09-29. Branch `main` on top of `e8b0b74`. All changes are
uncommitted in the working tree.

## What happened

Four read-only reviewers covered the whole repository (tools/agent/provider,
gateway/telegram/cron, cli/config/store, and docs/CI). Three fix agents then
worked in parallel on disjoint packages, a docs agent reconciled the
documentation against the resulting source, and the controller added a lint
configuration plus a handful of small fixes. Every defect fix carries a
regression test that was verified to fail with the fix reverted, except the
handful of comment-only and refactor items called out in the per-area reports.

Reports for this round:

- `code-reviewer-260929-2032-tools-agent-provider-review.md`
- `code-reviewer-260929-2032-gateway-telegram-cron-review.md`
- `code-reviewer-260929-2032-cli-config-store-review.md`
- `docs-manager-260929-2032-docs-ci-audit.md`
- `fullstack-developer-260929-2032-tools-agent-provider-fixes.md`
- `fullstack-developer-260929-2032-gateway-telegram-cron-fixes.md`
- `fullstack-developer-260929-2032-cli-config-store-fixes.md`
- `docs-manager-260929-2119-docs-update.md`

## Highest-impact fixes

- The default POSIX deny list now catches `rm -R`, a separator with no
  space before `rm`, backtick substitution, every force-push spelling,
  `chmod 777 -R /`, and pipes into an absolute shell path. The Windows list
  now targets PowerShell aliases and abbreviated parameters instead of
  cmd.exe switches. Existing configs keep the list onboard wrote for them;
  README's Upgrading section explains how to adopt the new rules.
- A malformed tool-call argument string no longer loses the whole turn from
  history; the provider boundary normalizes it so the turn stays persistable.
- `/new` and `/stop` can no longer lose to a message the worker has just
  dequeued, and the Telegram update pump no longer stalls for ten seconds
  when that race hits.
- Replies within one chat are delivered in turn order.
- The Telegram client now honors context cancellation, so shutdown no longer
  waits up to thirty seconds for an in-flight long poll. The 5xx retry is
  real now, driven by a small custom API caller.
- A `storage.dsn` containing `#`, `?`, or `%` no longer lands the database at
  a truncated, world-readable path.
- `cron run --deliver` validates its target before any model call, and a
  manual run honors the job's timeout.
- Doctor checks the storage directory instead of creating `~/.mtclaw`.
- The cron scheduler evaluates every wall-clock minute and re-aligns each
  loop, so a phase shift can no longer skip a minute silently.

## Controller decisions

- Replies are strictly ordered per chat; delivery stays detached from the
  shutdown drain.
- The 5xx retry was made real rather than deleted.
- `cron run --deliver` validates the target regardless of enabled state and
  does not refuse disabled jobs.
- Retention is documented only. A pruning feature is left as a product
  decision.
- No doctor check was added for configs carrying the older deny list; the
  README upgrade note covers it.
- Telegram's post-parse character limit (over-splitting code-heavy replies)
  and a Windows `CREATE_SUSPENDED` job assignment were deferred as
  documented best-effort limits.

## Controller additions

- `.golangci.yml` with the standard linter set and errcheck exclusions for
  deferred `Close`, diagnostic prints, `os.Remove`, and `syscall.Flock`.
  `make lint` runs `go vet` then `golangci-lint run`; CI's ubuntu leg runs
  the same via `golangci/golangci-lint-action@v9`. The tree is at zero lint
  issues.
- The force-push rule only allows git's own global options between `git`
  and `push`, so `git commit -m 'push +1'` is no longer denied. Two corpus
  entries pin that.
- `onboard` falls back to rename when the filesystem rejects the hard link.

## Validation

All green on the final tree: `gofmt -l .`, `go mod tidy -diff`, `go vet`
on linux and `GOOS=windows`, `make lint`, `CGO_ENABLED=0 go build ./...`,
and `go test -race -count=1 ./...` across all fifteen test packages. The
config docs-coverage test passes against the updated documentation. Every
doc file is under 800 lines.

## Unresolved questions

1. Should history, exec audit, cron runs, and approvals get a retention
   setting, or stay document-only?
2. Should doctor warn when a config's deny list predates the new default
   shapes?
3. The repository has no tags, so README's release install path leads to an
   empty page until a first tag is pushed.
4. The Windows deny rules and Job Object behavior were verified at regex and
   compile level only; no Windows host was available.
