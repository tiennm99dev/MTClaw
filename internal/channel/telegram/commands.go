package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/tiennm99/MTClaw/internal/config"
)

// commandNames is the fixed set of bot commands this channel understands
// itself (as opposed to ordinary text, which is forwarded to the agent
// loop). Keep this in sync with registerCommands' setMyCommands call.
var commandNames = map[string]string{
	"start":  "Show what this bot can do",
	"help":   "Show what this bot can do",
	"new":    "Start a fresh conversation in this chat",
	"status": "Show this session's message count, tokens, and model",
	"whoami": "Show your user id and this chat's id",
	"stop":   "Cancel the in-flight turn, if any",
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
// channel up (the gateway, in phase 7). Depending on this instead of
// internal/store or internal/agent directly keeps the channel boundary
// honest: this package never reaches into the loop's or the store's
// internals, only this three-method seam.
type Deps interface {
	// Status reports live numbers for /status.
	Status(ctx context.Context, chatID, threadID string) (SessionStatus, error)
	// Reset clears the session's history for /new.
	Reset(ctx context.Context, chatID, threadID string) error
	// Cancel cancels the in-flight turn for chatID/threadID, if any, and
	// reports whether anything was actually running.
	Cancel(chatID, threadID string) bool
}

// commandName extracts the bare, lowercased command word from text if text
// is a bot command - e.g. "/status@mybot" -> "status" - stripping the
// "@botname" suffix Telegram appends in groups. Returns "" for anything
// that is not a leading slash command at all.
func commandName(text string) string {
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	word := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(word, '@'); at >= 0 {
		word = word[:at]
	}
	return strings.ToLower(word)
}

// registerCommands publishes commandNames via setMyCommands so Telegram's
// client shows the "/" command menu.
func registerCommands(ctx context.Context, api botAPI) error {
	cmds := make([]telego.BotCommand, 0, len(commandNames))
	for _, name := range [...]string{"start", "help", "new", "status", "whoami", "stop"} {
		cmds = append(cmds, telego.BotCommand{Command: name, Description: commandNames[name]})
	}
	return api.SetMyCommands(ctx, &telego.SetMyCommandsParams{Commands: cmds})
}

// handleCommand runs one recognized command and returns the reply text.
// deps may be nil before the gateway wires it up (phase 7); commands that
// need it degrade to a clear "not available yet" message instead of
// panicking.
func handleCommand(ctx context.Context, cfg *config.Config, deps Deps, cmd, chatID, threadID string, fromID int64) string {
	switch cmd {
	case "start", "help":
		return helpText(cfg)

	case "new":
		if deps == nil {
			return "not available yet: this gateway has no session backend wired up."
		}
		if err := deps.Reset(ctx, chatID, threadID); err != nil {
			return fmt.Sprintf("could not start a new conversation: %v", err)
		}
		return "started a new conversation. Previous history in this chat is cleared."

	case "status":
		if deps == nil {
			return "not available yet: this gateway has no session backend wired up."
		}
		st, err := deps.Status(ctx, chatID, threadID)
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
		if deps != nil && deps.Cancel(chatID, threadID) {
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
	for _, name := range [...]string{"new", "status", "whoami", "stop", "help"} {
		fmt.Fprintf(&b, "/%s - %s\n", name, commandNames[name])
	}
	return b.String()
}
