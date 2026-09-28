package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestError_ErrorStringFoldsKindAndStatusIn(t *testing.T) {
	withStatus := &Error{Kind: ErrBadRequest, Status: 400, Msg: "invalid value for temperature"}
	assert.Equal(t, "bad_request (400): invalid value for temperature", withStatus.Error())

	withoutStatus := &Error{Kind: ErrCanceled, Msg: "context canceled"}
	assert.Equal(t, "canceled: context canceled", withoutStatus.Error())

	fallsBackToWrapped := &Error{Kind: ErrTransient, Err: errors.New("connection reset")}
	assert.Equal(t, "transient: connection reset", fallsBackToWrapped.Error())

	noMessageAtAll := &Error{Kind: ErrAuth}
	assert.Equal(t, "auth", noMessageAtAll.Error())
}

func TestError_UnwrapReachesTheOriginalCause(t *testing.T) {
	cause := errors.New("boom")
	err := &Error{Kind: ErrTransient, Err: cause}
	assert.ErrorIs(t, err, cause)
}

func TestClassify_Nil(t *testing.T) {
	assert.Nil(t, Classify(context.Background(), nil))
}

// TestClassify_PassesThroughAnAlreadyClassifiedError proves Classify never
// reclassifies an error a provider-specific classifier already turned into
// an *Error, even when ctx is done: the more specific classification (e.g.
// ErrAuth from a real 401) is always preserved over the generic ctx-based
// fallback.
func TestClassify_PassesThroughAnAlreadyClassifiedError(t *testing.T) {
	already := &Error{Kind: ErrAuth, Status: 401, Msg: "bad key"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := Classify(ctx, already)
	assert.Same(t, already, got)
}

func TestClassify_CtxDoneMeansCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := Classify(ctx, errors.New("connection reset by peer"))
	require.NotNil(t, got)
	assert.Equal(t, ErrCanceled, got.Kind)
}

// TestClassify_LiveCtxFallsBackToTransient proves the same distinction the
// openai package's own classifier makes: an error satisfying
// errors.Is(err, context.DeadlineExceeded) must not be reported as
// ErrCanceled when ctx itself never expired (the shape an
// http.Client.Timeout produces).
func TestClassify_LiveCtxFallsBackToTransient(t *testing.T) {
	got := Classify(context.Background(), context.DeadlineExceeded)
	require.NotNil(t, got)
	assert.Equal(t, ErrTransient, got.Kind)
	assert.ErrorIs(t, got, context.DeadlineExceeded)
}

func TestErrKind_StringCoversEveryKind(t *testing.T) {
	cases := map[ErrKind]string{
		ErrTransient:     "transient",
		ErrAuth:          "auth",
		ErrRateLimit:     "rate_limit",
		ErrContextLength: "context_length",
		ErrBadRequest:    "bad_request",
		ErrCanceled:      "canceled",
	}
	for kind, want := range cases {
		assert.Equal(t, want, kind.String())
	}
	assert.Equal(t, "unknown", ErrKind(99).String())
}
