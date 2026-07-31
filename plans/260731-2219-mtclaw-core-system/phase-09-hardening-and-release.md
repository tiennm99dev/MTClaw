---
phase: 9
title: "Hardening and Release"
status: pending
priority: P2
dependencies: [7, 8]
effort: ""
---

# Phase 9: Hardening and Release

## Overview

Close the loop: `onboard` and `doctor` so a new machine gets to a working gateway
without reading source, the documentation set, CI, and cross-compiled release
binaries. Also the pass where the security posture gets verified rather than assumed.

## Requirements

**Functional**
- `mtclaw onboard` — interactive first-run setup writing a valid config.
- `mtclaw doctor` — diagnose every external dependency and misconfiguration.
- Complete docs: configuration reference, security model, Telegram setup, architecture.
- CI: build, vet, test on Linux/macOS/Windows.
- Release: static binaries for linux/darwin/windows × amd64/arm64.

**Non-functional**
- A user with no Go toolchain can install and run from a release artifact.
- `go test ./...` green on all three OSes.
- No secret material in any log line, error message, or doc example.

## Architecture

### `mtclaw onboard`

Interactive, idempotent, and refuses to clobber: if a config exists, it offers to show
what would change rather than overwriting. Sequence:

1. Detect OS → pick the default shell argv and the OS-appropriate deny-list.
2. Prompt for the OpenAI key. **Never write the key into the config** — write
   `api_key_env: OPENAI_API_KEY` and print the exact `export`/`setx` line the user must
   add to their shell profile. Then verify the key from the current environment if
   present.
3. Prompt for the model. Verify it by listing models and confirming the name exists,
   so a typo fails at setup rather than at the first message.
4. Prompt for the Telegram token, same env-indirection treatment
   (`token_env: TELEGRAM_BOT_TOKEN`). Call `getMe` and print the bot username.
5. **Capture the user's Telegram ID interactively, with explicit confirmation.** This is
   the step that makes the fail-closed allowlist survivable — and it is also a
   privilege-assignment window, so it must not auto-trust. Telegram bot usernames are
   publicly discoverable, so whoever messages the bot during this window is a candidate
   owner of a process that can run shell commands.
   - Start a temporary long poll and print "send any message to @yourbot now".
   - Collect messages for the full window rather than returning on the first one.
   - **Print the sender's username and numeric ID and require an explicit yes** before
     writing it to `allow_from`. Never write an ID the user has not confirmed on screen.
   - If **more than one distinct sender** appears, show all of them, write none
     automatically, and make the user choose — a second sender means someone else found
     the bot.
   - Fall back to manual entry with a `/whoami` pointer if nothing arrives in 60s.
6. Prompt for the workspace directory; create it.
7. Write the config with `exec.mode: approval` and the OS deny-list. **Never offer
   `auto`** — it is documented, not onboarded.
8. Write a starter `~/.mtclaw/prompts/AGENTS.md` and reference it from
   `agent.system_prompt_files`.
9. Run `doctor` and print next steps.

Config is written with `0600` on POSIX. Even without inline secrets it contains the
allowlist and workspace layout.

### `mtclaw doctor`

Every check reports OK / WARN / FAIL with an actionable next step, and the command
exits non-zero if anything FAILs:

| Check | Fails when |
|---|---|
| Config found and valid | parse or validation error (prints the same multi-error as `config validate`) |
| Config file permissions | world-readable on POSIX (WARN) |
| State dir writable | cannot create/write `~/.mtclaw` |
| DB opens and migrates | migration error, or schema newer than the binary |
| Instance lock | another gateway holds it (INFO, not FAIL — expected while running) |
| OpenAI key resolves | env var and file both empty; reports which source won |
| OpenAI reachable | minimal completion fails; reports base URL, model, latency |
| Model exists | configured model not in the models list (WARN — new models can lag the list) |
| Telegram token resolves | as above |
| Telegram `getMe` | invalid token; prints bot username on success |
| Allowlist non-empty | empty with telegram enabled |
| Workspace exists and writable | missing or read-only |
| `filesystem.roots` exist | any root missing |
| `exec.cwd` inside a root | escaped confinement |
| Shell exists | `exec.shell[0]` not on PATH |
| Deny-list sanity | **empty deny-list with exec enabled (WARN, loudly)**; every pattern compiles |
| `exec.mode: auto` | **always WARN**: "auto mode is beta; the classifier is not a security control" |
| Cron | timezone loads; every expression valid; every `deliver_to` reachable; prints next due times |

