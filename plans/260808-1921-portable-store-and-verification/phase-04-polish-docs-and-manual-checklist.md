# Phase 4: Polish, Docs, Manual Checklist

**Status:** Completed · **Effort:** 2h · **Blocks:** none · **Blocked by:** phase 3

Accept `config.yml` as an extension alias, bring the docs in line with the new
store seam, and write down — precisely, with commands — what still cannot be
verified without live credentials.

## Context

- Config path resolution: `internal/config/paths.go:21-47`. The default branch
  returns `~/.mtclaw/config.yaml` unconditionally at `:35`; the flag/env branch
  (`:38-46`) expands and absolutizes whatever the user named.
- Flag help text mentions only `.yaml`: `internal/cli/root.go:89`.
- `onboard` writes to whatever path was resolved
  (`internal/cli/onboard_cmd.go:405-424`) and refuses to overwrite
  (`onboard_test.go:225`).
- Docs to refresh: `docs/architecture.md:53-57` (store package description),
  `:41-78` (package boundaries block), `docs/configuration.md:1-20` (intro
  mentions the file name).
- What the v1 plan still lists as unverified:
  `plans/260731-2219-mtclaw-core-system/plan.md:208-226`, with detail in
  `plans/reports/phase-09-implementation-260801-hardening-release-report.md`.
- Phase 1 closed the CI-verifiable parts of those items. This phase records
  the residue honestly rather than ticking it.

## Requirements

1. `~/.mtclaw/config.yml` is found when `config.yaml` is absent, with
   deterministic precedence and no ambiguity when both exist.
2. An explicitly named `--config`/`MTCLAW_CONFIG` path is never
   extension-rewritten.
3. `docs/architecture.md` describes the real post-refactor package layout,
   including the rule that keeps downgrades safe.
4. `docs/verification.md` exists: what CI proves, and the exact remaining
   manual steps with commands and expected output.
5. The v1 plan's checkbox list is updated to reflect what phase 1 actually
   asserts — no box ticked without a named test.

## Files

**Create**

| Path | Contents |
|---|---|
| `docs/verification.md` | Two sections: "Verified automatically" (test name → criterion) and "Requires live credentials" (numbered manual procedures). |

**Modify**

