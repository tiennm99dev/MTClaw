package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
)

func TestNew_MissingAPIKeyNamesConfiguredEnvVar(t *testing.T) {
	cfg := config.OpenAIConfig{APIKeyEnv: "MY_CUSTOM_OPENAI_KEY", BaseURL: "https://api.openai.com/v1"}

	_, err := New(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "MY_CUSTOM_OPENAI_KEY")
}

func TestNew_MissingAPIKeyDefaultsEnvVarName(t *testing.T) {
	cfg := config.OpenAIConfig{BaseURL: "https://api.openai.com/v1"}

	_, err := New(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPENAI_API_KEY")
}
