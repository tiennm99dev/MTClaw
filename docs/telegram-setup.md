# Telegram setup

How to create a bot, get its token, find the numeric IDs MTClaw's allowlist
needs, and understand `require_mention` and group chat IDs. Read
`docs/security.md` first: the bot token you are about to create is
equivalent to shell access on this machine once MTClaw is configured.

## 1. Create the bot with BotFather

1. Open a chat with [@BotFather](https://t.me/BotFather) in Telegram.
2. Send `/newbot` and follow the prompts: a display name, then a username
   ending in `bot` (e.g. `my_mtclaw_bot`).
3. BotFather replies with a token that looks like
   `123456789:AAExampleTokenDoNotUseThisOne`. That is the value
   `channels.telegram.token_env` (or `token_file`) points at - **never paste
   it into `config.yaml` directly**; MTClaw's config loader rejects a
   literal `token:` key outright.
4. Set it as an environment variable before running `mtclaw`:

   ```sh
   export TELEGRAM_BOT_TOKEN=123456789:AAExampleTokenDoNotUseThisOne   # POSIX
   setx TELEGRAM_BOT_TOKEN "123456789:AAExampleTokenDoNotUseThisOne"   # Windows
   ```

`mtclaw onboard` walks through this same sequence interactively and calls
`getMe` to confirm the token works before moving on.

## 2. `/setprivacy` and the re-add requirement

By default, a Telegram bot added to a **group** only receives messages that
are commands (`/something`) or that explicitly mention it - this is
Telegram's own "privacy mode," separate from MTClaw's own
`require_mention` setting described below.

If you want the bot to see more of a group's messages (still gated by
MTClaw's own `require_mention` and allowlist), talk to BotFather:

1. `/mybots` -> select your bot -> **Bot Settings** -> **Group Privacy**.
2. Turn privacy mode **off**.
3. **You must remove the bot from any group it is already in and re-add it**
   for the privacy setting change to take effect - Telegram applies privacy
   mode at the time the bot joins a group, not retroactively.

If you leave privacy mode on (the default), that is fine and often what you
want: combined with MTClaw's `require_mention: true` default, the bot only
ever sees messages that already mention it.

## 3. Finding your numeric user ID

MTClaw's allowlist (`channels.telegram.allow_from`) is a list of numeric
Telegram user IDs, not usernames - usernames can change; numeric IDs do not.

The easiest way: run `mtclaw onboard`. Its interactive capture step opens a
temporary long poll and tells you to message the bot; whoever messages it
during that window has their username and numeric ID printed on screen, and
onboard asks for explicit confirmation before writing it to `allow_from`.

To get your ID at any other time, once the bot is running:

- Message the bot `/whoami`. It replies with your numeric user ID (and the
  chat ID, if you sent it from a group).
- Alternatively, message any of the several public "get my Telegram ID" bots
  (e.g. `@userinfobot`) - useful before your own bot is even running yet.

## 4. Group and supergroup chat IDs

A regular Telegram group has a negative chat ID (e.g. `-123456789`). A
**supergroup** (what a regular group becomes once it grows past a certain
size, or once certain settings are enabled) has a chat ID with a `-100`
prefix (e.g. `-1001234567890`). Both forms are negative - if a chat ID you
are about to write into `channels.telegram.groups` is positive, it is a user
ID, not a group, and `config.Validate` rejects it with a message saying
exactly that.

To key a `channels.telegram.groups` entry:

```yaml
channels:
  telegram:
    groups:
      "-1001234567890":
        require_mention: true
        allow_from: []   # empty inherits channels.telegram.allow_from
```

Get a group's chat ID the same way as a user ID: send `/whoami` in that
group once the bot is a member (privacy mode considerations from step 2
apply to whether the bot even sees the command).

## 5. `require_mention` behavior

`require_mention` (default `true`, applied via the `"*"` key that every
group not otherwise listed inherits) controls whether the bot must be
`@mentioned` or replied-to in a group chat before it treats a message as
directed at it:

- `true` (default): only messages that `@mention` the bot, or that reply
  directly to one of the bot's own messages, are forwarded to the agent
  loop. Everything else in the group is ignored - this is what keeps a busy
  group chat from turning into a stream of unwanted API calls.
- `false`: every message from an allowed sender in that group is forwarded,
  mention or not. Only turn this off for a group you expect to use almost
  entirely for talking to the bot.

`require_mention` and the allowlist are independent checks - both must pass
for a group message to reach the agent loop: the sender must be on the
effective allowlist for that group (`channels.telegram.groups.<id>.allow_from`
if non-empty, otherwise `channels.telegram.allow_from`), **and** the message
must satisfy `require_mention` if it is set for that group.
