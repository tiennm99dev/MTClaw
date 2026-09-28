package openai

import (
	"context"
	"errors"
	"testing"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// canceledCtx returns a context that is already done, standing in for the
// turn's own ctx having been cancelled or hit its deadline - the one signal
// classify trusts to decide ErrCanceled (see provider.Classify's comment on
// why an error's own shape, e.g. errors.Is(err, context.DeadlineExceeded),
// is not enough on its own: an http.Client.Timeout produces that same shape
// while ctx itself is still healthy).
func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestClassify_Table(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want provider.ErrKind
	}{
		{"401 unauthorized", context.Background(), &openaisdk.Error{StatusCode: 401, Code: "invalid_api_key", Message: "Incorrect API key"}, provider.ErrAuth},
		{"403 forbidden", context.Background(), &openaisdk.Error{StatusCode: 403, Code: "insufficient_permissions", Message: "forbidden"}, provider.ErrAuth},
		{"429 rate limit", context.Background(), &openaisdk.Error{StatusCode: 429, Code: "rate_limit_exceeded", Message: "too many requests"}, provider.ErrRateLimit},
		{"500 server error", context.Background(), &openaisdk.Error{StatusCode: 500, Code: "server_error", Message: "internal error"}, provider.ErrTransient},
		{"400 context length exceeded", context.Background(), &openaisdk.Error{StatusCode: 400, Code: "context_length_exceeded", Message: "This model's maximum context length is 8192 tokens"}, provider.ErrContextLength},
		{"400 other bad request", context.Background(), &openaisdk.Error{StatusCode: 400, Code: "invalid_value", Message: "invalid value for temperature"}, provider.ErrBadRequest},
		{"404 model not found", context.Background(), &openaisdk.Error{StatusCode: 404, Code: "model_not_found", Message: "The model does not exist"}, provider.ErrBadRequest},
		{"422 unprocessable payload", context.Background(), &openaisdk.Error{StatusCode: 422, Code: "invalid_request", Message: "unprocessable entity"}, provider.ErrBadRequest},
		{"400 context length with no code", context.Background(), &openaisdk.Error{StatusCode: 400, Code: "", Message: "This model's maximum context length is 4096 tokens"}, provider.ErrContextLength},
		{"400 context length with a numeric code (vLLM/llama.cpp style)", context.Background(), &openaisdk.Error{StatusCode: 400, Code: "400", Message: "This model's maximum context length is 4096 tokens."}, provider.ErrContextLength},
		{"400 context size phrasing with a numeric code (llama.cpp)", context.Background(), &openaisdk.Error{StatusCode: 400, Code: "400", Message: "the request exceeds the available context size, try increasing it"}, provider.ErrContextLength},
		{"413 payload too large", context.Background(), &openaisdk.Error{StatusCode: 413, Code: "", Message: "payload too large"}, provider.ErrContextLength},
		{"409 conflict", context.Background(), &openaisdk.Error{StatusCode: 409, Code: "conflict", Message: "resource conflict"}, provider.ErrTransient},
		{"context actually cancelled", canceledCtx(), context.Canceled, provider.ErrCanceled},
		{"context actually past its deadline", canceledCtx(), context.DeadlineExceeded, provider.ErrCanceled},
		{"ctx cancelled overrides an unrelated error", canceledCtx(), errors.New("connection reset by peer"), provider.ErrCanceled},
		{"a live ctx with a DeadlineExceeded-shaped error (e.g. http.Client.Timeout) is transient, not cancelled", context.Background(), context.DeadlineExceeded, provider.ErrTransient},
		{"unrecognized transport error with a live ctx falls back to transient", context.Background(), errors.New("connection reset by peer"), provider.ErrTransient},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.ctx, tc.err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Kind, "kind for %s", tc.name)
		})
	}
}

func TestClassify_ContextLengthDistinguishableFromOtherBadRequest(t *testing.T) {
	ctxLen := classify(context.Background(), &openaisdk.Error{StatusCode: 400, Code: "context_length_exceeded", Message: "too long"})
	otherBad := classify(context.Background(), &openaisdk.Error{StatusCode: 400, Code: "invalid_value", Message: "bad value"})

	require.NotNil(t, ctxLen)
	require.NotNil(t, otherBad)
	assert.Equal(t, provider.ErrContextLength, ctxLen.Kind)
	assert.Equal(t, provider.ErrBadRequest, otherBad.Kind)
	assert.NotEqual(t, ctxLen.Kind, otherBad.Kind)
}

func TestClassify_Nil(t *testing.T) {
	assert.Nil(t, classify(context.Background(), nil))
}

func TestClassify_UnwrapPreservesCause(t *testing.T) {
	cause := context.DeadlineExceeded
	got := classify(canceledCtx(), cause)
	require.NotNil(t, got)
	assert.ErrorIs(t, got, context.DeadlineExceeded)
}

// TestClassify_TimeoutViaContextWithDeadline is an integration-style check
// using a real expired context, matching how Complete would actually see
// ctx.Err() once its own deadline (not just the HTTP client's timeout) has
// passed.
func TestClassify_TimeoutViaContextWithDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	got := classify(ctx, ctx.Err())
	require.NotNil(t, got)
	assert.Equal(t, provider.ErrCanceled, got.Kind)
}
