# MTClaw re-review — cli / config / logging / version / docs / build

Reviewer: code-reviewer · 2026-09-21 · advisory only, no files modified.

## Scope

Read in full: `main.go`, `internal/cli/*.go` (+tests, `prompts/AGENTS.md`),
`internal/config/*.go` (+tests), `internal/logging/logger.go`,
`internal/version/version.go`, `README.md`, `docs/{architecture,configuration,security,telegram-setup}.md`,
`Makefile`, `.github/workflows/{ci,release}.yml`, `go.mod`, `.gitignore`.
Cross-read for claim verification only: `internal/tools/{approver,registry,fs,exec,path_guard,web_fetch,classifier}.go`,
`internal/gateway/{gateway,shutdown,approver}.go`, `internal/agent/prompt.go`,
`internal/provider/{errors.go,openai/errors.go}`, `internal/store/sqlite/migrations/001_init.sql`.

Checks run (all from repo root, Go 1.27.1):

| Check | Result |
|---|---|
| `go vet ./...` | clean |
| `gofmt -l .` | clean |
| `go mod tidy -diff` | clean (no diff) |
| `go test ./internal/cli/... ./internal/config/...` | ok, ok |
| `go build .` + manual CLI probes | see evidence below |

Manual probes (binary built to scratchpad): exit codes, `config show` redaction,
multi-error aggregation, duration decoding, `MTCLAW_CONFIG` precedence, file modes.

## Overall assessment

Solid, unusually well-commented code; the comments are mostly accurate, which is
not the norm for AI-written Go. Error aggregation, secret redaction, fail-closed
approver defaults, and exit codes all behave as documented under test. No
trust-boundary defect found in this slice: `config show` leaks nothing
(verified), logs carry no message text or commands, provider errors render
`Msg` only, `send`/`cron run --deliver` cannot target a chat that validation did
not already accept.

The real problems are (a) a validation surface that stops one layer short —
every duration and byte cap except `openai.timeout` is unbounded, and one of them
panics the daemon; (b) no signal handling outside `gateway`, which defeats the
agent loop's own flush-on-cancel path; (c) three copies of the same
store/provider/registry/loop wiring.

No package here needs a rewrite. `config` needs two surgical fixes; `cli` needs
one small extraction plus a context change.

## Critical

None found in this slice.

## High

### H1. Every duration and byte cap except `openai.timeout` is unvalidated; a negative `max_read_bytes` panics the process

`internal/config/validate.go:86-99` bounds `openai.timeout` (`must be greater
than 0`) and nothing else. `validateTools` (`validate.go:141-185`) checks mode,
regex compilation, and cwd confinement only.

Verified empirically — this config **passes `mtclaw config validate` with exit 0**:

```yaml
tools:
  filesystem: { max_read_bytes: -5 }
  web_fetch:  { timeout: "0s" }
  exec:       { timeout: "0s", approval_timeout: "0s", max_output_bytes: 0 }
```

Runtime consequences, each traced to a line:

- `internal/tools/fs.go:106-113` — `limit := a.Limit; if limit <= 0 || limit > int64(f.maxReadBytes) { limit = int64(f.maxReadBytes) }` then `buf := make([]byte, limit)`. With `max_read_bytes: -5`, `make([]byte, -5)` panics (`makeslice: len out of range`). `grep -rn "recover()" internal/` returns **nothing** — the panic takes down the whole `mtclaw gateway` process, killing every concurrent session's in-flight turn. A single typo'd config value is a daemon crash triggered by the model's first `read_file`.
- `internal/tools/exec.go:200` — `context.WithTimeout(ctx, e.cfg.Timeout.Std())` with `0s` deadlines immediately; every `exec` call fails forever.
- `internal/tools/exec.go:222-224` — `output = output[:e.cfg.MaxOutputBytes]` with `0` returns empty output on every command.
- `internal/tools/web_fetch.go:37,70` — `http.Client{Timeout: 0}` means *no* timeout, the exact opposite of `docs/configuration.md:83`'s "Per-request timeout for a fetch".
- `prompt_cmd.go:47` / `tools.TerminalApprover` — `approval_timeout: 0s` expires every approval before a human can answer.

