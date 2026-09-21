package telegram

import (
	"testing"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"

	"github.com/tiennm99/MTClaw/internal/config"
)

const (
	testBotUsername = "mtclawbot"
	testBotID       = int64(999)
)

func mentionEntity(offset, length int) telego.MessageEntity {
	return telego.MessageEntity{Type: telego.EntityTypeMention, Offset: offset, Length: length}
}

func botCommandEntity(offset, length int) telego.MessageEntity {
	return telego.MessageEntity{Type: telego.EntityTypeBotCommand, Offset: offset, Length: length}
}

func TestDecide_Matrix(t *testing.T) {
	baseCfg := config.TelegramConfig{AllowFrom: []int64{100}}

	cfgWithGroups := config.TelegramConfig{
		AllowFrom: []int64{100},
		Groups: map[string]config.TelegramGroupConfig{
			"-1001": {RequireMention: false},
			"-1002": {RequireMention: true},
			"-1003": {RequireMention: false, AllowFrom: []int64{200}},
			"*":     {RequireMention: true},
		},
	}

	cases := []struct {
		name       string
		cfg        config.TelegramConfig
		msg        *telego.Message
		wantAccept bool
		wantText   string
	}{
		{
			name: "private allowed",
			cfg:  baseCfg,
			msg: &telego.Message{
				Chat: telego.Chat{ID: 100, Type: "private"},
				From: &telego.User{ID: 100},
				Text: "hello",
			},
			wantAccept: true,
			wantText:   "hello",
		},
		{
			name: "private denied - not in allow_from",
			cfg:  baseCfg,
			msg: &telego.Message{
				Chat: telego.Chat{ID: 999, Type: "private"},
				From: &telego.User{ID: 999},
				Text: "hello",
			},
			wantAccept: false,
		},
		{
			name: "sender is a bot - always dropped",
			cfg:  baseCfg,
			msg: &telego.Message{
				Chat: telego.Chat{ID: 100, Type: "private"},
				From: &telego.User{ID: 100, IsBot: true},
				Text: "hello",
			},
			wantAccept: false,
		},
		{
			name: "group with no entry - ignored",
			cfg: config.TelegramConfig{
				AllowFrom: []int64{100},
				Groups:    map[string]config.TelegramGroupConfig{"-1001": {}},
			},
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1999, Type: "supergroup"},
				From: &telego.User{ID: 100},
				Text: "hello",
			},
			wantAccept: false,
		},
		{
			name: "group via * default entry",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1500, Type: "supergroup"},
				From: &telego.User{ID: 100},
				Text: "@mtclawbot hello",
				Entities: []telego.MessageEntity{
					mentionEntity(0, 10),
				},
			},
			wantAccept: true,
			wantText:   "hello",
		},
		{
			name: "group with its own allow_from - member allowed",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1003, Type: "supergroup"},
				From: &telego.User{ID: 200},
				Text: "hello",
			},
			wantAccept: true,
			wantText:   "hello",
		},
		{
			name: "group with its own allow_from - non-member denied",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1003, Type: "supergroup"},
				From: &telego.User{ID: 999},
				Text: "hello",
			},
			wantAccept: false,
		},
		{
			name: "allowlisted user but group absent entirely",
			cfg: config.TelegramConfig{
				AllowFrom: []int64{100},
				Groups:    map[string]config.TelegramGroupConfig{"-1009": {}},
			},
			msg: &telego.Message{
				Chat: telego.Chat{ID: -55, Type: "group"},
				From: &telego.User{ID: 100},
				Text: "hello",
			},
			wantAccept: false,
		},
		{
			name: "require_mention true, no mention - rejected",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1002, Type: "supergroup"},
				From: &telego.User{ID: 100},
				Text: "hello",
			},
			wantAccept: false,
		},
		{
			name: "require_mention true, with a leading mention - accepted and stripped",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1002, Type: "supergroup"},
				From: &telego.User{ID: 100},
				Text: "@mtclawbot do the thing",
				Entities: []telego.MessageEntity{
					mentionEntity(0, 10),
				},
			},
			wantAccept: true,
			wantText:   "do the thing",
		},
		{
			name: "mention by reply to the bot's own message",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat:           telego.Chat{ID: -1002, Type: "supergroup"},
				From:           &telego.User{ID: 100},
				Text:           "yes please",
				ReplyToMessage: &telego.Message{From: &telego.User{ID: testBotID}},
			},
			wantAccept: true,
			wantText:   "yes please",
		},
		{
			name: "/cmd@bot treated as a mention",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1002, Type: "supergroup"},
				From: &telego.User{ID: 100},
				Text: "/status@mtclawbot",
				Entities: []telego.MessageEntity{
					botCommandEntity(0, len("/status@mtclawbot")),
				},
			},
			wantAccept: true,
			wantText:   "/status@mtclawbot",
		},
		{
			name: "bare /cmd without @bot in require_mention group - rejected",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1002, Type: "supergroup"},
				From: &telego.User{ID: 100},
				Text: "/status",
				Entities: []telego.MessageEntity{
					botCommandEntity(0, len("/status")),
				},
			},
			wantAccept: false,
		},
		{
			name: "group require_mention false - accepted without mention",
			cfg:  cfgWithGroups,
			msg: &telego.Message{
				Chat: telego.Chat{ID: -1001, Type: "supergroup"},
				From: &telego.User{ID: 100},
				Text: "hello",
			},
			wantAccept: true,
			wantText:   "hello",
		},
		{
			name: "unsupported chat type",
			cfg:  baseCfg,
			msg: &telego.Message{
				Chat: telego.Chat{ID: 100, Type: "channel"},
				From: &telego.User{ID: 100},
				Text: "hello",
			},
			wantAccept: false,
		},
		{
			name:       "nil message",
			cfg:        baseCfg,
			msg:        nil,
			wantAccept: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accept, text, reason := Decide(tc.cfg, testBotUsername, testBotID, tc.msg)
			assert.Equal(t, tc.wantAccept, accept)
			if tc.wantAccept {
				assert.Equal(t, tc.wantText, text)
				assert.Empty(t, reason)
			} else {
				assert.NotEmpty(t, reason, "a rejection must always carry a reason for logging")
			}
		})
	}
}

