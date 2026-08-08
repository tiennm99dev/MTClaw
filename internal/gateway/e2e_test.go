// End-to-end coverage: the real Gateway (real telego client, real
// openai-go client, real sqlite store) driven against fakeapi's httptest
// fakes instead of a live Telegram bot or a live OpenAI key. This is
// `package gateway` (not `gateway_test`) specifically to reach gw.disp and
// gw.cronSched, which the plan's cron test needs and no public seam exists
// for - see phase-01-e2e-verification-harness.md's Files table.
package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	yaml "github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/cron"
	"github.com/tiennm99/MTClaw/internal/testsupport"
	"github.com/tiennm99/MTClaw/internal/testsupport/fakeapi"
	// This package's own gateway.go already carries the blank import
	// that registers "sqlite" with store.Open's driver registry - which
	// transitively pulls in the pure-Go SQLite driver, so its
	// database/sql driver name "sqlite" is available process-wide for
	// waitForPendingApproval's direct read below, with no direct import
	// of the sqlite backend package needed in this file.
)

// e2eOpts customizes the config newE2EGateway renders beyond the shared
// baseline (fake HOME, a fake Telegram + the caller's fake OpenAI, one temp
// workspace and sqlite file). Its zero value is a plain DM-capable gateway
// with tools.exec disabled and cron disabled.
type e2eOpts struct {
	allowFrom   []int64
	execMode    string // "" disables tools.exec entirely (Mode ends up "off")
	cronEnabled bool
	cronJobs    []config.CronJob
}

// e2eHarness bundles one test's fully-wired real Gateway plus the fakes it
// talks to and the on-disk paths a test needs to reach past store.Store's
// interfaces (the approvals nonce lookup has no interface method for it;
// see waitForPendingApproval).
type e2eHarness struct {
	tg        *fakeapi.Telegram
	oa        *fakeapi.OpenAI
	gw        *Gateway
	cfg       *config.Config
	workspace string
	dbPath    string
}

// newE2EGateway builds one real Gateway (gateway.New; it does not call Run)
// against a fresh fake HOME, a fresh fakeapi.Telegram, and oa (the caller's
// already-scripted fakeapi.OpenAI). Every call gets its own fake HOME, so
// two calls in the same test never contend for the same instance lock file
// (see internal/gateway/lock.go's Acquire, which gateway.New calls).
func newE2EGateway(t *testing.T, oa *fakeapi.OpenAI, opts e2eOpts) *e2eHarness {
	t.Helper()
	testsupport.FakeHome(t)

	tg := fakeapi.NewTelegram()
	t.Cleanup(tg.Close)
	t.Cleanup(oa.Close)

	cfg, workspace, dbPath := buildE2EConfig(t, tg, oa, opts)

	gw, err := New(*cfg, discardLogger())
	require.NoError(t, err)
	// Idempotent: Run's own defer already closes the store when a test
	// calls Run, so this is only load-bearing for tests (the cron wiring
	// and delivery tests) that build a Gateway without ever calling Run.
	t.Cleanup(func() { _ = gw.store.Close() })

	return &e2eHarness{tg: tg, oa: oa, gw: gw, cfg: cfg, workspace: workspace, dbPath: dbPath}
}

// run starts h.gw.Run(ctx) in a goroutine and returns a channel that
// receives its result exactly once, when Run returns.
func (h *e2eHarness) run(ctx context.Context) <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- h.gw.Run(ctx) }()
	return errCh
}

