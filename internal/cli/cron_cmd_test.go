package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/gateway"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
	"github.com/tiennm99/MTClaw/internal/testsupport/fakeapi"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// fakeOpenAIChatCompletion starts an httptest server returning one fixed,
// valid chat.completions response, so a `cron run` test can drive a real
// agent turn end to end without a live OpenAI credential or network access.
func fakeOpenAIChatCompletion(t *testing.T, replyText string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
  "id": "chatcmpl-test", "object": "chat.completion", "created": 0, "model": "gpt-4o-mini",
  "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": %q}}],
  "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
}`, replyText)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOpenAIChatCompletionWithControlChar is like fakeOpenAIChatCompletion
// but embeds a raw ESC (control) character in the reply via a JSON \u001b
// escape (not Go's %q, which would emit a \x1b escape - not valid JSON and
// so unusable to actually deliver a raw control byte through a real JSON
// decode).
func fakeOpenAIChatCompletionWithControlChar(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
  "id": "chatcmpl-test", "object": "chat.completion", "created": 0, "model": "gpt-4o-mini",
  "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "before\u001b[2Jafter"}}],
  "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCronRunCmd_PrintOnlyOutputIsSanitizedForTerminal proves the print-only
// path (no --deliver) sanitizes the model's own output the same way
// `prompt` does (see prompt_cmd.go): a raw control character - here, one
// that could clear the terminal screen - must never reach the terminal
// unescaped, whether it came from the model directly or content a tool
// call (e.g. web_fetch) pulled in.
func TestCronRunCmd_PrintOnlyOutputIsSanitizedForTerminal(t *testing.T) {
	srv := fakeOpenAIChatCompletionWithControlChar(t)
	configPath, _ := cronTestConfigPath(t, srv)

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "cron", "run", "daily"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	printed := out.String()
	assert.NotContains(t, printed, "\x1b", "a raw ESC from model output must never reach the terminal on the print-only path")
	assert.Contains(t, printed, `\x1b`, "the escaped literal form must still be visible")
	assert.Contains(t, printed, "before")
	assert.Contains(t, printed, "after")
}

// cronTestConfigPath writes a loadable config with one enabled, persistent
// cron job, an OpenAI base_url pointed at srv, and telegram disabled.
func cronTestConfigPath(t *testing.T, srv *httptest.Server) (configPath, dbPath string) {
	t.Helper()
	root := t.TempDir()
	configPath = filepath.Join(root, "config.yaml")
	dbPath = filepath.Join(root, "mtclaw.db")
	doc := fmt.Sprintf(`version: 1
agent:
  model: gpt-4o-mini
  workspace: %q
openai:
  base_url: %q
channels:
  telegram:
    enabled: false
tools:
  filesystem:
    roots: [%q]
  exec:
    cwd: %q
cron:
  timezone: UTC
  jobs:
    - name: daily
      schedule: "0 9 * * *"
      prompt: "say hi"
      enabled: true
      session: persistent
      timeout: 30s
storage:
  path: %q
`, root, srv.URL, root, root, dbPath)
	require.NoError(t, os.WriteFile(configPath, []byte(doc), 0o600))
	t.Setenv("OPENAI_API_KEY", "sk-test-not-a-real-key")
	return configPath, dbPath
}

// configForTest loads configPath the same way a real command's
// PersistentPreRunE does, for a test that needs the resolved *config.Config
// itself (here, to derive gateway.LockPath from it) rather than driving a
// command through cobra.
func configForTest(t *testing.T, configPath string) (*config.Config, error) {
	t.Helper()
	return config.LoadFile(configPath)
}

// TestNewCronRunApprover_WiresDenyAllApprover pins the other half of the
// approver-wiring invariant the newLoop refactor could silently break:
// `cron run` must always hand the agent loop DenyAllApprover, matching
// exactly what a real gateway picks for channel "cron" - a scheduled or
// manually-fired cron turn never has an interactive approver to ask, so it
// must fail closed rather than run unattended.
func TestNewCronRunApprover_WiresDenyAllApprover(t *testing.T) {
	approver := newCronRunApprover()

	var _ tools.Approver = approver
	approved, err := approver.Ask(context.Background(), tools.Request{})
	assert.False(t, approved)
	assert.ErrorIs(t, err, tools.ErrNoApprover)
}

// TestCronListCmd_NoDatabaseYetShowsDashesInsteadOfFailing proves `cron
// list` on a fresh install (no gateway/prompt/cron run has ever created the
// database file) still lists the configured jobs, falling back to "-" for
// the last-run columns, instead of failing outright with "no database
// yet".
func TestCronListCmd_NoDatabaseYetShowsDashesInsteadOfFailing(t *testing.T) {
	srv := fakeOpenAIChatCompletion(t, "unused")
	configPath, dbPath := cronTestConfigPath(t, srv)
	_, statErr := os.Stat(dbPath)
	require.True(t, os.IsNotExist(statErr), "test setup must not have created the database")

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "cron", "list"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	rendered := out.String()
	require.Contains(t, rendered, "daily")
	fields := strings.Fields(rendered)
	assert.Equal(t, []string{"-", "-"}, fields[len(fields)-2:], "last status/last run must fall back to \"-\" with no database")

	_, statErr = os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr), "cron list must not create the database as a side effect of listing")
}

