package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/channel/telegram"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/testsupport"
)

// useFakeHome points os.UserHomeDir() (HOME on POSIX, USERPROFILE on
// Windows) at a fresh temp directory for the duration of one test.
// config.StateDir() and every "~/..." default in config.Default() resolve
// through the home directory, and onboard's own doctor step at the end of
// its run (step 9) exercises those unconditionally - without this, every
// onboard test would create a real ~/.mtclaw (database, prompts/AGENTS.md)
// on whatever machine runs the test suite. The implementation itself lives
// in internal/testsupport.FakeHome, generalized for internal/gateway's and
// internal/cli's e2e suites to share; this is a one-line delegate so every
// existing call site here keeps working unchanged.
func useFakeHome(t *testing.T) string {
	t.Helper()
	return testsupport.FakeHome(t)
}

// scriptedPrompter drives onboard's prompter interface from a fixed queue
// of scripted answers instead of a TTY, so onboard_test.go never blocks on
// real stdin. Each queue is consumed in call order; running out of answers
// fails the test loudly (via t.Fatalf) rather than hanging, so a test that
// does not script enough answers is caught immediately instead of timing
// out.
type scriptedPrompter struct {
	t        *testing.T
	texts    []string
	secrets  []string
	confirms []bool
	out      bytes.Buffer
}

var _ prompter = (*scriptedPrompter)(nil)

func (p *scriptedPrompter) Text(question, def string) (string, error) {
	if len(p.texts) == 0 {
		p.t.Fatalf("scriptedPrompter: no scripted Text answer left for question %q", question)
	}
	v := p.texts[0]
	p.texts = p.texts[1:]
	if v == "" {
		return def, nil
	}
	return v, nil
}

func (p *scriptedPrompter) Secret(question string) (string, error) {
	if len(p.secrets) == 0 {
		p.t.Fatalf("scriptedPrompter: no scripted Secret answer left for question %q", question)
	}
	v := p.secrets[0]
	p.secrets = p.secrets[1:]
	return v, nil
}

func (p *scriptedPrompter) Confirm(question string, defaultYes bool) (bool, error) {
	if len(p.confirms) == 0 {
		p.t.Fatalf("scriptedPrompter: no scripted Confirm answer left for question %q", question)
	}
	v := p.confirms[0]
	p.confirms = p.confirms[1:]
	return v, nil
}

func (p *scriptedPrompter) Printf(format string, args ...any) {
	fmt.Fprintf(&p.out, format, args...)
}

// fakeTelegramCapturer is telegramCapturer's test double: GetMe and Capture
// return exactly what the test configured, with no network and no real
// wait for the capture window.
type fakeTelegramCapturer struct {
	getMeUsername string
	getMeErr      error
	senders       []telegram.Sender
	captureErr    error
}

var _ telegramCapturer = (*fakeTelegramCapturer)(nil)

func (f *fakeTelegramCapturer) GetMe(_ context.Context, _ string) (string, error) {
	return f.getMeUsername, f.getMeErr
}

func (f *fakeTelegramCapturer) Capture(_ context.Context, _ string, _ time.Duration) ([]telegram.Sender, error) {
	return f.senders, f.captureErr
}

// onboardConfigPath returns a config path inside a fresh temp directory
// that does not yet exist, matching the fresh-machine scenario onboard is
// meant for.
func onboardConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "mtclaw", "config.yaml")
}

func TestRunOnboard_TelegramDisabled_WritesLoadableConfigWithNoSecret(t *testing.T) {
	useFakeHome(t)
	configPath := onboardConfigPath(t)
	const secretValue = "sk-should-never-be-written-1234567890"

	p := &scriptedPrompter{
		t:        t,
		texts:    []string{"", "gpt-4o-mini", filepath.Join(t.TempDir(), "workspace")},
		secrets:  []string{secretValue},
		confirms: []bool{false}, // "Enable the Telegram channel?" -> no
	}
	capturer := &fakeTelegramCapturer{}

	err := runOnboard(context.Background(), &p.out, configPath, p, capturer)
	require.NoError(t, err)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), secretValue, "onboard must never write a secret value into the config file")
	assert.Contains(t, string(data), "api_key_env: OPENAI_API_KEY")

	cfg, err := config.LoadFile(configPath)
	require.NoError(t, err, "the config onboard writes must be loadable and pass Validate")
	assert.Equal(t, "gpt-4o-mini", cfg.Agent.Model)
	assert.False(t, cfg.Channels.Telegram.Enabled)
	assert.Equal(t, "approval", cfg.Tools.Exec.Mode, "onboard must never write exec.mode: auto")
	assert.NotEmpty(t, cfg.Tools.Exec.Deny, "onboard must write a starter deny-list")
	require.Len(t, cfg.Agent.SystemPromptFiles, 1)
	assert.FileExists(t, cfg.Agent.SystemPromptFiles[0])
}

