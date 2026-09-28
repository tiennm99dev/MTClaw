package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// currentSchemaVersion is the highest version internal/store/migrations
// currently embeds. store.Dialect deliberately gives tests no exported
// way to ask the store package this directly (see internal/store/
// dialect.go's minimalism doc comment), so it is hardcoded here; bump it
// alongside adding a new internal/store/migrations/NNN_*.sql.
const currentSchemaVersion = 2

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

	// The downgrade guard depends on PRAGMA user_version staying in sync
	// with the ledger's highest applied version even though nothing in this
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

// TestOpen_ApprovalsSessionIDIndexExists pins the index a fresh database
// gets on approvals.session_id: it is an FK child with ON DELETE CASCADE and
// otherwise unindexed, so a session delete's cascade (and any other lookup
// by session_id) would scan the whole table.
func TestOpen_ApprovalsSessionIDIndexExists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")

	db, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	defer db.Close()

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name='approvals' AND sql LIKE '%session_id%'`).Scan(&count))
	assert.GreaterOrEqual(t, count, 1)
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

// TestOpen_WriterCreatesFileWithOwnerOnlyPermissions proves the database
// file and its WAL sidecars - which hold conversation history and exec
// output - are not left group/world readable after a writer open and a
// first write. Unix-only: file mode bits are not meaningful on Windows.
func TestOpen_WriterCreatesFileWithOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on Windows")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "perms.db")

	db, dia, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	defer db.Close()

	st := store.New(db, dia)
	sess, err := st.Sessions().Ensure(ctx, "cli", "perms", "")
	require.NoError(t, err)
	require.NoError(t, st.Messages().Append(ctx, sess.ID, []store.Message{{Role: "user", Content: "hello"}}))

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		require.NoError(t, err, p)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), p)
	}
}

// TestOpen_ReadOnlyOpenOfUpToDateDatabaseSucceeds pins the read-only path
// every CLI read command (sessions list/show, cron list, approvals list)
// depends on: a database already at the latest schema version must open
// read-only without ever needing a writer to run first. Adding a migration
// breaks this for every existing database until a writer upgrades it.
func TestOpen_ReadOnlyOpenOfUpToDateDatabaseSucceeds(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "uptodate.db")

	writer, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	reader, _, effectiveReadOnly, err := sqlite.Open(ctx, path, true)
	require.NoError(t, err)
	defer reader.Close()
	assert.True(t, effectiveReadOnly)
}

// TestOpen_ReadOnlyOpenOfBehindSchemaDatabaseFails pins the refusal message
// for a database whose ledger has not caught up to the binary's latest
// migration, so the behavior stays intentional rather than accidental if
// another migration is ever added.
func TestOpen_ReadOnlyOpenOfBehindSchemaDatabaseFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "behind.db")

	writer, _, _, err := sqlite.Open(ctx, path, false)
	require.NoError(t, err)
	_, err = writer.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", currentSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	_, _, _, err = sqlite.Open(ctx, path, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is behind the latest migration")
	assert.Contains(t, err.Error(), "open it for writing")
}

// TestOpen_LegacyDatabaseAdoptsExistingSchemaVersion is the legacy-
// adoption test: a database built by hand from the frozen v1 fixture,
// with PRAGMA user_version = 1 and no schema_migrations table, must be
// adopted - not re-migrated - on first open by the new code. It proves
// three things: adoption seeds exactly one ledger row, it never re-runs
// 001_init.sql against the sessions table that already exists (the
// table's own DDL in sqlite_master is byte-identical before and after),
// and a row written before the upgrade survives it.
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

	var legacyRowCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&legacyRowCount))
	assert.Equal(t, 1, legacyRowCount, "adoption must seed exactly one ledger row for the legacy version")

	// Anything newer than the adopted legacy version must still apply on
	// top of it in the same open, exactly as it would for a fresh database.
	var ledgerVersion int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&ledgerVersion))
	assert.Equal(t, currentSchemaVersion, ledgerVersion, "migrations newer than the adopted legacy version must still apply")

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