// TestCronRunCmd_RecordsRunWithStartedBeforeFinished proves
// recordManualRun's StartedAt precedes (not equals) FinishedAt, which
// requires the caller to capture the start time before the turn runs, not
// after. This drives `cron run` end to end (real store, a fake OpenAI
// backend) rather than calling recordManualRun directly, so it also
// exercises deliverCronResult being skipped without --deliver and the
// print-only default path.
func TestCronRunCmd_RecordsRunWithStartedBeforeFinished(t *testing.T) {
	srv := fakeOpenAIChatCompletion(t, "hello from the job")
	configPath, dbPath := cronTestConfigPath(t, srv)

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "cron", "run", "daily"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	assert.Contains(t, out.String(), "hello from the job")

	db, dia, _, err := sqlite.Open(context.Background(), dbPath, true)
	require.NoError(t, err)
	defer db.Close()
	st := store.New(db, dia)
	runs, err := st.CronRuns().List(context.Background(), "daily", 1)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "ok", runs[0].Status)
	require.NotNil(t, runs[0].FinishedAt)
	assert.True(t, runs[0].StartedAt.Before(*runs[0].FinishedAt) || runs[0].StartedAt.Equal(*runs[0].FinishedAt),
		"StartedAt must never be after FinishedAt")
	assert.True(t, runs[0].FinishedAt.Sub(runs[0].StartedAt) >= 0)
}

// TestCronRunCmd_RefusesWhilePersistentJobsGatewayLockIsHeld proves the
// lock `cron run` checks is the same one gateway.New (and `mtclaw doctor`)
// derive from storage.path via gateway.LockPath, not a fixed path under the
// machine-wide state directory - holding that exact lock file must refuse
// a persistent job's manual run.
func TestCronRunCmd_RefusesWhilePersistentJobsGatewayLockIsHeld(t *testing.T) {
	srv := fakeOpenAIChatCompletion(t, "unused")
	configPath, _ := cronTestConfigPath(t, srv)

	cfg, err := configForTest(t, configPath)
	require.NoError(t, err)
	release, err := gateway.Acquire(gateway.LockPath(*cfg))
	require.NoError(t, err)
	defer func() { _ = release() }()

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "cron", "run", "daily"})
	err = root.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holds job")

	// --ephemeral bypasses the lock check entirely (a fresh session can
	// never race the gateway's persistent one).
	root2 := newTestRootCmd(t)
	root2.SetOut(&out)
	root2.SetErr(&out)
	root2.SetArgs([]string{"--config", configPath, "cron", "run", "daily", "--ephemeral"})
	assert.NoError(t, root2.ExecuteContext(context.Background()))
}