Fix (smallest): extend `validateTools`/`validateAgent` with the same shape
`validateOpenAI` already uses:

```go
if fs.Enabled && fs.MaxReadBytes < 1  { errs.add("tools.filesystem.max_read_bytes",  "must be greater than 0, got %d", fs.MaxReadBytes) }
if fs.Enabled && fs.MaxWriteBytes < 1 { errs.add("tools.filesystem.max_write_bytes", "must be greater than 0, got %d", fs.MaxWriteBytes) }
if wf.Enabled && wf.Timeout.Std() <= 0 { errs.add("tools.web_fetch.timeout", "must be greater than 0, got %s", wf.Timeout.Std()) }
if wf.Enabled && wf.MaxBytes < 1 { ... }
if exec.Enabled && exec.Timeout.Std() <= 0 { ... }
if exec.Enabled && exec.ApprovalTimeout.Std() <= 0 { ... }
if exec.Enabled && exec.MaxOutputBytes < 1 { ... }
if cfg.OpenAI.MaxRetries < 0 { ... }
```

Independently worth raising with whoever owns `internal/tools`: `fs.go:113` should
clamp (`if limit < 1 { limit = defaultReadBytes }`) rather than trust config, and
the registry should not be able to panic a long-running process at all.

### H2. No signal handling outside `gateway`: Ctrl-C during `prompt` / `cron run` bypasses the loop's own flush-on-cancel path

`internal/cli/root.go:40` — `err := newRootCmd(s).Execute()`. Cobra's `Execute()`
installs `context.Background()`, so every `cmd.Context()` in the tree is
uncancellable. Only `gateway` recovers, because `gateway.Run` wraps it itself
(`internal/gateway/gateway.go:139` → `shutdown.go:25-27 notifyContext`).

`mtclaw prompt "…"` and `mtclaw cron run <name>` can each run a multi-minute
agent turn (`prompt_cmd.go:55`, `cron_cmd.go:152`). SIGINT kills the process
outright. That matters because the agent loop has an explicit cancellation path
it never gets to run: `internal/agent/loop.go:313` logs
`"agent: flush on cancellation failed"` — i.e. on ctx cancel the loop *flushes
the partial turn* to the store. A SIGINT that never reaches ctx skips it, so the
turn (including tool calls already executed — `exec` side effects are real; see
`docs/security.md:165-184`) leaves no session history at all. The user's only
recourse is `mtclaw approvals list`.

Fix: one line in `root.go`.

```go
func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := &state{}
	err := newRootCmd(s).ExecuteContext(ctx)
	...
}
```

`gateway.Run`'s own `notifyContext` then becomes redundant but harmless (nested
`signal.NotifyContext` is fine); it can be removed in the same change or left.
Second Ctrl-C still hard-kills via the default disposition after `stop()`.

## Medium

### M1. `config.Validate` writes to the filesystem — read-only commands create directories

`internal/config/validate.go:287-292` → `load.go:193-195`:
`ensureDirCreatable(dir)` is `os.MkdirAll(dir, 0o755)`. Verified: running
`mtclaw --config t1/config.yaml config validate` created `t1/state/` (mode 755)
on disk. Same for `config show`, `sessions list`, `approvals list`, `cron list`.

This contradicts the package's own contract — `validate.go:44-46` "it does not
read the filesystem except for storage.path's parent-directory check" (it
*writes*), and `docs/architecture.md:49-52` "A pure function of (file bytes, env
map, OS)". Practical impact: pointing `--config` at someone else's config file to
inspect it silently creates directories wherever that file's `storage.path`
points.

Fix (smallest): keep validation stat-only (walk up to the nearest existing
ancestor and check it is a writable directory), and move the actual `MkdirAll`
to `sqlite.Open`'s caller path — `state.openStore` (`root.go:60`) and
`gateway.New` — which are the only places that genuinely need the directory.

