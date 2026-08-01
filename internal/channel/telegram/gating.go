package telegram

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/mymmrac/telego"

	"github.com/tiennm99/MTClaw/internal/config"
)

// Decide is the whole access-control matrix, evaluated as a pure function
// of (config, bot identity, message) so it is entirely table-testable with
// no bot, no network, and no update loop. Every rejection is silent by
// design: the caller must not reply, react, or otherwise confirm the bot
// exists to a sender that failed gating.
func Decide(cfg config.TelegramConfig, botUsername string, botID int64, msg *telego.Message) (accept bool, cleanText string, reason string) {
	if msg == nil {
		return false, "", "nil message"
	}
	if msg.From != nil && msg.From.IsBot {
		return false, "", "sender is a bot"
	}

	var fromID int64
	if msg.From != nil {
		fromID = msg.From.ID
	}

	switch msg.Chat.Type {
	case "private":
		if !containsID(cfg.AllowFrom, fromID) {
			return false, "", "dm sender not in allow_from"
		}
		return true, msg.Text, ""

	case "group", "supergroup":
		group, ok := lookupGroup(cfg.Groups, msg.Chat.ID)
		if !ok {
			return false, "", "no group entry for this chat"
		}

		allow := group.AllowFrom
		if len(allow) == 0 {
			// An empty group allow_from inherits the channel-level list
			// rather than granting access to everyone (see
			// TelegramGroupConfig's doc comment).
			allow = cfg.AllowFrom
		}
		if !containsID(allow, fromID) {
			return false, "", "sender not in group (or channel) allow_from"
		}

		if !group.RequireMention {
			return true, msg.Text, ""
		}

		mentioned, clean := detectMention(msg, botUsername, botID)
		if !mentioned {
			return false, "", "require_mention is true and no mention found"
		}
		return true, clean, ""

	default:
		return false, "", "unsupported chat type " + msg.Chat.Type
	}
}

// containsID reports whether id appears in ids. A nil/empty ids never
// matches, including id==0 (an absent From.ID), so a config that forgot to
// set allow_from fails closed rather than accepting an unset sender.
func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// lookupGroup resolves chatID against groups: an exact string-keyed entry
// wins, falling back to the "*" default entry when present.
func lookupGroup(groups map[string]config.TelegramGroupConfig, chatID int64) (config.TelegramGroupConfig, bool) {
	key := strconv.FormatInt(chatID, 10)
	if g, ok := groups[key]; ok {
		return g, true
	}
	if g, ok := groups["*"]; ok {
		return g, true
	}
	return config.TelegramGroupConfig{}, false
}

// detectMention reports whether msg addresses the bot: a reply to one of
// the bot's own messages, a "@botusername" mention entity, or a
// "/command@botusername" bot_command entity. It never falls back to a bare
// substring search - only entities, which Telegram computes itself, decide
// this. clean is msg.Text with a leading "@botusername" mention stripped;
// the reply-to and /cmd@bot cases return the text unchanged, since there is
// no leading token to remove.
func detectMention(msg *telego.Message, botUsername string, botID int64) (matched bool, clean string) {
	clean = msg.Text

	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil && msg.ReplyToMessage.From.ID == botID {
		return true, clean
	}

	for _, e := range msg.Entities {
		switch e.Type {
		case telego.EntityTypeMention:
			token := utf16Slice(msg.Text, e.Offset, e.Length)
			if !strings.EqualFold(strings.TrimPrefix(token, "@"), botUsername) {
				continue
			}
			if e.Offset == 0 {
				clean = strings.TrimSpace(utf16Slice(msg.Text, e.Offset+e.Length, -1))
			}
			return true, clean

		case telego.EntityTypeBotCommand:
			token := utf16Slice(msg.Text, e.Offset, e.Length)
			at := strings.IndexByte(token, '@')
			if at < 0 {
				continue
			}
			if strings.EqualFold(token[at+1:], botUsername) {
				return true, clean
			}
		}
	}

	return false, clean
}

// utf16Slice returns the substring of s spanning [offset, offset+length) in
// UTF-16 code units - the unit Telegram's MessageEntity offsets and lengths
// are expressed in - not bytes or runes. length < 0 means "to the end".
// Out-of-range bounds are clamped rather than panicking, since a malformed
// or unexpected entity must not crash gating.
func utf16Slice(s string, offset, length int) string {
	units := utf16.Encode([]rune(s))
	if offset < 0 {
		offset = 0
	}
	if offset > len(units) {
		offset = len(units)
	}
	end := len(units)
	if length >= 0 {
		end = offset + length
		if end > len(units) {
			end = len(units)
		}
	}
	if end < offset {
		end = offset
	}
	return string(utf16.Decode(units[offset:end]))
}
