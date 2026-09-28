package provider

import (
	"context"
	"errors"
	"fmt"
)

// ErrKind classifies why a Provider.Complete call failed, so the agent loop
// can react without inspecting a concrete SDK error type.
type ErrKind int

const (
	// ErrTransient covers 5xx responses, 408/409/425, connection resets,
	// and network timeouts. The caller may retry.
	ErrTransient ErrKind = iota
	// ErrAuth covers 401/403: the configured credential is missing or
	// rejected. Retrying will not help.
	ErrAuth
	// ErrRateLimit covers a 429 that persisted after the SDK's retries
	// were exhausted.
	ErrRateLimit
	// ErrContextLength covers a 400 whose error code is
	// context_length_exceeded (or, when the backend left the code empty,
	// whose message matches common overflow phrasing - see
	// openai.isContextLengthError), and a 413 Payload Too Large, which
	// several OpenAI-compatible front ends use to report the same
	// condition. The agent loop reacts to this specifically: trim history
	// and retry once.
	ErrContextLength
	// ErrBadRequest covers a 400 that is not a context-window overflow,
	// plus other permanent 4xx faults in the request itself (404 model not
	// found, 422 malformed payload). Retrying will not fix these.
	ErrBadRequest
	// ErrCanceled covers ctx cancellation or deadline expiry.
	ErrCanceled
)

// String renders the kind for logs and test failure messages.
func (k ErrKind) String() string {
	switch k {
	case ErrTransient:
		return "transient"
	case ErrAuth:
		return "auth"
	case ErrRateLimit:
		return "rate_limit"
	case ErrContextLength:
		return "context_length"
	case ErrBadRequest:
		return "bad_request"
	case ErrCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Error is the error type every Provider implementation returns from
// Complete. Status is the HTTP status code when known, or 0 otherwise.
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	Err    error
}

// Error renders the kind (and HTTP status, when known) alongside the
// underlying message, so a log line built from this error - the loop and
// gateway branch on Kind directly and never need to parse this string back
// out - still shows the taxonomy a caller classified this as, not just
// whatever text the backend happened to send.
func (e *Error) Error() string {
	msg := e.Msg
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg == "" {
		return e.Kind.String()
	}
	if e.Status != 0 {
		return fmt.Sprintf("%s (%d): %s", e.Kind, e.Status, msg)
	}
	return fmt.Sprintf("%s: %s", e.Kind, msg)
}

// Unwrap exposes the underlying error so callers can still errors.Is/As
// through to the original cause (e.g. context.Canceled).
func (e *Error) Unwrap() error { return e.Err }

// Classify maps a transport-level Go error - context cancellation or a
// network timeout - into our Error taxonomy. It is the fallback every
// provider-specific classifier (e.g. internal/provider/openai's) calls once
// it has ruled out its own SDK-specific error type, so the ctx/net handling
// stays in one place rather than duplicated per provider.
//
// ctx, not err's shape, is the only source of truth for whether this was a
// cancellation: an HTTP client's own request timeout (e.g. http.Client.
// Timeout) also produces an error satisfying errors.Is(err,
// context.DeadlineExceeded), even though ctx itself never expired and the
// caller got no cancellation signal at all. Trusting err's shape there would
// report a slow backend as "the caller already cancelled this, say nothing",
// leaving the turn with no reply. Only ctx.Err() != nil means the caller
// actually gave up.
func Classify(ctx context.Context, err error) *Error {
	if err == nil {
		return nil
	}

	var already *Error
	if errors.As(err, &already) {
		return already
	}

	if ctx != nil && ctx.Err() != nil {
		return &Error{Kind: ErrCanceled, Msg: err.Error(), Err: err}
	}

	// Anything else - a network timeout, a connection reset, a raw
	// transport error a provider-specific classifier did not recognize -
	// is treated as transient: the caller may retry.
	return &Error{Kind: ErrTransient, Msg: err.Error(), Err: err}
}
