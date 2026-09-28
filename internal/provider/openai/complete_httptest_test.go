package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// successBody is a minimal, valid chat.completions response body: just
// enough for fromSDK to decode one assistant turn with usage.
const successBody = `{
  "id": "chatcmpl-test",
  "object": "chat.completion",
  "created": 0,
  "model": "test-model",
  "choices": [
    {"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi there"}}
  ],
  "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
}`

// jsonError writes an OpenAI-shaped {"error": {...}} envelope, the exact
// shape the SDK's response decoder unwraps into *openaisdk.Error (see
// requestconfig.Execute's gjson.GetBytes(contents, "error") extraction). A
// numeric code (json.Number, not a quoted string) is what several
// OpenAI-compatible backends (vLLM, llama.cpp front ends) send in place of
// OpenAI's own string error codes.
func jsonError(w http.ResponseWriter, status int, message string, code any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

func testClient(t *testing.T, url string, timeout time.Duration) *Client {
	t.Helper()
	return newClient("test-key", url, timeout, 0)
}

func testRequest() provider.Request {
	return provider.Request{
		Model:    "test-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}
}

// TestComplete_RealTimeoutIsTransientNotCanceled proves a slow backend that
// runs past the configured http.Client.Timeout is reported as ErrTransient,
// not ErrCanceled: ctx here is context.Background() and never expires, so
// the only thing that timed out is the HTTP round trip, and reporting it as
// "the caller already cancelled this" would leave a Telegram turn with no
// reply at all. See provider.Classify's comment for why an error satisfying
// errors.Is(err, context.DeadlineExceeded) is not enough on its own to
// conclude the turn was cancelled.
func TestComplete_RealTimeoutIsTransientNotCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successBody))
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 20*time.Millisecond)

	_, err := client.Complete(context.Background(), testRequest())
	require.Error(t, err)

	var perr *provider.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, provider.ErrTransient, perr.Kind)
}

// TestComplete_RealCancelIsCanceled is the mirror case: ctx itself is
// cancelled mid-request, so the same DeadlineExceeded/Canceled-shaped
// transport error this time must classify as ErrCanceled. The handler sleeps
// a fixed, bounded duration regardless of what the client does - rather than
// waiting on r.Context().Done(), which for a plain HTTP/1.1 handler with no
// request body left to read is not reliably cancelled just because the
// client gave up - so httptest.Server.Close() can never block on a stuck
// handler goroutine.
func TestComplete_RealCancelIsCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successBody))
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	_, err := client.Complete(ctx, testRequest())
	require.Error(t, err)

	var perr *provider.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, provider.ErrCanceled, perr.Kind)
}

// TestComplete_ContextLength_OpenAIStyle covers the shape OpenAI itself
// sends: a string error code equal to context_length_exceeded.
func TestComplete_ContextLength_OpenAIStyle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusBadRequest, "This model's maximum context length is 8192 tokens. However, you requested 9000 tokens.", "context_length_exceeded")
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 5*time.Second)
	_, err := client.Complete(context.Background(), testRequest())
	require.Error(t, err)

	var perr *provider.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, provider.ErrContextLength, perr.Kind)
}

// TestComplete_ContextLength_NumericCodeStyle covers vLLM and llama.cpp
// front ends, which send a *numeric* "code": 400 instead of OpenAI's string
// code - the SDK decodes that into apiErr.Code == "400", a non-empty value
// that used to make isContextLengthError skip the message check entirely
// and misclassify every such response as ErrBadRequest.
func TestComplete_ContextLength_NumericCodeStyle(t *testing.T) {
	cases := []struct {
		name    string
		message string
	}{
		{"vllm", "This model's maximum context length is 4096 tokens."},
		{"llama.cpp", "the request exceeds the available context size, try increasing it"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jsonError(w, http.StatusBadRequest, tc.message, 400)
			}))
			defer srv.Close()

			client := testClient(t, srv.URL, 5*time.Second)
			_, err := client.Complete(context.Background(), testRequest())
			require.Error(t, err)

			var perr *provider.Error
			require.ErrorAs(t, err, &perr)
			assert.Equal(t, provider.ErrContextLength, perr.Kind, "message: %s", tc.message)
		})
	}
}

// TestComplete_Unauthorized401 and TestComplete_RateLimit429 pin the two
// permanent/non-retryable-shaped statuses against a real SDK round trip,
// rather than only the hand-built *openaisdk.Error table in errors_test.go.
func TestComplete_Unauthorized401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusUnauthorized, "Incorrect API key provided", "invalid_api_key")
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 5*time.Second)
	_, err := client.Complete(context.Background(), testRequest())
	require.Error(t, err)

	var perr *provider.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, provider.ErrAuth, perr.Kind)
}

func TestComplete_RateLimit429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusTooManyRequests, "Rate limit reached", "rate_limit_exceeded")
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 5*time.Second)
	_, err := client.Complete(context.Background(), testRequest())
	require.Error(t, err)

	var perr *provider.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, provider.ErrRateLimit, perr.Kind)
}

// TestComplete_OmitsTemperatureWhenUnset proves the actual request body sent
// over the wire carries no "temperature" key at all when Request.Temperature
// is nil, and does carry the value when it is set - the shape gpt-5 and the
// o-series need, since they reject any explicit temperature other than 1.
func TestComplete_OmitsTemperatureWhenUnset(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successBody))
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 5*time.Second)

	_, err := client.Complete(context.Background(), testRequest())
	require.NoError(t, err)
	_, hasTemperature := gotBody["temperature"]
	assert.False(t, hasTemperature, "no temperature key must be sent when Request.Temperature is nil")

	temp := 0.4
	req := testRequest()
	req.Temperature = &temp
	_, err = client.Complete(context.Background(), req)
	require.NoError(t, err)
	require.Contains(t, gotBody, "temperature")
	assert.InDelta(t, 0.4, gotBody["temperature"], 1e-9)
}

// TestProbe_SendsMaxCompletionTokensNotMaxTokens proves the doctor
// reachability probe uses max_completion_tokens: gpt-5 and the o-series
// reject the deprecated max_tokens outright, which used to make Probe report
// a perfectly reachable endpoint as "unreachable".
func TestProbe_SendsMaxCompletionTokensNotMaxTokens(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successBody))
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 5*time.Second)
	result := client.Probe(context.Background(), "gpt-5")
	require.NoError(t, result.Err)

	assert.Contains(t, gotBody, "max_completion_tokens")
	assert.NotContains(t, gotBody, "max_tokens")
}

// TestListModels_ClassifiesRealTransportError is a sanity check that
// ListModels routes its own errors through the same ctx-aware classify as
// Complete and Probe, not a separate ad hoc path.
func TestListModels_ClassifiesRealTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusUnauthorized, "Incorrect API key provided", "invalid_api_key")
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, 5*time.Second)
	_, err := client.ListModels(context.Background())
	require.Error(t, err)

	var perr *provider.Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, provider.ErrAuth, perr.Kind)
}
