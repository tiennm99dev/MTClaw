package openai

import (
	"context"
	"errors"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// codeContextLengthExceeded is the OpenAI error *code* (not the English
// message text) that marks a 400 as a context-window overflow rather than
// an ordinary bad request. Matching on the message would break the moment
// OpenAI rewords it - which is why isContextLengthError checks this code
// first and only falls back to a message match when the backend left Code
// empty (see that function's comment).
const codeContextLengthExceeded = "context_length_exceeded"

// classify maps an error returned by the SDK into provider.Error. ctx decides
// cancellation (see provider.Classify's comment on why the error's shape
// alone cannot: an http.Client.Timeout also produces a DeadlineExceeded-
// shaped error with ctx still healthy). Once cancellation is ruled out, the
// SDK's *openai.Error is inspected for HTTP status and error code. Anything
// else (a raw network error, for instance) falls back to provider.Classify.
//
// Retries: the SDK already retried transient failures per
// option.WithMaxRetries before returning; classify only runs once the SDK
// has given up, so ErrTransient here means "still failing after retries",
// not "retry again yourself".
func classify(ctx context.Context, err error) *provider.Error {
	if err == nil {
		return nil
	}

	if ctx != nil && ctx.Err() != nil {
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
		case status == 400 && isContextLengthError(apiErr):
			return &provider.Error{Kind: provider.ErrContextLength, Status: status, Msg: msg, Err: err}
		case status == 413:
			// Payload Too Large is how several OpenAI-compatible front
			// ends (nginx, llama.cpp-style proxies) report a
			// context-window overflow. Unlike a 400, there is no other
			// reason an LLM completion API returns 413, so it needs no
			// message-based disambiguation the way isContextLengthError
			// gives 400 - classifying it as anything but ErrContextLength
			// would permanently deny the loop's trim-and-retry recovery
			// the exact backends base_url is meant to support.
			return &provider.Error{Kind: provider.ErrContextLength, Status: status, Msg: msg, Err: err}
		case status == 400, status == 404, status == 422:
			// A permanent fault in the request itself, not covered by the
			// context-length case above: 400 for anything else, 404
			// model_not_found (a typo'd or retired agent.model), 422 for a
			// malformed payload. Retrying will not fix these.
			return &provider.Error{Kind: provider.ErrBadRequest, Status: status, Msg: msg, Err: err}
		default:
			// 408/409/425 and every other 4xx/5xx: the caller may retry,
			// same as a plain 5xx. Nothing downstream branches on Kind
			// beyond ErrCanceled and ErrContextLength (see Error(), which
			// folds Kind and Status into the message so the taxonomy still
			// reaches a log even without a dedicated consumer), so a wider
			// case list here would only be more branches to keep in sync.
			return &provider.Error{Kind: provider.ErrTransient, Status: status, Msg: msg, Err: err}
		}
	}

	return provider.Classify(ctx, err)
}

// isContextLengthError reports whether apiErr represents a context-window
// overflow. The stable error code is authoritative when OpenAI itself sets
// it. base_url is a documented config knob that can point at any
// OpenAI-compatible endpoint (vLLM, llama.cpp, proxies, ...), and those
// routinely send a *numeric* "code": 400, which the SDK decodes into
// apiErr.Code == "400" - a value that is present but not
// codeContextLengthExceeded, so it must not short-circuit the message check
// the way an empty Code used to. Whenever Code is anything other than the
// OpenAI-defined value, the message is checked instead, so the agent loop's
// trim-and-retry recovery still fires against those backends instead of
// degrading to a permanent ErrBadRequest.
func isContextLengthError(apiErr *openaisdk.Error) bool {
	if apiErr.Code == codeContextLengthExceeded {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "context_length") ||
		strings.Contains(msg, "context length") ||
		strings.Contains(msg, "context size") ||
		strings.Contains(msg, "context window")
}
