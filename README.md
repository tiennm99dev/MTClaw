# MTClaw

MTClaw is a single-binary personal AI agent gateway: a long-running process
that talks to you over Telegram, runs an OpenAI-backed agent loop with real
tools (filesystem read/write, shell exec, web fetch), and replies in chat.
Everything is configured by one YAML file - no dashboard, no web UI, no
HTTP/RPC control surface.

## Security warning - read before you install

**MTClaw turns a Telegram message into shell execution on your machine.**
Anyone who can message the bot, and anyone who can inject text the model
reads (a web page it fetches, a file it opens, a forwarded message), is
attempting to run commands as your user account. The bot token is
equivalent to shell access: do not hand it, or an `allow_from` entry, to
anyone you would not hand a shell to. The default deny-list is the only real
enforcement boundary and it stops accidents and naive injection, not a
determined attacker who already has message access - there is no sandbox
underneath the exec tool. Read `docs/security.md` in full before you point a
real bot token at this.

## Install

Requires Go 1.25+ to build from source; no Go toolchain is required if you
download a release binary.

**From a release** (no Go toolchain needed): download the archive for your
OS/arch from the [releases page](https://github.com/tiennm99/MTClaw/releases),
verify it against `SHA256SUMS`, and put the `mtclaw` binary on your `PATH`.

**From source:**

```sh
git clone https://github.com/tiennm99/MTClaw.git
cd MTClaw
make build      # -> bin/mtclaw
```

Or, without cloning:

```sh
go install github.com/tiennm99/MTClaw@latest
```

`CGO_ENABLED=0` is used everywhere, so every build - local or released - is
a single static binary with no runtime dependency.

## 5-minute quickstart

```sh
mtclaw onboard    # interactive: OpenAI key, model, Telegram bot, your user id, workspace
mtclaw doctor     # re-check everything onboard just set up
mtclaw gateway    # start the long-running process; message your bot on Telegram
```

`onboard` never writes a secret into the config file: it writes
`api_key_env: OPENAI_API_KEY` / `token_env: TELEGRAM_BOT_TOKEN` (pointing at
environment variables) and prints the exact `export`/`setx` line you need to
add to your shell profile. It also captures your numeric Telegram user ID
interactively - with an explicit on-screen confirmation before writing it -
so the bot's allowlist is never left empty.

No Telegram bot yet? See `docs/telegram-setup.md` for the BotFather
walkthrough. Prefer the terminal first? `mtclaw prompt "list files in my
workspace"` runs one full tool-using agent turn with no Telegram involved.

## Commands

| Command | What it does |
|---|---|
| `mtclaw onboard` | Interactive first-run setup; writes a working config and runs `doctor`. |
| `mtclaw doctor` | Diagnoses config, database, OpenAI/Telegram reachability, workspace, shell, and policy settings; `--json` for scripting. |
| `mtclaw gateway` | Runs the long-running process: polls Telegram, serializes turns per chat, runs cron. |
| `mtclaw prompt "<text>"` | Runs one turn of the agent loop from the terminal against a persistent local session. |
| `mtclaw send --chat <id> "<text>"` | Sends one message via the Telegram bot without starting the gateway. |
| `mtclaw config path\|show\|validate` | Prints the resolved config path, shows the config with secrets redacted, or validates it and reports every error at once. |
| `mtclaw sessions list\|show\|rm` | Inspects and manages stored conversations. |
| `mtclaw approvals list` | Lists recent exec policy decisions (the `exec_audit` trail). |
| `mtclaw cron list\|run` | Lists configured cron jobs and their next-due times, or fires one manually. |
| `mtclaw version` | Prints the build-stamped version. |

## Documentation

- [`docs/configuration.md`](docs/configuration.md) - every config key: type, default, effect, what breaks if it's wrong.
- [`docs/security.md`](docs/security.md) - the threat model, the exec decision pipeline, deny-list limits, why `auto` mode is beta, why cron is allow-list-only.
- [`docs/telegram-setup.md`](docs/telegram-setup.md) - BotFather, privacy mode, finding user/group IDs, `require_mention`.
- [`docs/architecture.md`](docs/architecture.md) - component diagram, package boundaries, request lifecycle, what was deliberately not built.

## Windows support: honesty note

MTClaw builds and its test suite passes on Windows, and the `exec` tool
defaults to PowerShell there with its own OS-appropriate deny-list. That
said, Windows is the least battle-tested target: there is no POSIX-style
process-group kill for a runaway command (Windows needs a job object or
`taskkill /T` for equivalent behavior), file-permission checks that matter
on POSIX (world-readable secrets, `chmod 600` on the config file) are
documented no-ops on Windows because ACL-based permissions are not the mode
bits those checks read, and the deny-list corpus is necessarily
shell-specific. Treat Windows support as best-effort, not equivalent to
Linux/macOS, until it has seen more real use.
