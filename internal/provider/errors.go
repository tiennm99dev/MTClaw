package provider

import (
	"context"
	"errors"
	"net"
)

// ErrKind classifies why a Provider.Complete call failed, so the agent loop
// can react without inspecting a concrete SDK error type.
type ErrKind int

const (
	// ErrTransient covers 5xx responses, a 429 whose retries were already
	// exhausted by the SDK's own retry layer, connection resets, and
	// network timeouts. The caller may retry.
	ErrTransient ErrKind = iota
	// ErrAuth covers 401/403: the configured credential is missing or
	// rejected. Retrying will not help.
	ErrAuth
	// ErrRateLimit covers a 429 that persisted after the SDK's retries
	// were exhausted.
	ErrRateLimit
	// ErrContextLength covers a 400 whose error code is
	// context_length_exceeded. The agent loop reacts to this
	// specifically: trim history and retry once.
	ErrContextLength
	// ErrBadRequest covers any other 400 - a bug in our own request that
	// retrying will not fix.
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

func (e *Error) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Kind.String()
}

// Unwrap exposes the underlying error so callers can still errors.Is/As
// through to the original cause (e.g. context.Canceled).
func (e *Error) Unwrap() error { return e.Err }

// Classify maps a transport-level Go error - context cancellation or a
// network timeout - into our Error taxonomy. It is the fallback every
// provider-specific classifier (e.g. internal/provider/openai's) calls once
// it has ruled out its own SDK-specific error type, so the ctx/net handling
// stays in one place rather than duplicated per provider.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}

	var already *Error
	if errors.As(err, &already) {
		return already
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrCanceled, Msg: err.Error(), Err: err}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Kind: ErrTransient, Msg: err.Error(), Err: err}
	}

	return &Error{Kind: ErrTransient, Msg: err.Error(), Err: err}
}
