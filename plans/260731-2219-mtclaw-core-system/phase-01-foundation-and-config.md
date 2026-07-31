---
phase: 1
title: "Foundation and Config"
status: pending
priority: P1
dependencies: []
effort: ""
---

# Phase 1: Foundation and Config

## Overview

Stand up the Go module, the cobra command skeleton, structured logging, and the
YAML config system that every later phase reads from. Ends with a binary that can
validate and print its own config but does nothing else.

## Requirements

**Functional**
- Single YAML config at `~/.mtclaw/config.yaml`, overridable by `--config` and `MTCLAW_CONFIG`.
- Env overlay for secrets only, resolved through indirection (`*_env` / `*_file` keys).
- Strict parsing: unknown keys are errors, not silently ignored.
- Validation errors report the YAML line/column and the offending key path.
- `mtclaw config path|show|validate`, `mtclaw version`, and a `--log-level` global flag.
- `mtclaw config show` redacts every resolved secret.

**Non-functional**
- No CGO. `go build` must work with `CGO_ENABLED=0`.
- Config load must be a pure function of (file bytes, env map, OS) so it is table-testable.

## Architecture

Precedence, highest first: **CLI flag → env var → config file → built-in default**.
Only these keys participate in the env layer, and only as *indirection* — the
secret itself is never a config value:

| Config key | Resolves from | Fallback |
|---|---|---|
| `openai.api_key_env` | named env var, default `OPENAI_API_KEY` | `openai.api_key_file` |
| `channels.telegram.token_env` | named env var, default `TELEGRAM_BOT_TOKEN` | `channels.telegram.token_file` |

A literal `api_key:` or `token:` key in the YAML is a **validation error**, not a
warning. Rationale: the config file is the thing users paste into issues and
gists. `*_file` values are read at load, trimmed of trailing newline, and must not
be world-readable on POSIX (mode check, warn only on Windows).

Path expansion: leading `~` expands to the user home dir; relative paths resolve
against the config file's directory, not the process CWD, so `mtclaw` behaves the
same from any directory.

### Config schema (complete)

```yaml
version: 1                          # schema version; load fails on unknown major

agent:
  name: MTClaw
  model: gpt-5                      # no hardcoded default in code; onboard writes this
  temperature: 0.7
  max_iterations: 20                # think/act/observe cap per turn
  max_history_turns: 40             # hard trim; the only history bound in v1
  workspace: ~/mtclaw-workspace
  system_prompt_files: []           # concatenated in order, e.g. [~/.mtclaw/prompts/AGENTS.md]

openai:
  api_key_env: OPENAI_API_KEY
  api_key_file: ""
  base_url: https://api.openai.com/v1
  timeout: 120s
  max_retries: 3

channels:
  telegram:
    enabled: true
    token_env: TELEGRAM_BOT_TOKEN
    token_file: ""
    allow_from: []                  # numeric user IDs. EMPTY = DENY ALL (fail closed)
    groups:                         # keys are chat IDs, e.g. "-1001234567890"
      "*":
        require_mention: true
        allow_from: []              # empty inherits channel allow_from

tools:
  filesystem:
    enabled: true
    roots: [~/mtclaw-workspace]     # all paths confined to these roots
    max_read_bytes: 262144
    max_write_bytes: 1048576
  web_fetch:
    enabled: true
    timeout: 30s
    max_bytes: 1048576
  exec:
    enabled: true
    mode: approval                  # approval | auto | off
    shell: []                       # empty = OS default; POSIX [/bin/bash, -lc]
    cwd: ~/mtclaw-workspace
    timeout: 120s
    max_output_bytes: 65536
    approval_timeout: 5m
    deny: []                        # regex, checked FIRST, always wins
    allow: []                       # regex, auto-run with no prompt
    auto:
      model: ""                     # empty = agent.model
      confirm_on: [destructive, privileged, network, secret_access]

cron:
  enabled: false
  timezone: Local                   # IANA name or "Local"
  jobs: []

storage:
  path: ~/.mtclaw/mtclaw.db

log:
  level: info                       # debug | info | warn | error
  format: text                      # text | json
  file: ""                          # empty = stderr
```

## Related Code Files

- Create: `go.mod`, `main.go`
- Create: `internal/version/version.go` — `Version`, `Commit`, `Date` set by `-ldflags`
- Create: `internal/logging/logger.go` — `slog` handler selection from `log.*`
- Create: `internal/config/types.go` — structs with `yaml:` tags, `Duration` wrapper type
- Create: `internal/config/defaults.go` — `Default() *Config`
- Create: `internal/config/paths.go` — `ConfigPath()`, `StateDir()`, `ExpandPath()`
- Create: `internal/config/load.go` — read → strict decode → env overlay → expand → validate
- Create: `internal/config/validate.go` — all rules, returns a multi-error
- Create: `internal/config/load_test.go`, `internal/config/validate_test.go`
- Create: `internal/cli/root.go`, `internal/cli/config_cmd.go`, `internal/cli/version_cmd.go`
- Create: `.gitignore`, `Makefile`
- Modify: `README.md` — replace the one-line stub with install + quickstart

