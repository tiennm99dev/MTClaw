# Phase 1 Implementation Report - Foundation and Config

- Plan: `plans/260731-2219-mtclaw-core-system/plan.md`
- Phase: `plans/260731-2219-mtclaw-core-system/phase-01-foundation-and-config.md`
- Status: **completed**
- Date: 2026-08-01

## Files created

- `go.mod` (module `github.com/tiennm99/MTClaw`, `go 1.25`), `go.sum`
- `main.go` - 6 lines: calls `cli.Execute()`, `os.Exit(1)` on error
- `internal/version/version.go` - `Version`/`Commit`/`Date` vars + `String()`
- `internal/logging/logger.go` - `New(config.LogConfig) (*slog.Logger, error)`, text/json handler, file or stderr sink
- `internal/config/types.go` - full schema structs, `Duration` wrapper (`UnmarshalYAML`/`MarshalYAML`), unexported secret fields + accessors (`APIKey()`, `APIKeySource()`, `Token()`, `TokenSource()`)
- `internal/config/defaults.go` - `Default() *Config`
- `internal/config/paths.go` - `ConfigPath()`, `StateDir()`, `ExpandPath()`, `expandTilde()`
- `internal/config/load.go` - `Load()` (pure: bytes + baseDir + env map), `LoadFile()`, `resolveSecrets()`, `expandConfigPaths()`, `MarshalRedacted()`, `FileNotFoundError`
- `internal/config/validate.go` - `Validate()`, `ValidationErrors` (multi-error, `Unwrap() []error`)
- `internal/config/load_test.go` - 267 lines, table + dedicated tests (see below)
- `internal/config/validate_test.go` - 231 lines, per-rule tests
- `internal/cli/root.go` - cobra root, `--config`/`--log-level` persistent flags, `PersistentPreRunE` gate
- `internal/cli/config_cmd.go` - `config path|show|validate`
- `internal/cli/version_cmd.go` - `version`
- `.gitignore`, `Makefile` (`build`/`test`/`lint`/`fmt`/`install`/`clean`, `CGO_ENABLED=0` everywhere, ldflags version stamping)

## Files modified

- `README.md` - replaced the one-line stub with install + quickstart (brief; full rewrite deferred to phase 9 per plan)

## Tasks completed

- [x] `go mod init`, `go 1.25` directive, cobra + goccy/go-yaml + testify deps
- [x] `internal/version` package-level vars, ldflags wiring verified manually (`go build -ldflags ...` produces correctly stamped `version` output)
- [x] `internal/logging` slog builder (text/json, file-or-stderr, tolerant level parsing)
- [x] `internal/config/types.go` full schema mirror, `Duration` wrapper type, unexported secret fields + accessors
- [x] `internal/config/paths.go` - flag > `MTCLAW_CONFIG` > `~/.mtclaw/config.yaml`, tilde expansion, config-relative resolution
- [x] `internal/config/load.go` - strict decode (`DisallowUnknownField()` + `Strict()`) onto `Default()`, secret resolution, path expansion, validation
- [x] `internal/config/validate.go` - all listed rules, accumulating every failure via `ValidationErrors`
- [x] `internal/cli/root.go` - persistent flags, `PersistentPreRunE` skips config load only for `version` and `config path`
- [x] `internal/cli/config_cmd.go` - `path` (prints path + source), `show` (redacted YAML), `validate` (reaching `RunE` implies success, since `PersistentPreRunE` already ran `Load`→`Validate`)
- [x] `main.go` thin entrypoint
- [x] `Makefile`, `.gitignore`, `CGO_ENABLED=0` throughout

## Tests status

- Type check / build: **pass** - `CGO_ENABLED=0 go build ./...` clean
- `go vet ./...`: **pass**, no findings
- `gofmt -l .`: **clean** (no files listed)
- `go test ./... -count=1`: **pass**, `internal/config` package only has tests (cli/version/logging have none, matching phase scope - no test files required by the phase for those thin packages)

