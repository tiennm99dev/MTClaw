// Package openai wraps github.com/openai/openai-go/v3 behind
// internal/provider's Provider interface. This is the only package in the
// module permitted to import the SDK; the agent loop and everything else
// depend on internal/provider's types instead.
package openai

import (
	"context"
	"fmt"
	"net/http"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
)

// Client implements provider.Provider over the OpenAI chat completions API.
type Client struct {
	sdk     openaisdk.Client
	baseURL string
}

var _ provider.Provider = (*Client)(nil)

// New constructs a Client from the resolved OpenAI configuration. cfg.APIKey
// must already have been resolved by config.Load (via api_key_env or
// api_key_file); New errors out immediately, naming the configured source,
// rather than deferring to a confusing 401 on the first call.
func New(cfg config.OpenAIConfig) (*Client, error) {
	apiKey := cfg.APIKey()
	if apiKey == "" {
		envName := cfg.APIKeyEnv
		if envName == "" {
			envName = "OPENAI_API_KEY"
		}
		return nil, fmt.Errorf("openai: no API key resolved (set %s or configure openai.api_key_file)", envName)
	}
	return newClient(apiKey, cfg.BaseURL, cfg.Timeout.Std(), cfg.MaxRetries), nil
}

// NewWithAPIKey builds a Client directly from a raw API key, bypassing
// config.OpenAIConfig's env/file secret indirection entirely. `onboard` uses
// this to verify a key value the user just typed at a prompt: that value is
// never persisted and never round-tripped through a config.Config the normal
// way (New) would require, so this is the one place in the codebase that
// hands a live API key to the SDK client without it first passing through
// config.Load's resolution.
func NewWithAPIKey(apiKey, baseURL string, timeout time.Duration) (*Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("openai: empty API key")
	}
	// Mirrors New's retry budget (config.Default().OpenAI.MaxRetries) even
	// though there is no config.OpenAIConfig in scope here, so a key check
	// at onboarding does not silently fall back to the SDK's own default
	// retry count.
	return newClient(apiKey, baseURL, timeout, config.Default().OpenAI.MaxRetries), nil
}

// newClient builds the SDK client both New and NewWithAPIKey construct: the
// two differ only in how they obtain apiKey (config.Load's resolution vs. a
// raw value handed in directly), never in how the resulting option set is
// built, so a change to the http.Client timeout or any other option only
// needs to happen once, here.
func newClient(apiKey, baseURL string, timeout time.Duration, maxRetries int) *Client {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(maxRetries),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if timeout > 0 {
		opts = append(opts, option.WithHTTPClient(&http.Client{Timeout: timeout}))
	}
	return &Client{sdk: openaisdk.NewClient(opts...), baseURL: baseURL}
}

// ListModels returns every model ID the configured endpoint currently
// reports. `doctor`'s "model exists" check uses this to catch a typo'd
// agent.model at setup time instead of at the first real turn; the
// underlying /models endpoint is not actually paginated by OpenAI (see the
// SDK's own pagination.Page.GetNextPage doc comment), so one call is enough.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	page, err := c.sdk.Models.List(ctx)
	if err != nil {
		return nil, classify(ctx, err)
	}
	ids := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}