`doctor --json` for scripting.

### Documentation

Under `docs/`, each with a stated purpose so they do not drift into overlap:

- `docs/configuration.md` — every key: type, default, effect, and what breaks if wrong.
  Generated-adjacent: keep a test asserting every field in the config struct appears in
  this file, so new keys cannot ship undocumented.
- `docs/security.md` — the phase-5 threat model verbatim, the exec decision pipeline
  diagram, what the deny-list does and does not stop, why `auto` is beta, why cron is
  allow-list-only, and the plain statement that the bot token is equivalent to shell
  access.
- `docs/telegram-setup.md` — BotFather walkthrough, `/setprivacy` and the re-add
  requirement, finding user and group IDs via `/whoami`, the `-100` supergroup form,
  `require_mention` behaviour.
- `docs/architecture.md` — the plan's component diagram, package boundaries, the
  request lifecycle, and a short "what we deliberately did not build" list pointing at
  openclaw/goclaw for the full-featured versions.
- `README.md` — what it is, the one-paragraph security warning, install, 5-minute
  quickstart, command list, link to the docs.

### CI and release

GitHub Actions, two workflows:

- `ci.yml` on push/PR: matrix over ubuntu/macos/windows × latest Go. Steps: `go vet`,
  `go build`, `go test -race ./...`, `gofmt -l` check. `CGO_ENABLED=0` throughout.
- `release.yml` on tag: cross-compile the matrix below, stamp version via ldflags,
  produce checksums, attach to the release.

| GOOS | GOARCH |
|---|---|
| linux | amd64, arm64 |
| darwin | amd64, arm64 |
| windows | amd64 |

`CGO_ENABLED=0` is what makes this a plain matrix loop instead of a cross-toolchain
problem — the reason `modernc.org/sqlite` was chosen in phase 2.

`-race` matters here specifically: phase 7's worker map and phase 8's overlap flag are
exactly the code a race detector catches and review does not.

## Related Code Files

- Create: `internal/cli/onboard_cmd.go`, `internal/cli/onboard_prompts.go`
- Create: `internal/cli/doctor_cmd.go` — check registry, table and JSON output
- Create: `internal/cli/doctor_checks.go` — one function per check
- Create: `internal/cli/doctor_test.go`, `onboard_test.go`
- Create: `docs/configuration.md`, `docs/security.md`, `docs/telegram-setup.md`, `docs/architecture.md`
- Create: `internal/config/docs_coverage_test.go` — asserts every config field is documented
- Create: `.github/workflows/ci.yml`, `.github/workflows/release.yml`
- Create: `internal/cli/prompts/AGENTS.md` — embedded starter system prompt
- Modify: `README.md` — full rewrite from the two-line stub
- Modify: `Makefile` — `release` target mirroring CI locally

## Implementation Steps

1. Extract the doctor checks into a `[]Check` where each is
   `struct{ Name string; Run func(ctx, *config.Config) Result }`, so `onboard` reuses
   them and tests can run individual checks. Do this before writing the checks, not after.
2. Implement checks in the table order. Every FAIL message must contain the fix, not just
   the symptom: "Telegram allowlist is empty — run `mtclaw onboard` or add your user ID
   to `channels.telegram.allow_from` (get it with `/whoami`)".
3. `onboard`: the sequence above. Keep prompting in `onboard_prompts.go` behind a small
   interface so tests drive it with scripted answers rather than a TTY.
4. The interactive Telegram ID capture per step 5 above: temporary bot, 60s long poll,
   collect all senders, explicit on-screen confirmation before writing, multiple senders
   force a manual choice. There is no clock-skew check — an earlier draft proposed
   inferring skew from a Telegram response `Date` header, which is header-parsing
   machinery for a problem that announces itself.
5. Write the docs. `docs/security.md` is the one that must not be softened — it exists so
   a user can make an informed decision about running this at all.
6. `docs_coverage_test.go`: reflect over the config structs, collect every YAML key path,
   assert each appears verbatim in `docs/configuration.md`. Fails on any new undocumented key.
7. CI workflows. Cache modules. Upload test output on failure.
8. Release workflow with the matrix, ldflags stamping
   (`-X internal/version.Version=$TAG -X …Commit -X …Date`), `-trimpath`, `-s -w`,
   SHA256SUMS, artifacts attached to the GitHub release.
