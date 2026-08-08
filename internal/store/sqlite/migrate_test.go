package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// currentSchemaVersion is the highest version internal/store/migrations
// currently embeds. store.Dialect deliberately gives tests no exported
// way to ask the store package this directly (see internal/store/
// dialect.go's minimalism doc comment), so it is hardcoded here; bump it
// alongside adding a new internal/store/migrations/NNN_*.sql.
const currentSchemaVersion = 1

func TestOpen_FreshDatabaseMigratesToLatest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.db")

	db, _, effectiveReadOnly, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	defer db.Close()
	assert.False(t, effectiveReadOnly)

	var ledgerVersion int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&ledgerVersion))
	assert.Equal(t, currentSchemaVersion, ledgerVersion, "schema_migrations must record every embedded migration")

	// The downgrade guard (see plan.md's "Decision: migration-ledger
	// cutover") depends on PRAGMA user_version staying in sync with the
	// ledger's highest applied version even though nothing in this
	// codebase reads the pragma back - only a pre-cutover binary's own
	// guard does.
	var pragmaVersion int
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&pragmaVersion))
	assert.Equal(t, currentSchemaVersion, pragmaVersion, "PRAGMA user_version must track the ledger for a pre-cutover binary's downgrade guard")

	// The schema from 001_init.sql actually exists.
	var tableCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&tableCount))
	assert.Equal(t, 1, tableCount)
}

func TestOpen_ReopenUpToDateDatabaseAppliesNoMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")

	db1, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	// Insert a row so we can tell a re-migration didn't wipe or recreate
	// the table.
	_, err = db1.ExecContext(ctx, `INSERT INTO sessions (id, channel, chat_id, thread_id, created_at, updated_at) VALUES ('s1','cli','c1','',1,1)`)
	require.NoError(t, err)
	require.NoError(t, db1.Close())

	db2, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	defer db2.Close()

	var count int
	require.NoError(t, db2.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 1, count)
}

// TestOpen_NewerLedgerVersionIsRefused is the "ledger newer than binary"
// case: schema_migrations, not PRAGMA user_version, is what a write-mode
// or read-mode open now compares against the embedded migration set, so
// simulating "a newer MTClaw wrote this database" means inserting a
// ledger row ahead of what this binary knows, not poking the pragma.
func TestOpen_NewerLedgerVersionIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")

	db, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`, currentSchemaVersion+1, "future_migration", 0)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, _, _, err = sqlite.Open(ctx, path, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than this binary supports")

	// Read-only open must refuse for the same reason.
	_, _, _, err = sqlite.Open(ctx, path, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than this binary supports")
}

// TestOpen_LegacyDatabaseAdoptsExistingSchemaVersion is the legacy-
// adoption test (plan.md R2): a database built by hand from the frozen
// v1 fixture, with PRAGMA user_version = 1 and no schema_migrations
// table, must be adopted - not re-migrated - on first open by the new
// code. It proves three things: adoption seeds exactly one ledger row,
// it never re-runs 001_init.sql against the sessions table that already
// exists (the table's own DDL in sqlite_master is byte-identical before
// and after), and a row written before the upgrade survives it.
func TestOpen_LegacyDatabaseAdoptsExistingSchemaVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	seedDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	require.NoError(t, err)
	fixture, err := os.ReadFile("testdata/v1_legacy_schema.sql")
	require.NoError(t, err)
	_, err = seedDB.ExecContext(ctx, string(fixture))
	require.NoError(t, err)
	_, err = seedDB.ExecContext(ctx, "PRAGMA user_version = 1")
	require.NoError(t, err)
	_, err = seedDB.ExecContext(ctx, `INSERT INTO sessions (id, channel, chat_id, thread_id, created_at, updated_at) VALUES ('legacy-1','cli','c1','',1000,1000)`)
	require.NoError(t, err)

	var sessionsDDLBefore string
	require.NoError(t, seedDB.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&sessionsDDLBefore))
	require.NoError(t, seedDB.Close())

	db, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	defer db.Close()

	var ledgerCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&ledgerCount))
	assert.Equal(t, 1, ledgerCount, "adoption must seed exactly one ledger row for the legacy version")

	var ledgerVersion int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT version FROM schema_migrations`).Scan(&ledgerVersion))
	assert.Equal(t, 1, ledgerVersion)

	var sessionsDDLAfter string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&sessionsDDLAfter))
	assert.Equal(t, sessionsDDLBefore, sessionsDDLAfter, "adoption must never re-run DDL against an existing table")

	var channel string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT channel FROM sessions WHERE id = 'legacy-1'`).Scan(&channel))
	assert.Equal(t, "cli", channel, "a row written before the upgrade must survive it")
}

// TestFreshSchemaMatchesFrozenLegacyFixture proves
// internal/store/migrations/001_init.sql, rendered through the sqlite
// dialect's DDL tokens, is DDL-equivalent to the frozen
// testdata/v1_legacy_schema.sql - the two are compared per-column rather
// than byte-for-byte, since "DDL-equivalent" is the actual guarantee this
// phase depends on (a legacy database opens cleanly against a freshly
// migrated schema).
func TestFreshSchemaMatchesFrozenLegacyFixture(t *testing.T) {
	ctx := context.Background()

	freshPath := filepath.Join(t.TempDir(), "fresh-compare.db")
	freshDB, _, _, err := sqlite.Open(ctx, freshPath, false)
	require.NoError(t, err)
	defer freshDB.Close()

	legacyPath := filepath.Join(t.TempDir(), "legacy-compare.db")
	legacyDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(legacyPath))
	require.NoError(t, err)
	defer legacyDB.Close()
	fixture, err := os.ReadFile("testdata/v1_legacy_schema.sql")
	require.NoError(t, err)
	_, err = legacyDB.ExecContext(ctx, string(fixture))
	require.NoError(t, err)

	for _, table := range []string{"sessions", "messages", "approvals", "exec_audit", "cron_runs"} {
		assert.Equal(t, tableInfo(t, legacyDB, table), tableInfo(t, freshDB, table), "table %s must be DDL-equivalent between the tokenized migration and the frozen v1 fixture", table)
	}
}

type columnInfo struct {
	name    string
	ctype   string
	notNull bool
	pk      int
}

func tableInfo(t *testing.T, db *sql.DB, table string) []columnInfo {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	require.NoError(t, err)
	defer rows.Close()

	var out []columnInfo
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk))
		out = append(out, columnInfo{name: name, ctype: ctype, notNull: notNull != 0, pk: pk})
	}
	require.NoError(t, rows.Err())
	return out
}

// TestPragmasAndPoolAreSetOnWriterAndReader extends the writer/reader
// pragma coverage this phase must preserve verbatim: WAL,
// busy_timeout=5000, and foreign_keys=ON on both handles, plus the
// writer connection pool capped at exactly one connection (see
// dialect.go's Open doc comment for why each of these is a correctness
// invariant, not a tuning knob).
func TestPragmasAndPoolAreSetOnWriterAndReader(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pragmas.db")

	writer, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	defer writer.Close()

	assertPragmas(t, ctx, writer)
	assert.Equal(t, 1, writer.Stats().MaxOpenConnections, "the writer pool must be capped at one connection")

	reader, _, _, err := sqlite.Open(ctx, path, true)
	require.NoError(t, err)
	defer reader.Close()

	assertPragmas(t, ctx, reader)
}

func assertPragmas(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()

	var journalMode string
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)

	var busyTimeout int
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout))
	assert.Equal(t, 5000, busyTimeout)

	var fk int
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk)
}
