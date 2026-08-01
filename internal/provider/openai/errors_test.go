package openai

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// fakeTimeoutErr is a minimal net.Error implementation for exercising the
// net.Error timeout branch without opening a real socket.
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

var _ net.Error = fakeTimeoutErr{}

func TestClassify_Table(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want provider.ErrKind
	}{
		{"401 unauthorized", &openaisdk.Error{StatusCode: 401, Code: "invalid_api_key", Message: "Incorrect API key"}, provider.ErrAuth},
		{"403 forbidden", &openaisdk.Error{StatusCode: 403, Code: "insufficient_permissions", Message: "forbidden"}, provider.ErrAuth},
		{"429 rate limit", &openaisdk.Error{StatusCode: 429, Code: "rate_limit_exceeded", Message: "too many requests"}, provider.ErrRateLimit},
		{"500 server error", &openaisdk.Error{StatusCode: 500, Code: "server_error", Message: "internal error"}, provider.ErrTransient},
		{"400 context length exceeded", &openaisdk.Error{StatusCode: 400, Code: "context_length_exceeded", Message: "This model's maximum context length is 8192 tokens"}, provider.ErrContextLength},
		{"400 other bad request", &openaisdk.Error{StatusCode: 400, Code: "invalid_value", Message: "invalid value for temperature"}, provider.ErrBadRequest},
		{"context canceled", context.Canceled, provider.ErrCanceled},
		{"context deadline exceeded", context.DeadlineExceeded, provider.ErrCanceled},
		{"net timeout", fakeTimeoutErr{}, provider.ErrTransient},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Kind, "kind for %s", tc.name)
		})
	}
}

func TestClassify_ContextLengthDistinguishableFromOtherBadRequest(t *testing.T) {
	ctxLen := classify(&openaisdk.Error{StatusCode: 400, Code: "context_length_exceeded", Message: "too long"})
	otherBad := classify(&openaisdk.Error{StatusCode: 400, Code: "invalid_value", Message: "bad value"})

	require.NotNil(t, ctxLen)
	require.NotNil(t, otherBad)
	assert.Equal(t, provider.ErrContextLength, ctxLen.Kind)
	assert.Equal(t, provider.ErrBadRequest, otherBad.Kind)
	assert.NotEqual(t, ctxLen.Kind, otherBad.Kind)
}

func TestClassify_ChecksContextCancellationBeforeAPIError(t *testing.T) {
	// A context error wrapped alongside transport noise must still be
	// classified as ErrCanceled, checked ahead of any API-error
	// inspection.
	wrapped := errors.Join(context.Canceled, errors.New("request aborted"))
	got := classify(wrapped)
	require.NotNil(t, got)
	assert.Equal(t, provider.ErrCanceled, got.Kind)
}

func TestClassify_Nil(t *testing.T) {
	assert.Nil(t, classify(nil))
}

func TestClassify_UnwrapPreservesCause(t *testing.T) {
	cause := context.DeadlineExceeded
	got := classify(cause)
	require.NotNil(t, got)
	assert.ErrorIs(t, got, context.DeadlineExceeded)
}

// TestClassify_TimeoutViaContextWithDeadline is an integration-style check
// using a real expired context, matching how Complete would actually see
// context.DeadlineExceeded.
func TestClassify_TimeoutViaContextWithDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	got := classify(ctx.Err())
	require.NotNil(t, got)
	assert.Equal(t, provider.ErrCanceled, got.Kind)
}
