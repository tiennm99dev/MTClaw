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
		case status == 404, status == 422:
			// A permanent fault in the request itself: 404 model_not_found
			// (a typo'd or retired agent.model), 422 for a malformed
			// payload. Retrying will not fix these.
			return &provider.Error{Kind: provider.ErrBadRequest, Status: status, Msg: msg, Err: err}
		case status == 400:
			// Any other 400 not covered by the context-length case above.
			return &provider.Error{Kind: provider.ErrBadRequest, Status: status, Msg: msg, Err: err}
		case status == 408, status == 409, status == 425:
			// Request timeout, conflict, and too-early: the caller may
			// retry, same as a 5xx.
			return &provider.Error{Kind: provider.ErrTransient, Status: status, Msg: msg, Err: err}
		default:
			// Any other 4xx and 5xx.
			return &provider.Error{Kind: provider.ErrTransient, Status: status, Msg: msg, Err: err}
		}
	}

	return provider.Classify(err)
}

// isContextLengthError reports whether apiErr represents a context-window
// overflow. The stable error code is the primary signal, since it survives
// OpenAI rewording the message. base_url is a documented config knob that
// can point at any OpenAI-compatible endpoint (vLLM, llama.cpp, proxies,
// ...), and those routinely return a 400 with an empty Code for the same
// condition; when Code is empty, fall back to matching common overflow
// phrasing in the message so the agent loop's trim-and-retry recovery still
// fires against those backends instead of degrading to a permanent
// ErrBadRequest on every such backend.
func isContextLengthError(apiErr *openaisdk.Error) bool {
	if apiErr.Code == codeContextLengthExceeded {
		return true
	}
	if apiErr.Code != "" {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "context_length") ||
		strings.Contains(msg, "context length") ||
		strings.Contains(msg, "maximum context length")
}
