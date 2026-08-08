package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/provider/mock"
	"github.com/tiennm99/MTClaw/internal/store"

	// Blank import: registers "sqlite" with store.Open's driver registry.
	// Nothing else in this package's own dependency graph imports the
	// sqlite backend package, so this test file is the one place that
	// has to.
	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// newTestStore opens a fresh sqlite-backed store.Store at a temp path, the
// same real implementation production code uses, so these tests exercise
// the actual Append atomicity the loop's turn buffering depends on.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent-test.db")
	st, err := store.Open(ctx, config.StorageConfig{Driver: "sqlite", DSN: dbPath}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestSession(t *testing.T, st store.Store) string {
	t.Helper()
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)
	return sess.ID
}

// fakeToolRunner is a minimal, test-only ToolRunner: fn decides the result
// (or error) per call, and every invocation is recorded for assertions.
type fakeToolRunner struct {
	specs []provider.ToolSpec
	fn    func(ctx context.Context, call provider.ToolCall, meta Meta) (string, error)
	calls []provider.ToolCall
}

func (f *fakeToolRunner) Specs() []provider.ToolSpec { return f.specs }

func (f *fakeToolRunner) Run(ctx context.Context, call provider.ToolCall, meta Meta) (string, error) {
	f.calls = append(f.calls, call)
	return f.fn(ctx, call, meta)
}

func TestRun_SingleShot_NoToolCalls(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)
	prov := mock.New(mock.Step{Content: "hello back", Usage: provider.Usage{Prompt: 10, Completion: 3}})
	tools := &fakeToolRunner{}

	loop := New(testAgentConfig(), prov, st, tools, nil)
	result := loop.Run(context.Background(), sessionID, "hello", "", nil)

	require.NoError(t, result.Err)
	assert.Equal(t, "hello back", result.Text)
	assert.False(t, result.NoReply)
	assert.Equal(t, 1, result.Iterations)
	assert.Equal(t, 10, result.Usage.Prompt)
	assert.Equal(t, 3, result.Usage.Completion)

	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "hello back", msgs[1].Content)

	sess, err := st.Sessions().Get(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, 10, sess.PromptTokens)
	assert.Equal(t, 3, sess.CompletionTokens)
}

func TestRun_ToolPath_TwoCallsProduceTwoMatchingToolMessages(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	calls := []provider.ToolCall{
		{ID: "call_1", Name: "get_weather"},
		{ID: "call_2", Name: "get_time"},
	}
	prov := mock.New(
		mock.Step{ToolCalls: calls},
		mock.Step{Content: "here you go", Usage: provider.Usage{Prompt: 5, Completion: 2}},
	)
	tools := &fakeToolRunner{
		fn: func(ctx context.Context, call provider.ToolCall, meta Meta) (string, error) {
			assert.Equal(t, sessionID, meta.SessionID)
			assert.Equal(t, "cli", meta.Channel)
			assert.Equal(t, "local", meta.ChatID)
			return "result for " + call.ID, nil
		},
	}

	loop := New(testAgentConfig(), prov, st, tools, nil)
	result := loop.Run(context.Background(), sessionID, "what's up", "", nil)

	require.NoError(t, result.Err)
	assert.Equal(t, "here you go", result.Text)
	assert.Equal(t, 2, result.Iterations)
	require.Len(t, tools.calls, 2)

	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 5) // user, assistant(tool_calls), tool, tool, assistant(final)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.NotEmpty(t, msgs[1].ToolCalls)
	assert.Equal(t, "tool", msgs[2].Role)
	assert.Equal(t, "call_1", msgs[2].ToolCallID)
	assert.Equal(t, "result for call_1", msgs[2].Content)
	assert.Equal(t, "get_weather", msgs[2].ToolName)
	assert.Equal(t, "tool", msgs[3].Role)
	assert.Equal(t, "call_2", msgs[3].ToolCallID)
	assert.Equal(t, "result for call_2", msgs[3].Content)
	assert.Equal(t, "assistant", msgs[4].Role)
	assert.Equal(t, "here you go", msgs[4].Content)

	// The second request must carry both tool results.
	requests := prov.Requests()
	require.Len(t, requests, 2)
	last := requests[1].Messages
	require.Len(t, last, 5) // system + user + assistant(tool_calls) + tool + tool
	assert.Equal(t, provider.RoleTool, last[3].Role)
	assert.Equal(t, provider.RoleTool, last[4].Role)
	assert.Equal(t, "result for call_1", last[3].Content)
	assert.Equal(t, "result for call_2", last[4].Content)
}