### M2. `isWithinRoot` disagrees with the runtime guard it is meant to mirror (prior review L2, still open)

`internal/config/validate.go:187-194` uses `filepath.Rel` on raw strings.
`internal/tools/path_guard.go:34-67` resolves symlinks (`resolveDeepestSymlinks`,
`resolveRootBestEffort`) and compares case-insensitively on Windows/macOS.

Failure scenario the prior report did not list (a false *rejection*, not just a
false accept): `roots: [/home/me/ws]` where `ws` is a symlink to `/data/ws`, and
`exec.cwd: /data/ws`. `tools.Resolve` accepts it at runtime; `config.Validate`
refuses to load the file at all — `tools.exec.cwd: must be inside one of
tools.filesystem.roots`. The user cannot start the gateway with a setup the
filesystem tools would have honored.

Two fixes, pick one:

- **Preferred / smallest:** delete the confinement rule from `Validate` and rely
  on `doctor`'s `checkExecCWD` (`doctor_checks.go:296-309`), which already stats
  both paths and can resolve symlinks properly. `exec.cwd` is documented as "not
  a jail" (`docs/configuration.md:100`, `security.md:31-33`), so this is a
  usability check, not a security boundary — it does not belong in a pure
  validator that cannot see the filesystem.
- Or: extract the `withinRoot` comparison into a tiny shared package (`config`
  cannot import `tools` — `tools` imports `config`) and call it from both.

### M3. `TerminalApprover` rebuilds its `bufio.Reader` per `Ask` — the second approval in one `prompt` run reads EOF

`internal/tools/approver.go:107` — `bufio.NewReader(t.In).ReadString('\n')` inside
`Ask`. The first `Ask` buffers whatever `t.In` had available; a second `Ask` gets
a fresh reader over the already-drained stream. With `printf 'y\ny\n' | mtclaw
prompt "…"` the second approval sees EOF → `read approval response: EOF` →
audited `expired`. Fail-closed, so not a security issue, but scripted/CI use of
`prompt` is silently broken after the first approval.

Fix: build the reader once in `NewTerminalApprover` and store it on the struct
(`tools`-owned; surfaced by `prompt_cmd.go:47`).

### M4. Three copies of the same wiring; two copies of the same Telegram send

- `prompt_cmd.go:42-53`, `cron_cmd.go:122-134`, and `gateway.go:70-104` each do
  `openai.New(cfg.OpenAI)` → `tools.New(cfg, st, <approver>, log)` →
  `agent.New(cfg, client, st, registry, log)`. The *only* difference is the
  approver (`TerminalApprover` / `DenyAllApprover` / `approverMux`).
- `send_cmd.go:27-34` and `cron_cmd.go:228-240` duplicate token resolution,
  `telegram.SendOnce`, and the identical error string `"no telegram bot token
  resolved; set channels.telegram.token_env or channels.telegram.token_file"`.

Drift risk is concrete: a future change to provider construction (e.g. a proxy
option, a user-agent) must be made in three places or the CLI paths quietly
diverge from the gateway.

Fix (smallest, no new package): two unexported helpers in `internal/cli`:

```go
func (s *state) newLoop(st store.Store, approver tools.Approver) (*agent.Loop, error)
func (s *state) sendTelegram(ctx context.Context, chatID, threadID, text string) error
```

`prompt` and `cron run` call both; `gateway.New` keeps its own wiring (it also
owns the lock, dispatcher, and mux, and `gateway` cannot import `cli`).

### M5. `prompt` has no concurrency guard on its persistent session, while `cron run` does