func TestRunOnboard_TelegramEnabled_SingleSenderConfirmedIsWritten(t *testing.T) {
	useFakeHome(t)
	configPath := onboardConfigPath(t)
	const tokenValue = "123456:fake-token-should-never-be-written"

	p := &scriptedPrompter{
		t:        t,
		texts:    []string{"", "gpt-4o-mini", "", filepath.Join(t.TempDir(), "workspace")},
		secrets:  []string{"", tokenValue}, // skip openai verification, then telegram token
		confirms: []bool{true, true},       // enable telegram; confirm the single sender's id
	}
	capturer := &fakeTelegramCapturer{
		getMeUsername: "my_test_bot",
		senders:       []telegram.Sender{{UserID: 42, Username: "alice"}},
	}

	err := runOnboard(context.Background(), &p.out, configPath, p, capturer)
	require.NoError(t, err)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), tokenValue)

	cfg, err := config.LoadFile(configPath)
	require.NoError(t, err)
	assert.True(t, cfg.Channels.Telegram.Enabled)
	assert.Equal(t, []int64{42}, cfg.Channels.Telegram.AllowFrom)
}

func TestRunOnboard_TelegramEnabled_MultipleSendersWritesNone(t *testing.T) {
	useFakeHome(t)
	configPath := onboardConfigPath(t)

	p := &scriptedPrompter{
		t:        t,
		texts:    []string{"", "gpt-4o-mini", "", "", filepath.Join(t.TempDir(), "workspace")}, // "" manual id entry -> blank
		secrets:  []string{"", "123456:fake-token"},
		confirms: []bool{true}, // enable telegram
	}
	capturer := &fakeTelegramCapturer{
		getMeUsername: "my_test_bot",
		senders: []telegram.Sender{
			{UserID: 42, Username: "alice"},
			{UserID: 43, Username: "mallory"},
		},
	}

	err := runOnboard(context.Background(), &p.out, configPath, p, capturer)
	require.NoError(t, err)

	cfg, err := config.LoadFile(configPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.Channels.Telegram.AllowFrom, "multiple distinct senders must never be auto-written")
	assert.False(t, cfg.Channels.Telegram.Enabled, "an empty allowlist must not be written as enabled (config.Validate would reject it)")
	assert.Contains(t, p.out.String(), "alice")
	assert.Contains(t, p.out.String(), "mallory")
}

func TestRunOnboard_TelegramEnabled_NoSenderFallsBackToManualEntry(t *testing.T) {
	useFakeHome(t)
	configPath := onboardConfigPath(t)

	p := &scriptedPrompter{
		t:        t,
		texts:    []string{"", "gpt-4o-mini", "", "555", filepath.Join(t.TempDir(), "workspace")},
		secrets:  []string{"", "123456:fake-token"},
		confirms: []bool{true},
	}
	capturer := &fakeTelegramCapturer{getMeUsername: "my_test_bot"} // no senders

	err := runOnboard(context.Background(), &p.out, configPath, p, capturer)
	require.NoError(t, err)

	cfg, err := config.LoadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, []int64{555}, cfg.Channels.Telegram.AllowFrom)
}

func TestRunOnboard_RefusesToOverwriteExistingConfig(t *testing.T) {
	useFakeHome(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	original := []byte("version: 1\nagent:\n  model: gpt-4o-mini\nchannels:\n  telegram:\n    enabled: false\nstorage:\n  path: \"" + filepath.ToSlash(filepath.Join(dir, "mtclaw.db")) + "\"\n")
	require.NoError(t, os.WriteFile(configPath, original, 0o600))

	p := &scriptedPrompter{t: t}
	err := runOnboard(context.Background(), &p.out, configPath, p, &fakeTelegramCapturer{})
	require.NoError(t, err, "refusing to overwrite is not itself an error")

	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, original, after, "the existing config file must be byte-for-byte untouched")
	assert.Contains(t, p.out.String(), "refuses to overwrite")
}
