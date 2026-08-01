package openai

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
)

// TestE2E_RealCompletion is a network-gated smoke test: it only runs when
// MTCLAW_E2E=1 and OPENAI_API_KEY are both set, so `go test ./...` stays
// hermetic by default. Run explicitly with:
//
//	MTCLAW_E2E=1 OPENAI_API_KEY=sk-... go test ./internal/provider/openai/... -run TestE2E
func TestE2E_RealCompletion(t *testing.T) {
	if os.Getenv("MTCLAW_E2E") != "1" {
		t.Skip("set MTCLAW_E2E=1 (and OPENAI_API_KEY) to run the network-gated OpenAI E2E test")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY not set")
	}

	model := os.Getenv("MTCLAW_E2E_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	yamlDoc := `
version: 1
agent:
  model: ` + model + `
channels:
  telegram:
    enabled: false
storage:
  path: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "mtclaw.db")) + `"
`
	env := map[string]string{"OPENAI_API_KEY": os.Getenv("OPENAI_API_KEY")}
	cfg, err := config.Load([]byte(yamlDoc), t.TempDir(), env)
	require.NoError(t, err)

	client, err := New(cfg.OpenAI)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := client.Probe(ctx, model)
	require.NoError(t, result.Err)
	require.NotZero(t, result.Latency)
	t.Logf("probe ok: base_url=%s model=%s latency=%s", result.BaseURL, result.Model, result.Latency)
}
