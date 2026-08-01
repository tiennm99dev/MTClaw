package openai

import (
	"context"
	"errors"

	openaisdk "github.com/openai/openai-go/v3"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// codeContextLengthExceeded is the OpenAI error *code* (not the English
// message text) that marks a 400 as a context-window overflow rather than
// an ordinary bad request. Matching on the message would break the moment
// OpenAI rewords it.
const codeContextLengthExceeded = "context_length_exceeded"

// classify maps an error returned by the SDK into provider.Error. Context
// cancellation is checked first - it can occur independent of any HTTP
// response - then the SDK's *openai.Error is inspected for HTTP status and
// error code. Anything else (a raw network error, for instance) falls back
// to provider.Classify.
//
// Retries: the SDK already retried transient failures per
// option.WithMaxRetries before returning; classify only runs once the SDK
// has given up, so ErrTransient here means "still failing after retries",
// not "retry again yourself".
func classify(err error) *provider.Error {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &provider.Error{Kind: provider.ErrCanceled, Msg: err.Error(), Err: err}
	}

	var apiErr *openaisdk.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		msg := apiErr.Message
		switch {
		case status == 401, status == 403:
			return &provider.Error{Kind: provider.ErrAuth, Status: status, Msg: msg, Err: err}
		case status == 429:
			return &provider.Error{Kind: provider.ErrRateLimit, Status: status, Msg: msg, Err: err}
		case status == 400 && apiErr.Code == codeContextLengthExceeded:
			return &provider.Error{Kind: provider.ErrContextLength, Status: status, Msg: msg, Err: err}
		case status == 400:
			return &provider.Error{Kind: provider.ErrBadRequest, Status: status, Msg: msg, Err: err}
		case status >= 500:
			return &provider.Error{Kind: provider.ErrTransient, Status: status, Msg: msg, Err: err}
		default:
			return &provider.Error{Kind: provider.ErrTransient, Status: status, Msg: msg, Err: err}
		}
	}

	return provider.Classify(err)
}