Manual CLI smoke tests (all matched expected behavior):
- `mtclaw version` works with no config file present
- `mtclaw --config ./other.yaml config path` resolves to an absolute path and reports `source: flag`
- `mtclaw config validate` against a minimal valid config prints `OK: <path>`
- Unknown top-level key -> decode error with `[line:col]` position and source excerpt (goccy's built-in formatting)
- Inline `openai.api_key` -> validation error naming `openai.api_key_env`, plus (correctly) also flags the resulting empty allowlist in the same pass
- `telegram.enabled: true` with only the default `"*"` group (empty `allow_from`) -> "would accept no one" error (S5 amendment verified)
- `config show` with `OPENAI_API_KEY` set in the environment -> prints `api_key: <set:env:OPENAI_API_KEY>`, never the raw key value

### Table tests implemented (internal/config)

`load_test.go` `TestLoad_TableDriven`: valid minimal config; unknown top-level key; **unknown nested key** (dedicated case proving `DisallowUnknownField` recurses into nested structs - the phase's specific risk callout); bad duration; inline secret naming `*_env`; empty allowlist with only the default group; bad regex in `exec.deny`; cron job with a garbage `schedule` string that **must not** fail (proving expression validation is correctly deferred to phase 8); `exec.cwd` outside `filesystem.roots`.

Plus dedicated tests: `~` expansion (`TestLoad_TildeExpansion`), config-relative path resolution (`TestLoad_ConfigRelativePathResolution`), env-overlay precedence (`TestLoad_SecretEnvOverlay`: env-set / neither-set / file-set-with-trailing-LF / file-set-with-trailing-CRLF / both-set-env-wins), `FileNotFoundError` typed-error check, and two `config show` snapshot tests (`TestMarshalRedacted_NeverLeaksSecretMaterial`, `TestMarshalRedacted_UnsetSecretsShownAsUnset`) asserting the actual secret strings never appear in rendered output.

`validate_test.go`: one test per rule (version, agent fields as a 4-violation multi-error assertion, openai base_url/timeout, inline secrets, telegram allowlist - both failure and the two ways it can be satisfied, group key parsing incl. the positive-id hint, exec mode enum, deny/allow regex compilation, filesystem roots required, exec.cwd containment, cron timezone, cron job uniqueness/prompt/deliver_to, cron schedule explicitly NOT validated, and both branches (pass/fail) of the storage.path parent-directory-creatable check).

## Deviations from the phase file

1. **`api_key`/`token` inline-secret detection.** The phase file doesn't spell out the mechanism; strict decode alone would reject an inline `api_key:` as an opaque "unknown field" error, not one "naming the correct `*_env` key" as the acceptance criteria require. Resolved by adding `APIKeyInline`/`TokenInline` as real (but validation-forbidden) struct fields tagged `yaml:"api_key,omitempty"` / `yaml:"token,omitempty"`, so decode succeeds and `validate.go` can produce the actionable message. `config show` reuses these same fields transiently to render the `<set:env:...>`/`<unset>` placeholder in the same YAML position a literal secret would have occupied.
2. **World-readable `*_file` check on Windows.** Implemented as a documented no-op on Windows (`runtime.GOOS == "windows"`) rather than an approximate warning, since `os.FileMode` from `os.Stat` on Windows does not reflect real ACL-based permissions and any check would be either always-true or always-false noise. POSIX enforcement (`warnIfWorldReadable`) is implemented but untestable on this Windows dev machine; the logic mirrors the standard `mode.Perm()&0o004 != 0` check.
3. **`storage.path` parent-directory-creatable check has a side effect.** Implemented via `os.MkdirAll` (idempotent) rather than a side-effect-free dry-run, since a genuinely side-effect-free "would this succeed" check is platform-dependent and the phase file's literal wording ("parent dir creatable") reads as an attempted-creation check. This means every command that loads config (including read-only ones like `config show`) will create `~/.mtclaw/` (or wherever `storage.path`'s parent resolves to) if it doesn't already exist. Flagging this because it's a real, if minor, side effect on first run.
4. **CronJob schema fields** (`schedule`, `session`, `timeout`, `deliver_to.{channel,chat_id}`) were fully defined in `types.go` now, even though phase 1's own schema example shows only `jobs: []`. This was necessary because phase 8's "Related Code Files" list only modifies `internal/config/validate.go`, never `types.go` - so the complete `CronJob` shape had to already exist for phase 8 to compile against. Field names/shapes were taken directly from phase 8's documented YAML example to keep the contract consistent.
5. **`go.mod`** shows `go 1.25` (the plan's floor) even though the installed toolchain is 1.26.4; no `toolchain` directive was added since 1.26.4 already satisfies `go 1.25`.

No other scope was added; nothing from later phases (store, provider, telegram, tools, cron scheduler logic) was implemented.

## Pre-existing, unrelated working-tree state (not part of this phase's changes)

`git status` shows several `plans/*.md` files (phase-02, 04, 05, 06, 07, 08, and `plan.md`) as modified. Diffing confirms these are the plan's own documented "Session - 2026-08-01 (pre-implementation re-review)" S1-S9 amendments (e.g. the exact S5 allowlist wording I implemented against) already present in the working tree before this session started - I did not author or touch these edits, and per instructions nothing was committed.

## Success criteria checklist (from the phase file)

- [x] `CGO_ENABLED=0 go build ./...` succeeds
- [x] `mtclaw version` works with no config file present
- [x] `mtclaw config validate` reports every error in one pass with line numbers (decode-stage errors carry `[line:col]`; validate.go errors carry the dotted key path - see note below)
- [x] An unknown YAML key fails the load
- [x] An inline `api_key` fails the load with a message naming `openai.api_key_env`
- [x] Telegram enabled with an empty allowlist fails the load (including the amended default-`"*"`-group case)
- [x] `mtclaw config show` never prints secret material
- [x] `mtclaw --config ./other.yaml config path` resolves and reports the flag as the source

Note on "line numbers": decode-time errors (unknown key, bad duration, bad YAML syntax) carry goccy's native `[line:col]` + source excerpt. Semantic `validate.go` rules (e.g. `agent.max_iterations` out of range) operate on the already-decoded struct and report a dotted key path instead of a line number, since recovering AST positions post-decode for semantic checks was judged out of scope for phase 1 (the phase file's "where possible" qualifier). Flagging as a judgment call rather than a gap.

## Unresolved questions

None blocking. The two "decided-with-assumption" items from plan.md's Open Questions section (no hardcoded default model; `mtclaw send`/`cron run` as direct API calls) don't affect phase 1 and were left as documented.

Status: DONE
Summary: Phase 1 foundation (go module, cobra CLI, structured logging, YAML config load/validate pipeline) implemented per spec; build/vet/fmt/tests all green; all phase acceptance criteria manually and automatically verified.
Concerns/Blockers: None. Two design judgment calls documented above (side-effecting storage-dir creation during validate; validate.go errors use key paths rather than line numbers) worth a quick nod from the user/planner but do not block phase 2/3.