9. Full manual verification pass on a clean machine (or fresh container + a real bot
   token): download the binary, `onboard`, `doctor`, `gateway`, DM the bot, run a tool, hit
   an approval, deny it, approve one, restart and confirm history persisted, set a
   `* * * * *` cron job and watch it deliver, then Ctrl-C.
10. Security verification pass, executed and recorded rather than assumed — the list below.

### Security verification checklist

Run each and record the result in the phase report:

- [ ] Deny-listed command from Telegram: refused, no prompt shown, `exec_audit` row present
- [ ] Same command with an allow-list entry added that would match: still refused (deny precedence)
- [ ] `rm --recursive --force /tmp/probe` (long options): refused
- [ ] `/bin/rm -rf /tmp/probe` (path-prefixed): refused
- [ ] `web_fetch` against `http://127.0.0.1:<open port>`: refused
- [ ] `web_fetch` against `http://[::ffff:127.0.0.1]`: refused
- [ ] `web_fetch` against `http://169.254.169.254` (metadata): refused
- [ ] `web_fetch` against a public URL that 302s to `http://127.0.0.1`: refused
- [ ] `read_file` with `../../../../etc/passwd`: refused
- [ ] `read_file` via a symlink inside the workspace pointing to `/etc/passwd`: refused
- [ ] Non-allowlisted user messaging the bot: no reply, no session created
- [ ] Non-allowlisted group member tapping Approve on someone else's prompt: rejected
- [ ] Allowlisted user in a *different* chat tapping the callback: rejected
- [ ] `grep -ri` the logs from a full session for the bot token and API key: zero hits
- [ ] `mtclaw config show` output checked for secret material: zero hits
- [ ] A command containing an inline bearer token: neither the Telegram approval message nor the `exec_audit` row contains the secret
- [ ] `/stop` during a long-running command: the OS process and its children are gone
- [ ] SIGTERM with an approval prompt unanswered: process exits within the drain deadline, not after `approval_timeout`
- [ ] `onboard` with a second sender messaging the bot during the capture window: no ID written automatically, user forced to choose
- [ ] `auto` mode with a classifier forced to error: prompts rather than running
- [ ] Cron turn attempting an unmatched command: refused with the no-approver message

## Tests / Validation

- Doctor checks unit-tested individually with fabricated configs; a full-FAIL run exits
  non-zero; `--json` output parses.
- `onboard` driven by scripted prompt answers: writes a config that
  `config.Load` + `Validate` accept; refuses to overwrite an existing file; never writes a
  key or token value into the YAML (asserted by scanning the written bytes for the secrets
  it was given).
- Docs coverage test passes and demonstrably fails when a field is added without docs.
- `go test -race ./...` on all three OSes in CI.
- Release workflow dry-run produces all six binaries; each `--version` reports the tag.

## Success Criteria

- [ ] A clean machine reaches a working Telegram round trip using only the README and `onboard`
- [ ] `onboard` writes no secret into the config file (asserted in a test)
- [ ] `onboard` captures the user's Telegram ID interactively and never writes one without on-screen confirmation
- [ ] `doctor` catches every failure in its table with an actionable message and exits non-zero
- [ ] `doctor` warns on empty deny-list and on `auto` mode
- [ ] Every config key is documented; the coverage test enforces it
- [ ] `docs/security.md` states the exec threat model, deny-list limits, and token-equals-shell plainly
- [ ] `go test -race ./...` green on Linux, macOS, Windows
- [ ] Tagged release produces six static binaries plus checksums
- [ ] Every item in the security verification checklist executed and recorded
- [ ] `README.md` carries the security warning above the install instructions

## Risk Assessment

- **The "hardening" phase is where hardening actually gets skipped**, because the product
  already works after phase 7. The security verification checklist is therefore an explicit
  deliverable with recorded results, not a review activity. If phases get cut, this list
  survives.
- **`onboard` writing secrets** is the most likely way a key ends up in a git repo or a
  pasted gist. The env-indirection design prevents it structurally; the test asserting the
  written bytes contain neither secret is what keeps it that way.
- **Docs drift** is inevitable without enforcement, hence the coverage test. It only checks
  presence, not accuracy — accuracy stays a review concern.
- **Cross-compilation confidence.** CGO-free means the matrix builds, but "builds" is not
  "runs". At minimum run the linux/amd64 and the host binary; note untested targets in the
  release notes rather than implying they were verified.
- **Windows is the weakest target**: PowerShell quoting, no process groups in the POSIX
  sense, a different deny-list, and different path semantics in the guard. Either verify it
  properly here or state in the README that Windows is best-effort. Do not leave it ambiguous.
