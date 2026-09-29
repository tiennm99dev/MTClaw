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

Requires Go 1.25.7 or newer to build from source (the `go` directive in
`go.mod` is a minimum, not a pin: with the default `GOTOOLCHAIN=auto`, an
older local Go fetches 1.25.7 automatically; `GOTOOLCHAIN=local` needs a
toolchain that is already at least 1.25.7). No Go toolchain is required if
you download a release binary.

**From a release** (no Go toolchain needed): download the `mtclaw` binary for
your OS/arch from the [releases page](https://github.com/tiennm99dev/MTClaw/releases)
(bare binaries, not archives), verify it against the accompanying
`SHA256SUMS`, and put it on your `PATH`. Release files are named
`mtclaw-<tag>-<os>-<arch>` (`.exe` on Windows); rename yours to `mtclaw` and
make it executable for the commands below. If the releases page lists
nothing yet, build from source.

**From source:**

```sh
git clone https://github.com/tiennm99dev/MTClaw.git
cd MTClaw
make build      # -> bin/mtclaw, version-stamped from git
```

Or, without cloning:

```sh
go install github.com/tiennm99/MTClaw@latest
```

The module's import path is `github.com/tiennm99/MTClaw` - a different
owner than the repo actually lives under
(`github.com/tiennm99dev/MTClaw`, see the clone URL above). `go install`
resolves the import path through GitHub's own repo-rename redirect, which
works today but is a redirect this project does not control; if it is ever
missed, clone or download a release instead. `go install` also reports a
real `Version` (whatever `@vX` you asked for) but no commit or build date,
since there is no local `.git` checkout for `runtime/debug.ReadBuildInfo`
to read VCS metadata from - only `make build`/`install` and the release
workflow, both run from inside a git checkout, get all three.

Every build - local or released - passes `CGO_ENABLED=0`, so it is a single
static binary with no runtime dependency; the plain `go install` line above
is the one exception, since it does not set that variable itself and
inherits whatever `CGO_ENABLED` your own environment defaults to.

## Upgrading

The gateway's instance lock lives next to the database
(`storage.dsn + ".lock"`, see `docs/configuration.md`'s `storage.dsn`
row) - not at a fixed path under `~/.mtclaw/`. If you are upgrading from a
version that predates this, stop the old gateway process before starting
the new binary: a new `mtclaw gateway`, `cron run`, or `doctor` checks only
the new lock path, so it cannot see an old gateway still holding the old
`~/.mtclaw/gateway.lock` and would happily run against the same database at
the same time. The stale `~/.mtclaw/gateway.lock` left behind by the old
process is no longer read by anything and can be deleted.

**Upgrading, then wanting to go back?** Once you have run `mtclaw onboard`
(or edited your config by hand) with a version that writes `storage.driver`/
`storage.dsn`, an older binary will refuse to start against that config file
at all - it rejects unrecognized keys, not just ignore them. Your database
is unaffected either way; see `docs/configuration.md`'s storage section for
the two-line edit that restores an older binary's compatibility.

**Existing configs keep their old deny list.** `onboard` writes the starter
`tools.exec.deny` list once, when it creates the config; upgrading the binary
never rewrites it, and `doctor` does not flag an outdated list. The current
defaults (`internal/tools/deny_defaults.go`) additionally catch:

- `rm` with an uppercase `-R`, and `rm` glued to a separator with no space
  (`ls;rm -rf ~`, `a&&rm -rf ~`, `ls|rm -rf ~`, a backtick before `rm`);
- force pushes as `git push -fu ...`, `git push origin +main`, and
  `git -C . push -f`, alongside `--force` and `--force-with-lease`;
- `chmod 777 -R /` (flags after `777`) and pipe-to-shell into a
  path-qualified shell (`curl x | /bin/bash`);
- on Windows, `Remove-Item` and its aliases (`ri`, `rm`, `rmdir`, `rd`,
  `del`, `erase`) with any `-r*` or `-fo*` parameter, abbreviated or not,
  and `irm` / `Invoke-RestMethod` piped to `iex`.

To adopt them, run `mtclaw onboard --config <fresh path>` and copy the
`tools.exec.deny` block into your config, or copy the patterns from
`deny_defaults.go` directly.

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
interactively - with an explicit on-screen confirmation before writing it.
If no id is confirmed (nobody messaged the bot, several people did and you
declined to pick one, or you left the manual prompt blank), `onboard` writes
`channels.telegram.enabled: false` and leaves `allow_from` empty instead of
an unloadable config; `mtclaw gateway` will not start until you add your id
(`/whoami` shows it) and set `enabled: true`. `onboard` refuses to overwrite
an existing config and never overwrites `prompts/AGENTS.md`.

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

Every command accepts two global flags: `--config <path>` (see
`docs/configuration.md` for how the path is resolved) and `--log-level
<level>`, which overrides `log.level` from the config file and is validated
the same way. Run `mtclaw <command> --help` for a command's own flags; two
are worth knowing up front:

- `mtclaw prompt --session <id>` reuses a specific session instead of the
  default `cli`/`local` one, and `--new` starts a fresh one (the two are
  mutually exclusive).
- `mtclaw cron run <name>` prints the result by default. `--deliver` actually
  sends it to the job's `deliver_to` chat, so it is a real message to a real
  chat; `--ephemeral` runs against a throwaway session instead of the job's
  persistent one. A persistent job refuses to run while the gateway holds the
  instance lock.

Exit codes: `0` on success; `1` on any error; `128 + signal` when a run is
interrupted (`130` for SIGINT, `143` for SIGTERM). `doctor` exits non-zero if
any row is `FAIL` (a `WARN` does not), and `onboard` exits `1` if a config
already exists.

## Documentation

- [`docs/configuration.md`](docs/configuration.md) - every config key: type, default, effect, what breaks if it's wrong.
- [`docs/security.md`](docs/security.md) - the threat model, the exec decision pipeline, deny-list limits, why `auto` mode is beta, why cron is allow-list-only.
- [`docs/telegram-setup.md`](docs/telegram-setup.md) - BotFather, privacy mode, finding user/group IDs, `require_mention`.
- [`docs/architecture.md`](docs/architecture.md) - component diagram, package boundaries, request lifecycle, what was deliberately not built.
- [`docs/verification.md`](docs/verification.md) - what CI proves versus what still needs live credentials, and how to run the local checks (`make lint`, `make test`).
- `docs/journals/` - historical implementation notes, not current documentation.

## Windows support: honesty note

MTClaw builds and its test suite passes on Windows, and the `exec` tool
defaults to PowerShell there with its own OS-appropriate deny-list. A
runaway command's whole process tree is killed on timeout or turn
cancellation there too, by assigning the command to a Job Object that is
closed on kill (Windows has no POSIX process group to signal, so this is a
different mechanism than the `setpgid`+`SIGKILL` the POSIX build uses, not a
gap; a grandchild spawned in the short window before the shell is assigned
to the job is outside it). That said, Windows is still the
least battle-tested target: file-permission checks that matter on POSIX
(world-readable secrets, `chmod 600` on the config file) are documented
no-ops on Windows because ACL-based permissions are not the mode bits those
checks read, and the deny-list corpus is necessarily shell-specific. Treat
Windows support as best-effort, not equivalent to Linux/macOS, until it has
seen more real use.