func TestRun_ToolErrorString_KeepsLoopAlive(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	prov := mock.New(
		mock.Step{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "exec"}}},
		mock.Step{Content: "I could not run that command."},
	)
	tools := &fakeToolRunner{
		fn: func(ctx context.Context, call provider.ToolCall, meta Meta) (string, error) {
			return "error: permission denied", nil
		},
	}

	loop := New(testAgentConfig(), prov, st, tools, nil)
	result := loop.Run(context.Background(), sessionID, "delete everything", "", nil)

	require.NoError(t, result.Err)
	assert.Equal(t, "I could not run that command.", result.Text)
	assert.Equal(t, 2, result.Iterations)

	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 4)
	assert.Equal(t, "error: permission denied", msgs[2].Content)
}

func TestRun_ToolGoError_AbortsTurn(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	prov := mock.New(mock.Step{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "exec"}}})
	boom := errors.New("registry crashed")
	tools := &fakeToolRunner{
		fn: func(ctx context.Context, call provider.ToolCall, meta Meta) (string, error) {
			return "", boom
		},
	}

	loop := New(testAgentConfig(), prov, st, tools, nil)
	result := loop.Run(context.Background(), sessionID, "run something", "", nil)

	require.Error(t, result.Err)
	assert.ErrorIs(t, result.Err, boom)

	// The turn is still internally consistent: exactly one tool row for
	// the one tool_calls entry, even though the tool itself errored.
	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "tool", msgs[2].Role)
	assert.Equal(t, "call_1", msgs[2].ToolCallID)
	assert.Contains(t, msgs[2].Content, "registry crashed")
}

// TestRun_ToolCancelMidBatch_BackfillsUnrunCalls is the H1 regression test:
// a two-tool-call batch where the first call aborts the turn (context.
// Canceled, the shape a /stop or cron timeout produces) must not leave the
// second call's id unanswered in the persisted transcript. Feeding that
// transcript back through HardTrim must find no orphaned tool_calls id
// either - the exact failure mode that poisons every later request in the
// session.
func TestRun_ToolCancelMidBatch_BackfillsUnrunCalls(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	calls := []provider.ToolCall{
		{ID: "call_1", Name: "slow_tool"},
		{ID: "call_2", Name: "slow_tool"},
	}
	prov := mock.New(mock.Step{ToolCalls: calls})
	tools := &fakeToolRunner{
		fn: func(ctx context.Context, call provider.ToolCall, meta Meta) (string, error) {
			if call.ID == "call_1" {
				return "", context.Canceled
			}
			t.Fatalf("call %q must never run: the turn already aborted on call_1", call.ID)
			return "", nil
		},
	}

	loop := New(testAgentConfig(), prov, st, tools, nil)
	result := loop.Run(context.Background(), sessionID, "run two slow things", "", nil)

	require.Error(t, result.Err)
	var perr *provider.Error
	require.ErrorAs(t, result.Err, &perr)
	assert.Equal(t, provider.ErrCanceled, perr.Kind)

	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 4, "user, assistant(tool_calls x2), and a tool row for BOTH ids - the second backfilled")
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "tool", msgs[2].Role)
	assert.Equal(t, "call_1", msgs[2].ToolCallID)
	assert.Equal(t, "tool", msgs[3].Role)
	assert.Equal(t, "call_2", msgs[3].ToolCallID)
	assert.Contains(t, msgs[3].Content, "aborted")

	provMsgs := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		pm, convErr := m.ToProviderMessage()
		require.NoError(t, convErr)
		provMsgs[i] = pm
	}
	assertNoOrphans(t, HardTrim(provMsgs, 10))
}

func TestRun_IterationCap_EnforcedAndExplained(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	cfg := testAgentConfig()
	cfg.Agent.MaxIterations = 3

	steps := make([]mock.Step, cfg.Agent.MaxIterations)
	for i := range steps {
		steps[i] = mock.Step{ToolCalls: []provider.ToolCall{{ID: fmt.Sprintf("call_%d", i), Name: "loop_tool"}}}
	}
	prov := mock.New(steps...)
	tools := &fakeToolRunner{
		fn: func(ctx context.Context, call provider.ToolCall, meta Meta) (string, error) {
			return "ok", nil
		},
	}

	loop := New(cfg, prov, st, tools, nil)
	result := loop.Run(context.Background(), sessionID, "keep going forever", "", nil)

	require.NoError(t, result.Err)
	assert.Equal(t, cfg.Agent.MaxIterations, result.Iterations)
	assert.Len(t, prov.Requests(), cfg.Agent.MaxIterations)
	assert.Contains(t, result.Text, fmt.Sprintf("%d iterations", cfg.Agent.MaxIterations))
}

