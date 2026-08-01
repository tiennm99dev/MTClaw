package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider/mock"
)

func TestLLMClassifier_ValidResponse(t *testing.T) {
	prov := mock.New(mock.Step{Content: `{"risk":"high","categories":["destructive"],"reason":"deletes everything"}`})
	c := &LLMClassifier{prov: prov, model: "test-model"}

	result, err := c.Classify(context.Background(), "rm -rf /tmp/x", "/workspace", []string{"/bin/bash", "-lc"})
	require.NoError(t, err)
	assert.Equal(t, "high", result.Risk)
	assert.Equal(t, []string{"destructive"}, result.Categories)
	assert.Equal(t, "deletes everything", result.Reason)
}

func TestLLMClassifier_StripsMarkdownFence(t *testing.T) {
	prov := mock.New(mock.Step{Content: "```json\n{\"risk\":\"low\",\"categories\":[\"none\"],\"reason\":\"fine\"}\n```"})
	c := &LLMClassifier{prov: prov, model: "test-model"}

	result, err := c.Classify(context.Background(), "ls", "/workspace", []string{"/bin/bash", "-lc"})
	require.NoError(t, err)
	assert.Equal(t, "low", result.Risk)
}

func TestLLMClassifier_MalformedJSON(t *testing.T) {
	prov := mock.New(mock.Step{Content: "this is not json at all"})
	c := &LLMClassifier{prov: prov, model: "test-model"}

	_, err := c.Classify(context.Background(), "ls", "/workspace", nil)
	assert.Error(t, err)
}

func TestLLMClassifier_EmptyBody(t *testing.T) {
	prov := mock.New(mock.Step{Content: ""})
	c := &LLMClassifier{prov: prov, model: "test-model"}

	_, err := c.Classify(context.Background(), "ls", "/workspace", nil)
	assert.Error(t, err)
}

func TestLLMClassifier_InvalidRiskValueIsUnparseable(t *testing.T) {
	prov := mock.New(mock.Step{Content: `{"risk":"extreme","categories":[],"reason":"x"}`})
	c := &LLMClassifier{prov: prov, model: "test-model"}

	_, err := c.Classify(context.Background(), "ls", "/workspace", nil)
	assert.Error(t, err)
}

func TestLLMClassifier_ProviderError(t *testing.T) {
	prov := mock.New(mock.Step{Err: errors.New("provider exploded")})
	c := &LLMClassifier{prov: prov, model: "test-model"}

	_, err := c.Classify(context.Background(), "ls", "/workspace", nil)
	assert.Error(t, err)
}

func TestLLMClassifier_ContextAlreadyDone(t *testing.T) {
	prov := mock.New(mock.Step{Content: `{"risk":"low","categories":["none"],"reason":"x"}`})
	c := &LLMClassifier{prov: prov, model: "test-model"}

	// An already-cancelled context, rather than an expired WithTimeout: a
	// nanosecond timer is not guaranteed to have fired by the time Classify
	// runs when the test host is under load.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Classify(ctx, "ls", "/workspace", nil)
	assert.Error(t, err)
}

func TestBuildClassifierPrompt_ContainsOnlyCommandCWDAndShell(t *testing.T) {
	prompt := buildClassifierPrompt("rm -rf /", "/workspace", []string{"/bin/bash", "-lc"})
	assert.Contains(t, prompt, "rm -rf /")
	assert.Contains(t, prompt, "/workspace")
	assert.Contains(t, prompt, "/bin/bash")
}

func TestExtractJSON_StripsCodeFence(t *testing.T) {
	assert.Equal(t, `{"a":1}`, extractJSON("```json\n{\"a\":1}\n```"))
	assert.Equal(t, `{"a":1}`, extractJSON(`{"a":1}`))
	assert.Equal(t, `{"a":1}`, extractJSON("```\n{\"a\":1}\n```"))
}