// buildE2EConfig renders a Config value (agent.model, the fakes' URLs,
// filesystem/exec/cron settings) and loads it through the exact same
// config.Load pipeline a real config file goes through - marshaling the
// struct with the same yaml package internal/config itself uses (rather
// than hand-templating YAML text) so there is no risk of the
// Windows-backslash-in-a-quoted-path escaping problem hand-written YAML
// would have to work around with filepath.ToSlash. Going through Load
// (rather than handing Validate a hand-built *Config directly) is what
// resolves channels.telegram.token_env/openai.api_key_env into the
// unexported secret fields New actually reads - see plan.md finding A4.
func buildE2EConfig(t *testing.T, tg *fakeapi.Telegram, oa *fakeapi.OpenAI, opts e2eOpts) (cfg *config.Config, workspace, dbPath string) {
	t.Helper()

	dir := t.TempDir()
	workspace = filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	dbPath = filepath.Join(dir, "mtclaw.db")

	draft := config.Default()
	draft.Agent.Model = "fake-model"
	draft.Agent.Workspace = workspace
	draft.OpenAI.BaseURL = oa.BaseURL()
	draft.Channels.Telegram.TokenEnv = "E2E_TG_TOKEN"
	draft.Channels.Telegram.APIBaseURL = tg.URL()
	draft.Channels.Telegram.AllowFrom = opts.allowFrom
	draft.Tools.Filesystem.Roots = []string{workspace}
	draft.Tools.Exec = e2eExecConfig(opts.execMode, workspace)
	draft.Storage.Path = dbPath
	draft.Cron.Timezone = "UTC"
	draft.Cron.Enabled = opts.cronEnabled
	draft.Cron.Jobs = opts.cronJobs

	raw, err := yaml.Marshal(draft)
	require.NoError(t, err)

	loaded, err := config.Load(raw, dir, map[string]string{
		"E2E_TG_TOKEN":   tg.Token(),
		"OPENAI_API_KEY": "sk-fake",
	})
	require.NoError(t, err)
	return loaded, workspace, dbPath
}

// e2eExecConfig builds tools.exec: "off" (fully disabled) when mode is "",
// otherwise enabled under mode with an explicit, OS-appropriate shell (see
// testShell in cron_approver_test.go) and no allow/deny patterns, matching
// the approval e2e tests' requirement of an unmatched command reaching the
// approver.
func e2eExecConfig(mode, workspace string) config.ExecConfig {
	if mode == "" {
		return config.ExecConfig{Enabled: false, Mode: "off"}
	}
	return config.ExecConfig{
		Enabled: true,
		Mode:    mode,
		Shell:   testShell(),
		CWD:     workspace,
		Timeout: config.Duration(5 * time.Second),
		// Generous relative to the fake-server polling latency the test
		// itself incurs (learning the approval nonce via a direct sqlite
		// query, then a getUpdates round trip to deliver the callback): a
		// tight timeout here would race the test's own plumbing, not the
		// approval flow under test.
		ApprovalTimeout: config.Duration(30 * time.Second),
		MaxOutputBytes:  65536,
	}
}

// waitForPendingApproval polls dbPath directly for the most recent pending
// approval addressed to chatID, returning its nonce id and the message id
// holding its inline buttons. There is no store.ApprovalStore method for
// "the current pending row for this chat" (by design: it is Approver's own
// bookkeeping, not a query production code needs), so this is the "direct
// query" the phase's own spec calls for - never the inline keyboard, which
// stays test-inspectable separately via extractCallbackData for the
// two-buttons assertion.
func waitForPendingApproval(t *testing.T, dbPath string, chatID int64) (id string, promptMessageID int) {
	t.Helper()
	ctx := context.Background()

	// mode=ro mirrors the sqlite backend package's own read-only DSN: the
	// gateway under test holds its own write connection open
	// concurrently, and WAL mode lets a read-only reader coexist with it
	// without blocking either side.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// database/sql.Open never actually dials; Ping here (mirroring that
	// same package's own eager Ping) so a wrong path fails this test
	// immediately with a clear error, instead of silently retrying
	// inside waitCond below until its own timeout.
	require.NoError(t, db.PingContext(ctx))

	waitCond(t, func() bool {
		var idVal, midVal string
		row := db.QueryRowContext(ctx,
			`SELECT id, message_id FROM approvals WHERE state = 'pending' AND chat_id = ? ORDER BY created_at DESC LIMIT 1`,
			strconv.FormatInt(chatID, 10))
		if err := row.Scan(&idVal, &midVal); err != nil {
			return false
		}
		mid, err := strconv.Atoi(midVal)
		if err != nil || mid == 0 {
			// message_id is set by a second write (Approver.SetMessageID)
			// after the row is created; an empty/zero value means the
			// prompt has not finished sending yet.
			return false
		}
		id, promptMessageID = idVal, mid
		return true
	}, 10*time.Second)

	return id, promptMessageID
}