## Implementation Steps

1. `go mod init github.com/tiennm99/MTClaw`, pin `go 1.25`. Add cobra and goccy/go-yaml.
2. `internal/version`: package-level vars stamped via ldflags; `Makefile` sets them from `git describe`.
3. `internal/logging`: build an `*slog.Logger` from level+format+file. `text` uses a compact handler; `file` opens append-only, creating parent dirs.
4. `internal/config/types.go`: mirror the schema above. Wrap `time.Duration` in a
   `Duration` type with `UnmarshalYAML` accepting `"120s"`, `"5m"`. Store secret
   *values* in unexported fields with accessor methods (`c.OpenAI.APIKey()`), so a
   plain struct dump cannot leak them.
5. `internal/config/paths.go`: `ConfigPath()` honours `--config` > `MTCLAW_CONFIG` >
   `~/.mtclaw/config.yaml`. `ExpandPath` handles `~` and config-relative resolution.
6. `internal/config/load.go`:
   - Read file. A missing file is a distinct typed error so `onboard` can offer to create it.
   - Decode with `yaml.NewDecoder(r, yaml.DisallowUnknownField(), yaml.Strict())` onto the defaults struct.
   - Resolve secrets: `*_env` then `*_file`. Record which source won for `doctor`.
   - Expand every path field.
   - Validate, return `(*Config, error)`.
7. `internal/config/validate.go` — accumulate all failures, do not stop at the first:
   - `version` must be `1`
   - `agent.model` non-empty; `max_iterations` in 1..100; `max_history_turns` >= 2; `temperature` in 0..2
   - `openai.base_url` parses as absolute http/https URL; timeout > 0
   - inline `api_key`/`token` keys present → error naming the correct `*_env` key
   - `telegram.enabled` with empty `allow_from` and no group entries → error: "would accept no one; set channels.telegram.allow_from"
   - group keys parse as int64; `-100…` supergroup form noted in the error text when a bare positive ID looks like a group
   - `tools.exec.mode` in {approval, auto, off}; every `deny`/`allow` entry compiles as a regex, with the failing pattern quoted
   - `tools.filesystem.roots` non-empty when filesystem enabled; each is absolute after expansion
   - `tools.exec.cwd` inside one of `filesystem.roots`, else error (prevents a shell that can escape the sandbox by default)
   - `cron.timezone` loads via `time.LoadLocation`; every job name unique and non-empty; every job has a non-empty `prompt` and a `deliver_to`. **Cron *expression* validation and `deliver_to` reachability are added in phase 8**, which is where `gronx` enters the dependency set — do not pull that dependency in early just to validate a feature that does not exist yet.
   - `storage.path` parent dir creatable
8. `internal/cli/root.go`: persistent flags `--config`, `--log-level`. A
   `PersistentPreRunE` loads config for every subcommand except `version` and
   `config path` (which must work with a broken or absent config).
9. `internal/cli/config_cmd.go`: `path` prints the resolved path and its source;
   `show` marshals the config back to YAML with secrets replaced by
   `<set:env:OPENAI_API_KEY>` or `<unset>`; `validate` prints either `OK` plus the
   resolved path or the full multi-error and exits 1.
10. `main.go` is three lines: call `cli.Execute()`, exit non-zero on error.
11. `Makefile`: `build`, `test`, `lint` (`go vet`), `fmt`, `install`. `CGO_ENABLED=0` everywhere.

## Tests / Validation

- Table tests over the loader: valid minimal config; unknown key; bad duration; inline secret; empty allowlist with telegram enabled; bad regex; bad cron expression; `exec.cwd` outside roots; `~` expansion; config-relative path resolution.
- Env overlay: `*_env` set / unset / `*_file` present / both present (env wins) / file with trailing newline.
- `config show` snapshot test asserting no secret material appears in the output.
- `CGO_ENABLED=0 go build ./...` in CI.

## Success Criteria

- [ ] `CGO_ENABLED=0 go build ./...` succeeds
- [ ] `mtclaw version` works with no config file present
- [ ] `mtclaw config validate` reports every error in one pass with line numbers
- [ ] An unknown YAML key fails the load
- [ ] An inline `api_key` fails the load with a message naming `openai.api_key_env`
- [ ] Telegram enabled with an empty allowlist fails the load
- [ ] `mtclaw config show` never prints secret material
- [ ] `mtclaw --config ./other.yaml config path` resolves and reports the flag as the source

## Risk Assessment

- **Config sprawl.** The schema is already near the edge of "simple". Any key added
  in a later phase must be justified against a use case, not added speculatively.
- **goccy/go-yaml strict-mode behaviour differs from yaml.v3** on embedded structs
  and `omitempty`. Verify `DisallowUnknownField` actually rejects nested unknown keys
  in a test before building the rest of the schema on that assumption.
- **Fail-closed allowlist is a usability cliff** — a fresh user with a valid token
  and no `allow_from` gets a bot that ignores them. Mitigated by making it a *load
  error* with an actionable message rather than silent runtime rejection, and by
  `onboard` capturing the user ID interactively.