func TestRun_ErrContextLength_RetriesOnceWithSmallerRequest(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	// Seed 6 prior turns so the hard trim to 4 turns on retry measurably
	// shrinks the request.
	for i := 0; i < 6; i++ {
		require.NoError(t, st.Messages().Append(context.Background(), sessionID, []store.Message{
			{Role: "user", Content: fmt.Sprintf("old question %d", i)},
			{Role: "assistant", Content: fmt.Sprintf("old answer %d", i)},
		}))
	}

	prov := mock.New(
		mock.Step{Err: &provider.Error{Kind: provider.ErrContextLength}},
		mock.Step{Content: "ok now"},
	)
	tools := &fakeToolRunner{}

	loop := New(testAgentConfig(), prov, st, tools, nil)
	result := loop.Run(context.Background(), sessionID, "one more question", "", nil)

	require.NoError(t, result.Err)
	assert.Equal(t, "ok now", result.Text)

	requests := prov.Requests()
	require.Len(t, requests, 2, "exactly one retry")
	assert.Less(t, len(requests[1].Messages), len(requests[0].Messages), "the retried request must be measurably smaller")
}

func TestRun_ErrContextLength_SecondFailureReturnsError(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	prov := mock.New(
		mock.Step{Err: &provider.Error{Kind: provider.ErrContextLength}},
		mock.Step{Err: &provider.Error{Kind: provider.ErrContextLength}},
	)
	loop := New(testAgentConfig(), prov, st, &fakeToolRunner{}, nil)
	result := loop.Run(context.Background(), sessionID, "hello", "", nil)

	require.Error(t, result.Err)
	assert.Len(t, prov.Requests(), 2, "only one retry is attempted")

	// Only the user message is persisted on a non-recovered provider error.
	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0].Role)
}

func TestRun_OtherProviderError_FlushesOnlyUserMessage(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	prov := mock.New(mock.Step{Err: &provider.Error{Kind: provider.ErrAuth, Msg: "bad key"}})
	loop := New(testAgentConfig(), prov, st, &fakeToolRunner{}, nil)
	result := loop.Run(context.Background(), sessionID, "hello", "", nil)

	require.Error(t, result.Err)
	var perr *provider.Error
	require.ErrorAs(t, result.Err, &perr)
	assert.Equal(t, provider.ErrAuth, perr.Kind)

	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0].Role)
}

func TestRun_CancelMidTurn_PersistsPartialTurn(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	prov := mock.New(mock.Step{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "slow_tool"}}})
	tools := &fakeToolRunner{
		fn: func(ctx context.Context, call provider.ToolCall, meta Meta) (string, error) {
			// Simulate the caller giving up partway through this turn: the
			// tool itself still finishes normally, but the loop must
			// notice cancellation before starting another Think call.
			cancel()
			return "partial result", nil
		},
	}

	loop := New(testAgentConfig(), prov, st, tools, nil)
	result := loop.Run(ctx, sessionID, "do a slow thing", "", nil)

	require.Error(t, result.Err)
	var perr *provider.Error
	require.ErrorAs(t, result.Err, &perr)
	assert.Equal(t, provider.ErrCanceled, perr.Kind)

	// The partial turn - user, assistant tool_calls, and the one tool
	// result the loop had already paired - is still recorded whole.
	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "tool", msgs[2].Role)
	assert.Equal(t, "call_1", msgs[2].ToolCallID)
}

func TestRun_NoReply_SetsFlagAndStillPersists(t *testing.T) {
	st := newTestStore(t)
	sessionID := newTestSession(t, st)

	prov := mock.New(mock.Step{Content: "  NO_REPLY  "})
	loop := New(testAgentConfig(), prov, st, &fakeToolRunner{}, nil)
	result := loop.Run(context.Background(), sessionID, "just testing", "", nil)

	require.NoError(t, result.Err)
	assert.True(t, result.NoReply)

	msgs, err := st.Messages().Recent(context.Background(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "  NO_REPLY  ", msgs[1].Content, "the transcript stays honest about exactly what the model produced")
}

func TestRun_UnknownSession_ReturnsError(t *testing.T) {
	st := newTestStore(t)
	loop := New(testAgentConfig(), mock.New(), st, &fakeToolRunner{}, nil)
	result := loop.Run(context.Background(), "does-not-exist", "hi", "", nil)
	require.Error(t, result.Err)
}