// extractCallbackData returns every callback_data value found in a
// sendMessage call's reply_markup.inline_keyboard, in row-major order. It
// exists solely to assert the *shape* of the approval prompt (two buttons);
// waitForPendingApproval, not this, is what a test uses to learn which
// nonce to actually push back as a callback.
func extractCallbackData(body map[string]any) []string {
	markup, _ := body["reply_markup"].(map[string]any)
	rows, _ := markup["inline_keyboard"].([]any)
	var out []string
	for _, r := range rows {
		buttons, _ := r.([]any)
		for _, b := range buttons {
			button, _ := b.(map[string]any)
			if data, ok := button["callback_data"].(string); ok {
				out = append(out, data)
			}
		}
	}
	return out
}

// TestE2E_DMRoundTrip_ToolCallFedBackAndChunkAwareReply drives one DM turn
// that calls read_file, asserts the tool's result reached the model's next
// request (proving Observe actually ran, not just Act), and that the final
// text reached the originating chat via sendMessage.
func TestE2E_DMRoundTrip_ToolCallFedBackAndChunkAwareReply(t *testing.T) {
	const userID = int64(1001)
	const chatID = userID // Telegram DMs: the private chat id equals the sender's user id.

	oa := fakeapi.NewOpenAI(
		fakeapi.Step{ToolCalls: []fakeapi.ToolCall{{ID: "call_1", Name: "read_file", Args: `{"path":"notes.txt"}`}}},
		fakeapi.Step{Content: "your notes say: buy oat milk"},
	)
	h := newE2EGateway(t, oa, e2eOpts{allowFrom: []int64{userID}})
	require.NoError(t, os.WriteFile(filepath.Join(h.workspace, "notes.txt"), []byte("buy oat milk"), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	runErr := h.run(ctx)

	h.tg.PushMessage(chatID, userID, "read my notes")

	waitCond(t, func() bool { return len(h.tg.SentTexts(chatID)) > 0 }, 15*time.Second)

	texts := h.tg.SentTexts(chatID)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[len(texts)-1], "buy oat milk", "the final reply must carry the model's answer")

	reqs := oa.Requests()
	require.Len(t, reqs, 2, "the tool call must trigger exactly one follow-up completion request")
	second, err := json.Marshal(reqs[1])
	require.NoError(t, err)
	assert.Contains(t, string(second), "buy oat milk", "the read_file tool's result must be fed back to the model in the follow-up request")

	select {
	case err := <-runErr:
		t.Fatalf("gw.Run exited unexpectedly: %v", err)
	default:
	}
}

// TestE2E_ChunkedReply_SplitsAcrossMultipleSendMessageCalls asserts a reply
// longer than telegram.DefaultChunkLimit arrives as more than one
// sendMessage call, all addressed to the same chat, in order.
func TestE2E_ChunkedReply_SplitsAcrossMultipleSendMessageCalls(t *testing.T) {
	const userID = int64(1002)
	const chatID = userID

	longText := longRepeat("Reporting the same finding again. ", 200) // well over 4096 bytes
	oa := fakeapi.NewOpenAI(fakeapi.Step{Content: longText})
	h := newE2EGateway(t, oa, e2eOpts{allowFrom: []int64{userID}})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h.run(ctx)

	h.tg.PushMessage(chatID, userID, "give me the long report")

	waitCond(t, func() bool { return len(h.tg.SentTexts(chatID)) >= 2 }, 15*time.Second)

	texts := h.tg.SentTexts(chatID)
	assert.GreaterOrEqual(t, len(texts), 2, "a reply over the chunk limit must arrive as more than one sendMessage call")
	for _, c := range h.tg.Calls() {
		if c.Method == "sendMessage" {
			assert.EqualValues(t, chatID, fakeapi.ChatIDFromBody(c.Body), "every chunk must be addressed to the originating chat")
		}
	}
}

func longRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// TestE2E_ApprovalApprove_RunsCommandAndAudits drives an exec tool call in
// approval mode through a real approval round trip: the prompt carries two
// inline buttons, an Approve callback runs the command, and both the
// callback and the audit trail reflect it.
func TestE2E_ApprovalApprove_RunsCommandAndAudits(t *testing.T) {
	const userID = int64(1003)
	const chatID = userID

	oa := fakeapi.NewOpenAI(
		fakeapi.Step{ToolCalls: []fakeapi.ToolCall{{ID: "call_1", Name: "exec", Args: `{"command":"echo mtclaw-e2e"}`}}},
		fakeapi.Step{Content: "command output: mtclaw-e2e"},
	)
	h := newE2EGateway(t, oa, e2eOpts{allowFrom: []int64{userID}, execMode: "approval"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h.run(ctx)

	h.tg.PushMessage(chatID, userID, "run echo mtclaw-e2e")

	id, promptMessageID := waitForPendingApproval(t, h.dbPath, chatID)

	var promptCall fakeapi.Call
	found := false
	for _, c := range h.tg.Calls() {
		if c.Method != "sendMessage" {
			continue
		}
		if data := extractCallbackData(c.Body); len(data) == 2 {
			promptCall, found = c, true
		}
	}
	require.True(t, found, "the approval prompt must carry reply_markup with two callback_data buttons")
	buttons := extractCallbackData(promptCall.Body)
	assert.Contains(t, buttons, "ok:"+id)
	assert.Contains(t, buttons, "no:"+id)

	h.tg.PushCallback(chatID, userID, "ok:"+id, promptMessageID)

	waitCond(t, func() bool {
		for _, c := range h.tg.Calls() {
			if c.Method == "answerCallbackQuery" {
				return true
			}
		}
		return false
	}, 10*time.Second)

	waitCond(t, func() bool {
		texts := h.tg.SentTexts(chatID)
		return len(texts) > 0 && strings.Contains(texts[len(texts)-1], "mtclaw-e2e")
	}, 10*time.Second)

	waitCond(t, func() bool {
		rows, err := h.gw.store.Audit().List(context.Background(), 0)
		require.NoError(t, err)
		return len(rows) == 1 && rows[0].Decision == "approved"
	}, 10*time.Second)
}

// TestE2E_ApprovalDeny_RefusalReachesModelAndAudits pushes "no:<id>" and
// asserts the refusal reaches the model as a tool result (not a Go error -
// the turn must still complete) and exec_audit records denied_user.
func TestE2E_ApprovalDeny_RefusalReachesModelAndAudits(t *testing.T) {
	const userID = int64(1004)
	const chatID = userID

	oa := fakeapi.NewOpenAI(
		fakeapi.Step{ToolCalls: []fakeapi.ToolCall{{ID: "call_1", Name: "exec", Args: `{"command":"echo mtclaw-e2e"}`}}},
		fakeapi.Step{Content: "sorry, that command was refused"},
	)
	h := newE2EGateway(t, oa, e2eOpts{allowFrom: []int64{userID}, execMode: "approval"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	runErr := h.run(ctx)

	h.tg.PushMessage(chatID, userID, "run echo mtclaw-e2e")

	id, promptMessageID := waitForPendingApproval(t, h.dbPath, chatID)
	h.tg.PushCallback(chatID, userID, "no:"+id, promptMessageID)

	waitCond(t, func() bool { return len(h.tg.SentTexts(chatID)) > 0 }, 15*time.Second)

	waitCond(t, func() bool { return len(oa.Requests()) >= 2 }, 10*time.Second)
	second, err := json.Marshal(oa.Requests()[1])
	require.NoError(t, err)
	assert.Contains(t, string(second), "denied by the approver",
		"the denial must reach the model as a tool message, not just abort the turn")

	waitCond(t, func() bool {
		rows, err := h.gw.store.Audit().List(context.Background(), 0)
		require.NoError(t, err)
		return len(rows) == 1 && rows[0].Decision == "denied_user"
	}, 10*time.Second)

	select {
	case err := <-runErr:
		t.Fatalf("gw.Run exited unexpectedly: %v", err)
	default:
	}
}

// fakeNowAtNextMinuteIn returns a now() func for cron.New whose apparent
// time tracks real elapsed time from an anchor placed delay before the next
// true minute boundary, so alignDelay (cron/scheduler.go) computes
// approximately delay and, once that real wait elapses, a second call to
// the returned func reads a time whose Second() is 0.
//
// A literally frozen clock (one func value returning the same instant on
// every call, as an early draft of this test used) does not work: gronx
// silently prepends a literal "0" seconds segment to a 5-field expression
// (gronx@v1.20.0's Segments), so "* * * * *" is really "0 * * * * *" and
// IsDue requires Second() == 0 at the instant it is asked about, not merely
// "the same minute". Scheduler.Start calls now() twice - once to compute
// alignDelay, once again inside tick() after that delay elapses - and a
// frozen clock answers both calls with the pre-boundary second (:59.9),
// which IsDue correctly refuses forever. Production code never hits this
// because its now is the real clock, which genuinely advances between
// those two calls; this helper's whole job is reproducing that same
// real-time advancement under a virtual anchor instead of the real
// wall-clock instant, which is what actually lets the fake minute boundary
// arrive in ~100ms instead of up to 60s.
func fakeNowAtNextMinuteIn(delay time.Duration, loc *time.Location) func() time.Time {
	anchor := time.Now().In(loc).Truncate(time.Minute).Add(time.Minute).Add(-delay)
	start := time.Now()
	return func() time.Time { return anchor.Add(time.Since(start)) }
}

// TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun drives a real
// cron.Scheduler (fake clock, so the real minute-aligned wait - up to 60s -
// is never exercised here; phase 4's manual checklist covers that) wired to
// this Gateway's own real dispatcher and store, and asserts delivery lands
// on deliver_to.chat_id, never the unrelated chat, and that cron_runs
// reaches status "ok". The scheduler is built directly in the test (not
// gw.cronSched, and gw.Run is deliberately never called) precisely so a
// fake now() can be injected - see TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled
// for the separate assertion that gateway.New's own wiring (which this
// bypasses) is still correct.
func TestE2E_CronDelivery_RoutesToDeliverToChatAndRecordsRun(t *testing.T) {
	const unrelatedChatID = int64(1005)
	const deliverChatID = int64(2005)
	const jobName = "e2e-cron-job"

	oa := fakeapi.NewOpenAI(fakeapi.Step{Content: "scheduled turn done"})
	job := config.CronJob{
		Name:      jobName,
		Schedule:  "* * * * *",
		Prompt:    "run the scheduled thing",
		Enabled:   true,
		Session:   "persistent",
		Timeout:   config.Duration(10 * time.Second),
		DeliverTo: config.CronDeliverTo{Channel: "telegram", ChatID: strconv.FormatInt(deliverChatID, 10)},
	}
	h := newE2EGateway(t, oa, e2eOpts{
		allowFrom: []int64{unrelatedChatID, deliverChatID},
		cronJobs:  []config.CronJob{job},
	})

	loc := time.UTC
	sched := cron.New(cron.JobsFromConfig(h.cfg.Cron), loc, h.gw.store.CronRuns(), h.gw.store.Sessions(),
		h.gw.disp.dispatch, discardLogger(), fakeNowAtNextMinuteIn(100*time.Millisecond, loc))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go sched.Start(ctx)

	waitCond(t, func() bool { return len(h.tg.SentTexts(deliverChatID)) > 0 }, 8*time.Second)
	assert.Empty(t, h.tg.SentTexts(unrelatedChatID), "delivery must never land on a chat other than deliver_to.chat_id")

	waitCond(t, func() bool {
		runs, err := h.gw.store.CronRuns().List(context.Background(), jobName, 1)
		require.NoError(t, err)
		return len(runs) == 1 && runs[0].Status == "ok" && runs[0].FinishedAt != nil
	}, 8*time.Second)
}

// TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled closes the gap the
// fake-clock delivery test above bypasses: gateway.New itself (gateway.go's
// startup order) must construct a Scheduler exactly when cron.enabled is
// true, and leave gw.cronSched nil when it is false.
func TestE2E_GatewayNew_BuildsCronSchedulerOnlyWhenEnabled(t *testing.T) {
	enabled := newE2EGateway(t, fakeapi.NewOpenAI(), e2eOpts{allowFrom: []int64{1006}, cronEnabled: true})
	assert.NotNil(t, enabled.gw.cronSched, "gateway.New must build a scheduler when cron.enabled is true")

	disabled := newE2EGateway(t, fakeapi.NewOpenAI(), e2eOpts{allowFrom: []int64{1006}, cronEnabled: false})
	assert.Nil(t, disabled.gw.cronSched, "gateway.New must leave cronSched nil when cron.enabled is false")
}
