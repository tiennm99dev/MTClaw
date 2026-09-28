package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// TestApprovalsListCmd_SanitizesEmbeddedControlCharsAndNewlines proves an
// exec_audit row's Command, which can carry whatever a model asked to run
// (redacted, but not escaped for terminal display), never reaches the
// operator raw: embedded control bytes must not print unescaped, and an
// embedded real newline/tab must not splice extra rows/columns into the
// table.
func TestApprovalsListCmd_SanitizesEmbeddedControlCharsAndNewlines(t *testing.T) {
	configPath, dbPath := sessionsTestConfigPath(t)

	db, dia, _, err := sqlite.Open(context.Background(), dbPath, false)
	require.NoError(t, err)
	st := store.New(db, dia)
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)
	require.NoError(t, st.Audit().Append(context.Background(), &store.ExecAudit{
		SessionID: sess.ID,
		Command:   "echo hi\nrm -rf /\x1b[2J",
		Decision:  "denied_rule",
	}))
	require.NoError(t, st.Close())

	root := newRootCmd(&state{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "approvals", "list"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	rendered := out.String()
	assert.NotContains(t, rendered, "\x1b")
	assert.Contains(t, rendered, `\x1b`)
	assert.Contains(t, rendered, `\n`, "an embedded real newline must render as the literal two-character escape, not a table row break")
	assert.Equal(t, 2, strings.Count(rendered, "\n"), "exactly one header line and one data line - an embedded newline must not add a third")
}

func TestDecider_RefusedTooLongMapsToPolicy(t *testing.T) {
	assert.Equal(t, "policy", decider("refused_too_long"))
}
