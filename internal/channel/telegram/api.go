package telegram

import (
	"context"

	"github.com/mymmrac/telego"
)

// botAPI is the slice of *telego.Bot this package actually calls. Defining
// it as an interface - rather than depending on *telego.Bot concretely
// everywhere - lets tests inject a fake transport, since telego.Bot itself
// is a concrete struct with no seam for stubbing the network: see
// gating_test.go's and chunk_test.go's sibling send/approver tests for the
// fakes that implement this.
type botAPI interface {
	GetMe(ctx context.Context) (*telego.User, error)
	SetMyCommands(ctx context.Context, params *telego.SetMyCommandsParams) error
	SendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error)
	EditMessageText(ctx context.Context, params *telego.EditMessageTextParams) (*telego.Message, error)
	AnswerCallbackQuery(ctx context.Context, params *telego.AnswerCallbackQueryParams) error
	SendChatAction(ctx context.Context, params *telego.SendChatActionParams) error
	UpdatesViaLongPolling(ctx context.Context, params *telego.GetUpdatesParams, options ...telego.LongPollingOption) (<-chan telego.Update, error)
}

var _ botAPI = (*telego.Bot)(nil)
