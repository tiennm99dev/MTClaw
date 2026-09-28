package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/tiennm99/MTClaw/internal/config"
)

// botCommand is one bot command this channel understands itself (as
// opposed to ordinary text, which is forwarded to the agent loop).
type botCommand struct {
	name, description string
}

// commands is the fixed, ordered set of bot commands: registerCommands
// publishes all of them via setMyCommands, and helpText lists every one
// except "start" (its own text is the help reply, not a menu entry worth
// repeating).
var commands = [...]botCommand{
	{"start", "Show what this bot can do"},
	{"help", "Show what this bot can do"},
	{"new", "Start a fresh conversation in this chat"},
	{"status", "Show this session's message count, tokens, and model"},
	{"whoami", "Show your user id and this chat's id"},
	{"stop", "Cancel the in-flight turn, if any"},
}

// isCommand reports whether name is one of commands, and known's caller
// treats an unmatched name as ordinary text instead.
func isCommand(name string) bool {
	for _, c := range commands {
		if c.name == name {
			return true
		}
	}
	return false
}

// SessionStatus is the display data /status reports.
type SessionStatus struct {
	SessionID        string
	Messages         int
	PromptTokens     int
	CompletionTokens int
	Model            string
	CreatedAt        time.Time
}

// Deps is the narrow surface bot commands need from whatever wires this
// channel up (the gateway). Depending on this instead of internal/store or
// internal/agent directly keeps the channel boundary honest: this package
// never reaches into the loop's or the store's internals, only this
// three-method seam.
type Deps interface {
	// Status reports live numbers for /status.
	Status(ctx context.Context, chatID, threadID string) (SessionStatus, error)
	// Reset clears the session's history for /new.
	Reset(ctx context.Context, chatID, threadID string) error
	// Cancel cancels the in-flight turn for chatID/threadID, if any, and
	// reports whether anything was actually running.
	Cancel(chatID, threadID string) bool
}

// parseCommand extracts the bare, lowercased command word from text if
// text is a bot command - e.g. "/status@mybot" -> ("status", "mybot") -
// splitting off the "@botname" suffix Telegram appends in groups instead of
// silently discarding it: a command explicitly addressed to a different
// bot must never run as this bot's own (see handleMessage). Returns ("",
// "") for anything that is not a leading slash command at all.
func parseCommand(text string) (name, target string) {
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", ""
	}
	word := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(word, '@'); at >= 0 {
		return strings.ToLower(word[:at]), word[at+1:]
	}
	return strings.ToLower(word), ""
}

// registerCommands publishes commands via setMyCommands so Telegram's
// client shows the "/" command menu.
func registerCommands(ctx context.Context, api botAPI) error {
	cmds := make([]telego.BotCommand, 0, len(commands))
	for _, c := range commands {
		cmds = append(cmds, telego.BotCommand{Command: c.name, Description: c.description})
	}
	return api.SetMyCommands(ctx, &telego.SetMyCommandsParams{Commands: cmds})
}

// commandDBTimeout bounds every command's own call into deps: /new and
// /status run on the Telegram update pump, not a session worker, so an
// unbounded call here would let a held store write lock (a manual `mtclaw
// cron run`, a backup holding the database's write lock) stall every
// update this process ever processes, including approval callbacks.
const commandDBTimeout = 10 * time.Second

// handleCommand runs one recognized command and returns the reply text.
// deps is the only production caller's non-nil Deps; a test exercising
// "new"/"status"/"stop" supplies its own fake rather than nil.
func handleCommand(ctx context.Context, cfg *config.Config, deps Deps, cmd, chatID, threadID string, fromID int64) string {
	switch cmd {
	case "start", "help":
		return helpText(cfg)

	case "new":
		cctx, cancel := context.WithTimeout(ctx, commandDBTimeout)
		defer cancel()
		if err := deps.Reset(cctx, chatID, threadID); err != nil {
			return fmt.Sprintf("could not start a new conversation: %v", err)
		}
		return "started a new conversation. Previous history in this chat is cleared."

	case "status":
		cctx, cancel := context.WithTimeout(ctx, commandDBTimeout)
		defer cancel()
		st, err := deps.Status(cctx, chatID, threadID)
		if err != nil {
			return fmt.Sprintf("could not read session status: %v", err)
		}
		return fmt.Sprintf(
			"session: %s\nmessages: %d\ntokens: %d prompt / %d completion\nmodel: %s\nstarted: %s",
			st.SessionID, st.Messages, st.PromptTokens, st.CompletionTokens, st.Model,
			st.CreatedAt.Local().Format(time.RFC3339),
		)

	case "whoami":
		tid := "(none - general chat)"
		if threadID != "" {
			tid = threadID
		}
		return fmt.Sprintf(
			"your user id: %d\nthis chat id: %s\nthis thread id: %s\n\nUse these to fill in channels.telegram.allow_from and channels.telegram.groups.",
			fromID, chatID, tid,
		)

	case "stop":
		if deps.Cancel(chatID, threadID) {
			return "cancelling the current turn."
		}
		return "nothing running."

	default:
		return ""
	}
}

// helpText summarizes capabilities from cfg: exec mode and workspace path
// are exactly what a user needs to know before asking the bot to do
// anything with files or a shell.
func helpText(cfg *config.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s - a personal AI agent.\n\n", cfg.Agent.Name)
	fmt.Fprintf(&b, "model: %s\n", cfg.Agent.Model)
	fmt.Fprintf(&b, "workspace: %s\n", cfg.Agent.Workspace)
	fmt.Fprintf(&b, "exec mode: %s\n\n", cfg.Tools.Exec.Mode)
	b.WriteString("Commands:\n")
	for _, c := range commands {
		if c.name == "start" {
			continue
		}
		fmt.Fprintf(&b, "/%s - %s\n", c.name, c.description)
	}
	return b.String()
}