func TestDecide_PositiveGroupIDConfigIsSimplyAnUnmatchedKey(t *testing.T) {
	// Phase 1 validation is responsible for rejecting a positive-ID group
	// key at config load time; Decide itself just treats it as any other
	// string key that will not match a real (negative) supergroup id, so
	// gating itself never has to special-case it.
	cfg := config.TelegramConfig{
		Groups: map[string]config.TelegramGroupConfig{
			"12345": {},
		},
	}
	msg := &telego.Message{
		Chat: telego.Chat{ID: -1001, Type: "supergroup"},
		From: &telego.User{ID: 100},
		Text: "hello",
	}
	accept, _, reason := Decide(cfg, testBotUsername, testBotID, msg)
	assert.False(t, accept)
	assert.NotEmpty(t, reason)
}

// TestDecide_CaptionUsedWhenTextIsEmpty is the M2 regression test: a
// captioned photo (Text is always empty on media messages; the caption
// lives in a separate field) must not gate through as blank text - Decide
// falls back to Caption so a captioned photo addressed to the bot is not
// silently treated as having nothing to say.
func TestDecide_CaptionUsedWhenTextIsEmpty(t *testing.T) {
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	msg := &telego.Message{
		Chat:    telego.Chat{ID: 100, Type: "private"},
		From:    &telego.User{ID: 100},
		Caption: "what is this?",
	}
	accept, text, reason := Decide(cfg, testBotUsername, testBotID, msg)
	assert.True(t, accept)
	assert.Equal(t, "what is this?", text)
	assert.Empty(t, reason)
}

// TestDecide_CaptionEntitiesUsedForMentionDetection proves the caption
// fallback also swaps in CaptionEntities (not the empty top-level Entities)
// for require_mention's mention check in a group.
func TestDecide_CaptionEntitiesUsedForMentionDetection(t *testing.T) {
	cfg := config.TelegramConfig{
		Groups: map[string]config.TelegramGroupConfig{
			"-1001": {RequireMention: true, AllowFrom: []int64{100}},
		},
	}
	msg := &telego.Message{
		Chat:            telego.Chat{ID: -1001, Type: "supergroup"},
		From:            &telego.User{ID: 100},
		Caption:         "@mtclawbot look at this",
		CaptionEntities: []telego.MessageEntity{mentionEntity(0, len("@mtclawbot"))},
	}
	accept, text, reason := Decide(cfg, testBotUsername, testBotID, msg)
	assert.True(t, accept)
	assert.Equal(t, "look at this", text)
	assert.Empty(t, reason)
}

// TestDecide_TextTakesPriorityOverCaption proves a normal text message
// (Caption empty) is entirely unaffected by the fallback.
func TestDecide_TextTakesPriorityOverCaption(t *testing.T) {
	cfg := config.TelegramConfig{AllowFrom: []int64{100}}
	msg := &telego.Message{
		Chat: telego.Chat{ID: 100, Type: "private"},
		From: &telego.User{ID: 100},
		Text: "hello there",
	}
	accept, text, _ := Decide(cfg, testBotUsername, testBotID, msg)
	assert.True(t, accept)
	assert.Equal(t, "hello there", text)
}