| Path | Change |
|---|---|
| `internal/config/paths.go:21-47` | Default branch: prefer an existing `config.yaml`; else an existing `config.yml`; else `config.yaml` (so a fresh machine's `onboard` still writes `.yaml`). Warn to stderr when both exist. |
| `internal/config/load_test.go` | New `ConfigPath` cases: yaml-only, yml-only, both, neither, explicit flag ending `.yml`, explicit flag with no matching file. (There is no `paths_test.go`; `ConfigPath` coverage lives with the other loader tests.) |
| `internal/cli/root.go:89` | Flag help: `~/.mtclaw/config.yaml` (or `config.yml`). |
| `docs/configuration.md:1-20` | Intro: mention the `.yml` alias and its precedence. |
| `docs/architecture.md:53-57` | Rewrite the `store/` bullet: interfaces + generic SQL implementation + portable `migrations/` + `dialect.go`/`factory.go`, with `sqlite/` holding driver quirks and registering itself. |
| `docs/architecture.md:41-78` | Add `testsupport/` to the layout block; note it is imported only by tests. |
| `docs/architecture.md` (new short section) | "Schema versioning": `schema_migrations` is authoritative; the SQLite dialect keeps `PRAGMA user_version` in sync with the highest applied version **so an older binary's downgrade guard still fires** — a rule a future migration author must not break. |
| `plans/260731-2219-mtclaw-core-system/plan.md:194,196,203,208-226` | Tick only what phase 1 asserts, cite the test name inline, and rewrite the "Remaining manual verification" section to point at `docs/verification.md`. |

**Delete** — none.

## Design

### `.yml` alias precedence

Only the **default** path is alias-resolved. Explicit input is honoured
verbatim — a user who types `--config foo.yml` gets `foo.yml`, and a typo is an
honest "file not found" rather than a surprise fallback.

| `config.yaml` | `config.yml` | Result |
|---|---|---|
| exists | — | `config.yaml`, source `default` |
| absent | exists | `config.yml`, source `default` |
| exists | exists | `config.yaml` + one stderr warning naming the ignored file |
| absent | absent | `config.yaml` (so the not-found error and `onboard` both name the canonical file) |

This is the one place in the codebase where `ConfigPath` touches the
filesystem. Document that in its doc comment: it was previously pure
(`paths.go:14-20` promises only `os.UserHomeDir`), and the change is
deliberate.

## Implementation steps

1. **`ConfigPath` alias.** Implement the table above in the default branch of
   `paths.go:30-36`. Use `os.Stat`; treat any error other than `ErrNotExist` as
   "absent" rather than failing config resolution over a stat hiccup.
2. **Tests** for all six cases, using `t.Setenv` on `HOME`/`USERPROFILE`
   (reuse `testsupport.FakeHome` from phase 1) so no real home directory is
   touched. Assert the returned `source` string is still `"default"` for both
   extensions — callers key off it.
3. **Help text and configuration.md intro.**
4. **`docs/architecture.md`.** Update the `store/` description, the layout
   block, and add the schema-versioning section. Re-read the surrounding prose
   before editing: the dependency-direction paragraph (`:80-85`) is still
   accurate and must not be casually reworded.
5. **`docs/verification.md`.** Section A, "Verified automatically": one row per
   closed criterion → the test that closes it, e.g.
   `mtclaw prompt tool-using turn → internal/cli/prompt_e2e_test.go::TestE2EPromptToolUsingTurn`.
   Section B, "Requires live credentials": the residue, each with a command
   block and an expected-output line. At minimum:
   1. Real OpenAI turn (`mtclaw doctor` OpenAI rows + one `mtclaw prompt`) —
      needs a paid key; the fake server cannot prove tool-call formatting
      against the real model.
   2. Real Telegram DM round trip and `/whoami` `/new` `/status` against a
      real chat — needs a bot token; also the only way to confirm MarkdownV2
      rendering as Telegram actually renders it.
   3. Real cron fire on a real minute boundary — phase 1 uses a fake clock,
      so the minute-alignment path (`cron/scheduler.go:114,139-142`) is
      unexercised.
   4. `onboard` on a clean machine, including the Telegram user-ID capture
      window (`internal/channel/telegram/capture.go:26-40`), which needs a real
      bot receiving a real message.
   5. Token-leak log grep (v1 security checklist #14) with a real token
      present in the environment.
   6. Tagged release producing five binaries.
   Each entry states *why* it cannot be faked, in one line. If an entry cannot
   justify itself, it belongs in Section A instead — write the test.
6. **Update the v1 plan's checkboxes.** Tick `:194` (prompt tool-using turn),
   `:196` (gateway DM round trip), `:203` (cron delivery) **only** with an
   inline citation of the phase 1 test that asserts it, and add a one-line
   note that the assertion is against a fake upstream, with the live step
   remaining in `docs/verification.md`. Do not tick the clean-machine onboard
   item — phase 1 does not close it.
7. **Final sweep.** Re-read `docs/configuration.md`, `docs/architecture.md`,
   `docs/security.md`, and `README.md` for any claim this plan invalidated
   (search for `user_version`, `storage.path`, `sqlite.Open`,
   `config.yaml`). Fix or delete; do not leave a stale sentence standing
   because it is out of scope.

## Tests / validation

```powershell
go test -race ./internal/config/...
go test -race ./...
$env:CGO_ENABLED=0; go build ./...

# alias behaviour, by hand, against a scratch home
$env:USERPROFILE = "$env:TEMP\mtclaw-alias"
New-Item -ItemType Directory -Force "$env:USERPROFILE\.mtclaw" | Out-Null
Copy-Item .\testcfg.yaml "$env:USERPROFILE\.mtclaw\config.yml"
go run . config path            # -> ...\.mtclaw\config.yml (source: default)
go run . config validate        # loads the .yml
Copy-Item .\testcfg.yaml "$env:USERPROFILE\.mtclaw\config.yaml"
go run . config path            # -> config.yaml, plus a warning naming config.yml

# docs sanity
rg -n "user_version|storage\.path|sqlite\.Open" docs\ README.md
```

## Success criteria

- [x] `~/.mtclaw/config.yml` loads when `config.yaml` is absent; `mtclaw config path` reports it with source `default` - verified by `TestConfigPath_DefaultAliasPrecedence`/"yml only" and by hand with a real binary
- [x] With both present, `.yaml` wins and one stderr warning names the ignored `.yml` - verified by the same test's "both present" case and by hand
- [x] With neither present, the not-found error names `config.yaml` - verified by the same test's "neither present" case (the reported default path names `config.yaml`, which is exactly what `LoadFile`'s `FileNotFoundError` then echoes)
- [x] `--config foo.yml` and `MTCLAW_CONFIG=foo.yml` load `foo.yml`; neither is extension-rewritten - verified by `TestConfigPath_ExplicitFlagNeverAliasResolved` and by hand with both flag and env var
- [x] Six `ConfigPath` test cases pass and touch no real home directory - `TestConfigPath_DefaultAliasPrecedence` (4 subtests) + `TestConfigPath_ExplicitFlagNeverAliasResolved` (2 subtests), all via `testsupport.FakeHome`/`t.TempDir`
- [x] `docs/architecture.md` describes the post-refactor `store/` layout and contains the schema-versioning rule about `PRAGMA user_version` - both added
- [x] `docs/verification.md` exists; every Section A row names a real test that exists; every Section B entry has a command and a one-line reason it cannot be faked
- [x] `plans/260731-2219-mtclaw-core-system/plan.md` boxes are ticked only where a named phase 1 test asserts them
- [x] `rg -n "user_version|storage\.path|sqlite\.Open" docs\ README.md` returns only intentional, still-true mentions - re-run, confirmed (see phase 4 implementation report)
- [ ] `go test -race ./...` green - **not verified; left unticked.** `-race` cannot run in this environment at all (no C compiler, `CGO_ENABLED=0`); CI is configured to run it but its last actual run failed for two pre-existing, out-of-scope reasons unrelated to this phase - see `docs/verification.md`'s CI note. `CGO_ENABLED=0 go build ./...` itself is green.

## Risks and rollback

| Risk | Mitigation |
|---|---|
| `.yml` alias makes `ConfigPath` filesystem-dependent, breaking its purity contract (`paths.go:14-20`) | Doc comment updated to say so; stat errors other than `ErrNotExist` are treated as absent so resolution never fails on a transient error; tests use a fake home. |
| Both files present and the user edits the ignored one | `.yaml` wins deterministically and the warning names the ignored file by full path. |
| `docs/verification.md` becomes a place to park work rather than do it | Every Section B entry must state why it cannot be faked; anything that fails that test becomes a phase 1-style test instead. |
| Ticking v1 plan boxes overstates what a fake upstream proves | Each tick carries an inline test citation plus a "fake upstream" note; the clean-machine onboard box stays unticked. |

**Rollback:** fully additive apart from the `ConfigPath` default branch.
Reverting restores unconditional `config.yaml`; a user who renamed their file
to `.yml` would need `--config` until they rename back — call that out in the
release notes.