`cron_cmd.go:110-115` refuses to run a persistent job while the gateway holds the
lock, with a good rationale at `cron_cmd.go:188-191` ("nothing serializes two
processes appending to the same session"). `prompt_cmd.go:37,97` uses a
*persistent* `cli/local` session by default and has no equivalent guard — two
concurrent `mtclaw prompt` invocations read the same history and both flush at
turn end, interleaving the transcript.

Fix: either document it (`prompt` is single-user, interleaving is accepted) or
reuse `refuseIfGatewayLocked`'s shape with a separate `prompt.lock`. Cheapest
honest option: a sentence in `--help` and `docs/configuration.md`.

### M6. Secret hygiene is inconsistent: config 0600, but transcripts and logs 0644 in a 0755 state dir

Verified on disk:

- `~/.mtclaw` and any `storage.path` parent: `0o755` (`config/load.go:194`,
  `cli/doctor_checks.go:87`, `cli/onboard_cmd.go:416`, `gateway/gateway.go` MkdirAll).
- database: `644 t1/state/db.sqlite` (created via `sessions rm`) — contains every
  message of every session.
- log file: `644 t3/l/mtclaw.log` (`internal/logging/logger.go:26`, `0o644`).
- config file: `0600` (`onboard_cmd.go:423`), and `doctor` *warns loudly* if it is
  world-readable (`doctor_checks.go:69-71`).

So the project warns about the file containing env-var *names* while writing the
file containing full conversation transcripts world-readable. On a single-user
laptop this is near-zero risk; on the shared/remote hosts this project is clearly
meant for, it is not.

Fix: `0o700` for the state dir, `0o600` for the log file; raise the DB file mode
with whoever owns `internal/store/sqlite`.

### M7. `onboard` writes `AGENTS.md` to `~/.mtclaw/prompts` regardless of `--config`

`onboard_cmd.go:388-402` uses `config.StateDir()`, ignoring `configPath`, while
`onboard_cmd.go:175` tells the user "(or pass `--config` pointing elsewhere) to
onboard a fresh one". Two "separate" installs therefore share and silently
overwrite one starter prompt file, and the second install's config points at the
first's file.

Fix: write it next to the config (`filepath.Join(filepath.Dir(configPath), "prompts", "AGENTS.md")`).

### M8. Test gaps in exactly the code this review flags

- `config.ConfigPath` (flag > `MTCLAW_CONFIG` > default precedence), `StateDir`,
  and `ExpandPath`/`expandTilde` have **no direct tests** (`grep ConfigPath
  internal/config/*_test.go` → nothing); only an indirect tilde case via
  `TestLoad_TildeExpansion`.
- `internal/logging` and `internal/version` have **no test files at all** — the
  invalid-level fallback, `json` format selection, and the unwritable-log-file
  error path are untested.
- No test constructs the cobra tree: `root.go` (`prepare`, `skipsConfigLoad`,
  `openStore` read-only/read-write selection, `Execute`'s store close), and every
  command except `doctor`/`onboard` is uncovered. The `prompt` approver wiring
  and `cron run`'s `DenyAllApprover` choice — both load-bearing security
  decisions — are asserted only by comments.

The tests that *do* exist are good: `onboard_test.go:116-145` reloads the written
config through `config.LoadFile` and asserts the secret is absent; `doctor_test.go:299`
drives a full run and round-trips the JSON. No phantom tests found.

## Low

- **L1. Factually wrong comment.** `doctor_checks.go:245-252` justifies
  `checkAllowlistNonEmpty` with "`onboard` calls these checks directly, against a
  config it just wrote, before `config.Load` ever gets a chance to reject
  anything". Not true: `runOnboard` calls `runDoctor` (`onboard_cmd.go:132`),
  which calls `config.LoadFile` → `Validate` first (`doctor_cmd.go:60`), and
  `doctorChecks(` has exactly one caller. The check is harmless; the comment
  should say it is a defensive duplicate, not claim an unvalidated caller exists.
- **L2.** `config show` output is not a loadable config: it emits
  `api_key: <set:env:MY_KEY>` / `token: <set:env:MY_TG>` (verified), which
  `validate.go:88,103` rejects on the next load. Worth one line in the command's
  `Short`/docs so nobody pipes it back over their config.
- **L3.** `warnIfWorldReadable` (`load.go:134-145`) writes to `os.Stderr` directly
  from a library function, bypassing the configured logger and the command's own
  `ErrOrStderr()`. Better: return warnings from `Load` and let `cli` print them.
- **L4.** `--log-level` silently accepts garbage (`logging/logger.go:43-53`
  falls back to info — documented) and is a complete no-op for `version`,
  `config path`, `onboard`, `doctor`, which return from `prepare` before
  `logging.New` (`root.go:116-118`).
- **L5.** `tools.exec.cwd` confinement is validated whenever `exec.Enabled`
  (`validate.go:169`), even with `mode: "off"` — but the registry never registers
  the tool in that case (`tools/registry.go:109`). A config can fail to load over
  a value nothing will use.
- **L6.** `defaultShellArgv` (`doctor_checks.go:329-337`) duplicates `tools`'
  unexported default. Documented as deliberate; still a drift vector — a shared
  exported `tools.DefaultShell()` would cost nothing.
- **L7.** `cron list` issues one `CronRuns().List` per job (`cron_cmd.go:70`)
  inside the render loop. Bounded by job count, so not a real N+1 today; note only.
- **L8.** `manualAllowFromEntry` (`onboard_cmd.go:376-381`) accepts any int64,
  including a negative group id, into `allow_from` (a user-id list). One
  `if id <= 0 { warn }` would catch the paste mistake `docs/telegram-setup.md:65-73`
  spends a section on.
- **L9.** `showWouldNotOverwrite` (`onboard_cmd.go:150-177`) dumps the entire
  default config (~60 lines of YAML) on every refused re-onboard. Low value,
  high noise; a two-line refusal plus `mtclaw config show` would do.

## Build / release hygiene

- `go.mod` `go 1.25.7`, no `toolchain` directive. Fine for CI (`go-version-file:
  go.mod` in both workflows), but a contributor on Go 1.25.0–1.25.6 with
  `GOTOOLCHAIN=local` gets a hard failure, while README says "Requires Go 1.25+".
  Either relax the directive to `go 1.25` or fix the README (see drift table).
- `CGO_ENABLED=0` is consistent: Makefile `build`/`test`/`install`/`release`,
  `ci.yml:33-41`, `release.yml:14-17`. `-race` correctly documented as the CGO
  exception in both places.
- `release.yml:49` targets == `Makefile:40` `RELEASE_TARGETS` (5 targets, exact
  match). ldflags `-X` paths match `internal/version`'s three vars exactly in both.
  `-trimpath` and `-s -w` in both. SHA256SUMS produced in both. Good.
- Actions use moving major tags (`checkout@v4`, `setup-go@v5`, `upload-artifact@v4`,
  `action-gh-release@v2`, `setup-mingw@v2`) — matches the workspace version-pinning
  rule; no change wanted.
- **CI never runs the CGO-free test path.** `ci.yml` runs `go vet` and `go build`
  with `CGO_ENABLED=0` but only `go test -race` with `CGO_ENABLED=1`. The
  Makefile's own `test` target (`CGO_ENABLED=0 go test ./...`) is therefore never
  exercised in CI. One extra step, or accept it deliberately.
- CI does not run `go mod tidy -diff` (currently clean) or a release-matrix build;
  a broken cross-compile is only discovered when a tag is pushed.
- `make release` runs `clean` first, so `sha256sum *` cannot ingest a stale
  `SHA256SUMS`; `release.yml` runs in a fresh checkout. No bug.

## Docs-drift table

| Location | Claim | Actual | Severity |
|---|---|---|---|
| `docs/configuration.md:40` | `agent.workspace` — "created if missing" | Nothing creates it except `onboard` (`onboard_cmd.go:107`). `grep Agent.Workspace` shows only reads elsewhere; `doctor` FAILs on a missing one (`doctor_checks.go:269-271`). Gateway/prompt start fine and fail at first tool call. | High |
| `docs/configuration.md:83` | `tools.web_fetch.timeout` — "Per-request timeout for a fetch" | `0s` is accepted by validation and yields `http.Client{Timeout: 0}` = **no** timeout (`web_fetch.go:37,70`). See H1. | High |
| `docs/architecture.md:49-52` | config is "A pure function of (file bytes, env map, OS)" | `Validate` → `os.MkdirAll` (`validate.go:289`, `load.go:194`); `resolveSecret` reads files and prints to `os.Stderr` (`load.go:121-127,143`). See M1. | Medium |
| `docs/architecture.md:51-52` | "no other package reads `os.Getenv` for config purposes" | `internal/cli/onboard_cmd.go:203` and `:304` read `os.Getenv(envName)` to verify the key/token. Benign (never written to cfg), but the absolute claim is false. | Low |
| `docs/configuration.md:100` | `exec.cwd` "Must resolve inside one of `tools.filesystem.roots` or load fails" | True, but the check is symlink-blind and case-sensitive, unlike the runtime guard — a symlinked root makes a valid setup unloadable (M2). Doc should say "compared as literal paths". | Medium |
| `docs/configuration.md:100` | a cwd that passes validation but is missing "fails `mtclaw doctor`'s **'exec.cwd inside a root'** check" | Check name is right, but the message it prints is about existence, not confinement (`doctor_checks.go:303`). Cosmetic. | Low |
| `README.md:24` | "Requires Go 1.25+ to build from source" | `go.mod` says `go 1.25.7`; 1.25.0–1.25.6 fails with `GOTOOLCHAIN=local`. | Low |
| `README.md:80` + table | "`mtclaw version` — Prints the build-stamped version" next to `go install …@latest` (`README.md:42`) | `go install` applies no ldflags: verified output `mtclaw dev (commit none, built unknown)`. Only `make build`/`install` and the release workflow stamp. | Low |
| `README.md:27-29` | "download the **archive** for your OS/arch … verify it against `SHA256SUMS`" | `release.yml:55-64` uploads **bare binaries** (`mtclaw-<ver>-<os>-<arch>[.exe]`) plus `SHA256SUMS`. No archives. | Low |
| `docs/configuration.md:6-8` | the coverage test "asserts every field … appears verbatim" | True (`docs_coverage_test.go:65-82`), but it is presence-only — the *values* in the tables are unverified, which is how the two High drifts above survived. Worth stating. | Low |

Verified-correct doc claims (spot-checked, no drift): deny-list checked first and
unoverridable (`tools/policy.go` order, `security.md:22`); `mode: off` never
registers the tool (`registry.go:109`, `configuration.md:98`); classifier
`max_retries` forced to 0 (`classifier.go:71`, `configuration.md:107`);
`web_fetch` refuses loopback/private/link-local/CGNAT/metadata at connect time
(`web_fetch.go:97,113-114`, `configuration.md:86-88`); filesystem roots are
symlink-resolved (`path_guard.go:16-33`, `configuration.md:74`); cron turns get
`DenyAllApprover` (`cron_cmd.go:130`, `gateway/approver.go:48`, `security.md:130-142`);
positive group ids rejected (`validate.go:134-137`, `telegram-setup.md:70-73`);
`/whoami` exists (`telegram/commands.go:112`); system-prompt files are
warn-and-skip, not fatal (`agent/prompt.go:80-88`, `configuration.md:41`);
README command table matches `root.go:92-101` exactly (10 commands, all present).

## Keep / Refactor / Rewrite

| Package / file | Verdict | Reason |
|---|---|---|
| `main.go` | **Keep** | 13 lines, correct: error → exit 1. Nothing to do. |
| `internal/config` (types, defaults, load, paths) | **Keep** | Pipeline shape (decode → resolve secrets → expand paths → validate) is right and testable; `Load(data, baseDir, env)` as a pure entry point with `LoadFile` as the impure shim is the correct split. Redaction verified leak-free. |
| `internal/config/validate.go` | **Refactor (small)** | H1 (add the missing bounds), M1 (stop writing to disk), M2 (drop or fix the cwd confinement rule). ~40 lines of change, no restructuring. |
| `internal/cli/root.go` | **Refactor (small)** | H2 (`ExecuteContext` + `signal.NotifyContext`) and M4 (host the two shared wiring helpers). The `state` struct + `skipsConfigLoad` design is sound; keep it. |
| `internal/cli/{prompt,cron,send}_cmd.go` | **Refactor (small)** | M4 only — delete the duplicated wiring, keep the commands. Logic is otherwise correct; `cron run`'s lock guard and `--deliver` default-off are good calls. |
| `internal/cli/gateway_cmd.go` | **Keep** | 59 lines, thin, correct; the `auto`-mode warn banner earns its place. |
| `internal/cli/{config,sessions,approvals,version}_cmd.go` | **Keep** | Thin, read-only, correctly choose read-only store handles. |
| `internal/cli/onboard_cmd.go` + `onboard_prompts.go` (576 lines) | **Keep** | Not over-built. It is the product's entire first-run UX, the `prompter`/`telegramCapturer` seams are what make it testable without a TTY or network, and the tests prove the written config reloads with no secret. Trim only L9 (the default-config dump) and fix M7. |
| `internal/cli/doctor_cmd.go` + `doctor_checks.go` (536 lines) | **Keep** | 17 checks × ~15 lines each, each with a FAIL message containing the fix, each tested. For a tool whose failure modes are "your env var isn't exported" and "your model name is wrong", this is the highest-value code in the repo. Fix L1's wrong comment; optionally drop `checkAllowlistNonEmpty` once that comment's false premise is gone. |
| `internal/logging` | **Keep** | 54 lines, right shape. Add tests (M8) and `0o600` (M6). |
| `internal/version` | **Keep** | Correct; ldflags paths match both build paths. |
| `docs/` | **Keep + fix** | Genuinely good docs — better than the code average. Fix the drift table's two High rows; consider extending `docs_coverage_test.go` to assert documented *defaults* match `config.Default()` via reflection, which would have caught nothing today but pins the tables going forward. |
| `Makefile`, `release.yml` | **Keep** | Matrix, ldflags, trimpath, checksums all consistent. |
| `ci.yml` | **Keep + 1 step** | Add a `CGO_ENABLED=0 go test ./...` leg (and optionally `go mod tidy -diff`). |

## Recommended actions, in order

1. H1 — add the missing bounds in `validate.go`; separately clamp `limit` in `tools/fs.go:113` so no config value can panic the daemon.
2. H2 — `ExecuteContext` + `signal.NotifyContext` in `root.go:38-45`.
3. M1 — take `os.MkdirAll` out of `Validate`; move it to `openStore`/`gateway.New`.
4. M2 — drop the `exec.cwd` confinement rule from `Validate` (leave it to `doctor`), or make it symlink/case-aware.
5. M4 — extract `state.newLoop` and `state.sendTelegram`; delete the three/two duplicates.
6. M6 — `0o700` state dir, `0o600` log file; raise the DB mode with the store owner.
7. Docs: fix "workspace created if missing" and the `web_fetch.timeout` row; fix README's Go version, `go install` stamping, and "archive" wording.
8. M8 — tests for `ConfigPath` precedence, `ExpandPath`, `logging.New`, and one end-to-end cobra test per command that asserts the approver type `prompt` and `cron run` wire.
9. M3, M5, M7, then the L-items as they are touched.

## Unresolved questions

1. Is a `mtclaw prompt` running concurrently with another `prompt` (M5) an accepted single-user trade-off, or should it take a lock like `cron run` does? The asymmetry is currently undocumented.
2. Should `mtclaw --config /elsewhere/config.yaml onboard` produce a self-contained install (its own `prompts/AGENTS.md`, its own state dir), or is `~/.mtclaw` intentionally global (M7)? The refuse-to-overwrite message implies the former; the code does the latter.
3. Database file mode (`0644`, M6) is owned by `internal/store/sqlite` — confirm with that slice's reviewer whether it is set explicitly or inherited from the driver's default.
4. `openai.max_retries` has no upper bound and no lower bound (`< 0` is accepted and passed to the SDK). Is a negative value defined behavior in `openai-go v3`, or should validation reject it?
5. Is the `exec.cwd`-inside-roots rule meant as a safety property at all, or purely as a "you probably meant this" convenience? The answer decides between the two M2 fixes.
