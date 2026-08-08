package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"

	// Blank import: exactly what a production wiring site must also do
	// for store.Open to resolve "sqlite" - see this package's own
	// dialect.go init(). Without this line, TestOpen_SQLiteDriver would
	// fail with the same "unknown storage driver" error
	// TestOpen_UnregisteredDriverNamesSupportedSet exercises on purpose.
	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
)

func TestOpen_SQLiteDriverReturnsUsableStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "factory-test.db")
	cfg := config.StorageConfig{Driver: "sqlite", DSN: dbPath}

	st, err := store.Open(context.Background(), cfg, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)
}

func TestOpen_SQLiteDriverHonorsPathAliasViaEffectiveDSN(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "factory-test-path-alias.db")
	// EffectiveDSN, not either field directly, is what a caller like
	// store.Open must read - see config.StorageConfig's own doc comment.
	cfg := config.StorageConfig{Driver: "sqlite", Path: dbPath}

	st, err := store.Open(context.Background(), cfg, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	_, err = st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)
}

// TestOpen_UnregisteredDriverNamesSupportedSet is the mitigation R8 (and
// plan.md's A6) call for: the registry pattern trades a compile-time
// import-cycle for a runtime "forgot the blank import" failure mode, and
// the only guardrail is an error message that names the fix. This also
// covers "Driver: postgres" from the phase's own success criteria - this
// binary registers no second backend, so any unregistered name exercises
// the identical path.
func TestOpen_UnregisteredDriverNamesSupportedSet(t *testing.T) {
	cfg := config.StorageConfig{Driver: "postgres", DSN: "irrelevant"}

	_, err := store.Open(context.Background(), cfg, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrUnknownDriver)
	assert.Contains(t, err.Error(), `"postgres"`)
	assert.Contains(t, err.Error(), "sqlite")
}

func TestRegister_DuplicateDriverNamePanics(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r, "Register must panic on a duplicate driver name, like sql.Register does")
		assert.Contains(t, r, "sqlite")
	}()
	// "sqlite" is already registered by this package's own blank import
	// above (internal/store/sqlite's init()), so this is guaranteed to be
	// a duplicate without depending on test execution order.
	store.Register("sqlite", nil)
}