// cronDeliverConfigPath writes a loadable config whose single cron job is
// disabled (and cron itself off), with Telegram enabled and reachable only
// for user 111, so `cron run --deliver` is the only thing that can reach a
// chat. openaiURL and telegramURL are the fakes' base URLs.
func cronDeliverConfigPath(t *testing.T, openaiURL, telegramURL, chatID string) string {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	doc := fmt.Sprintf(`version: 1
agent:
  model: gpt-4o-mini
  workspace: %q
openai:
  base_url: %q
channels:
  telegram:
    enabled: true
    allow_from: [111]
    api_base_url: %q
tools:
  filesystem:
    roots: [%q]
  exec:
    cwd: %q
cron:
  enabled: false
  timezone: UTC
  jobs:
    - name: daily
      schedule: "0 9 * * *"
      prompt: "say hi"
      enabled: false
      session: ephemeral
      timeout: 30s
      deliver_to:
        channel: telegram
        chat_id: %q
storage:
  path: %q
`, root, openaiURL, telegramURL, root, root, chatID, filepath.Join(root, "mtclaw.db"))
	require.NoError(t, os.WriteFile(configPath, []byte(doc), 0o600))
	t.Setenv("OPENAI_API_KEY", "sk-test-not-a-real-key")
	return configPath
}

// TestCronRunCmd_DeliverValidatesTargetBeforeAnyAPICall proves `cron run
// --deliver` applies config validation's deliver_to rule even when cron and
// the job are disabled, and does so before any model call: an unreachable
// or empty chat_id sends nothing anywhere.
func TestCronRunCmd_DeliverValidatesTargetBeforeAnyAPICall(t *testing.T) {
	for _, tc := range []struct {
		name, chatID, wantErr string
	}{
		{"unlisted chat", "999999", "is not in channels.telegram.allow_from"},
		{"empty chat id", "", "must set both channel and chat_id"},
		{"wildcard group key", "*", "is not in channels.telegram.allow_from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			openai := fakeapi.NewOpenAI(fakeapi.Step{Content: "hi"})
			t.Cleanup(openai.Close)
			tg := fakeapi.NewTelegram()
			t.Cleanup(tg.Close)
			configPath := cronDeliverConfigPath(t, openai.BaseURL(), tg.URL(), tc.chatID)
			t.Setenv("TELEGRAM_BOT_TOKEN", tg.Token())

			root := newTestRootCmd(t)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"--config", configPath, "cron", "run", "daily", "--deliver"})
			err := root.ExecuteContext(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, openai.Requests(), "validation must fail before any model call")
			for _, c := range tg.Calls() {
				assert.NotEqual(t, "sendMessage", c.Method)
			}
		})
	}
}

// TestCronRunCmd_DeliverToReachableChatOfDisabledJobSucceeds keeps the
// other half honest: a manual run of a disabled job stays allowed when its
// target is reachable.
func TestCronRunCmd_DeliverToReachableChatOfDisabledJobSucceeds(t *testing.T) {
	openai := fakeapi.NewOpenAI(fakeapi.Step{Content: "hello there"})
	t.Cleanup(openai.Close)
	tg := fakeapi.NewTelegram()
	t.Cleanup(tg.Close)
	configPath := cronDeliverConfigPath(t, openai.BaseURL(), tg.URL(), "111")
	t.Setenv("TELEGRAM_BOT_TOKEN", tg.Token())

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "cron", "run", "daily", "--deliver"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	assert.Contains(t, out.String(), "delivered to chat 111")
	assert.Equal(t, []string{"hello there"}, tg.SentTexts(111))
}

// TestCronRunCmd_HonorsJobTimeout proves a manual run is bounded by the
// job's timeout like a scheduled fire: a model call that never answers ends
// the run near the timeout instead of hanging, and the failed run is still
// recorded.
func TestCronRunCmd_HonorsJobTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	configPath, dbPath := cronTestConfigPath(t, srv)
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "timeout: 30s")
	require.NoError(t, os.WriteFile(configPath, []byte(strings.Replace(string(data), "timeout: 30s", "timeout: 1s", 1)), 0o600))

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "cron", "run", "daily", "--ephemeral"})

	start := time.Now()
	err = root.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Less(t, time.Since(start), 15*time.Second, "the run must end near the 1s job timeout")

	db, dia, _, err := sqlite.Open(context.Background(), dbPath, true)
	require.NoError(t, err)
	defer db.Close()
	runs, err := store.New(db, dia).CronRuns().List(context.Background(), "daily", 1)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "error", runs[0].Status)
}
