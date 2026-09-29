package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqlitedriver "modernc.org/sqlite"
)

// TestIsReadOnlyRecovery_DiscriminatesCode264FromOtherSQLiteErrors proves
// isReadOnlyRecovery inspects the driver error's Code() rather than just
// its dynamic type: it must return false for a *sqlitedriver.Error whose
// code is not 264 (SQLITE_READONLY_RECOVERY).
//
// Code 264 itself fires only when a read-only connection has to roll a
// WAL file forward after the writer that owned it crashed mid-checkpoint
// (see Open's doc comment above). Reproducing that deterministically
// would mean fabricating a crashed-writer WAL, which a unit test cannot do
// portably; a UNIQUE constraint violation is
// used instead purely to obtain a genuine *sqlitedriver.Error from the
// real driver to classify.
//
// This file stays package sqlite (white-box), not sqlite_test, because
// isReadOnlyRecovery is unexported - but it imports neither
// internal/store nor internal/store/sqlite's own store_test.go/
// migrate_test.go helpers (which do import internal/store, and so live in
// sqlite_test to avoid an import cycle), so it introduces none of that
// risk.
func TestIsReadOnlyRecovery_DiscriminatesCode264FromOtherSQLiteErrors(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.db")

	db, err := sql.Open(driverName, "file:"+filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO t (id) VALUES (1)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO t (id) VALUES (1)`)
	require.Error(t, err, "a duplicate primary key must fail")

	var sqliteErr *sqlitedriver.Error
	require.ErrorAs(t, err, &sqliteErr, "modernc.org/sqlite must surface its own typed error")
	assert.NotEqual(t, sqliteReadOnlyRecovery, sqliteErr.Code())
	assert.False(t, isReadOnlyRecovery(err))
}

// TestFinishRecovery_ReturnsReadOnlyHandleWithoutMigrating pins what Open
// does after a read-only open hits the recovery error: the file is opened
// read-write only to be recovered, and the caller gets back a handle that
// cannot write and a schema that was never touched. A raw table with no
// schema_migrations ledger stands in for "a database the read path must
// not migrate".
func TestFinishRecovery_ReturnsReadOnlyHandleWithoutMigrating(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recover.db")

	seed, err := sql.Open(driverName, dsn(path, false))
	require.NoError(t, err)
	_, err = seed.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	require.NoError(t, seed.Close())

	db, err := finishRecovery(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx, `INSERT INTO t (id) VALUES (1)`)
	require.Error(t, err, "the recovered handle must be read-only")
	assert.Contains(t, strings.ToLower(err.Error()), "readonly")

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'schema_migrations'`).Scan(&n))
	assert.Zero(t, n, "recovery must not migrate the schema")
}

// TestDSN_EscapesURIMetacharactersInPath proves '%', '?' and '#' in a
// database path reach SQLite as literal path characters: the file must
// appear at the exact configured path, owner-only, instead of at a
// truncated or decoded one.
func TestDSN_EscapesURIMetacharactersInPath(t *testing.T) {
	for _, dir := range []string{"a#1", "b?x", "c%41", "d%2Fe"} {
		t.Run(dir, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), dir, "mtclaw.db")

			db, _, _, err := Open(ctx, path, false)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			info, err := os.Stat(path)
			require.NoError(t, err, "database must exist at the exact configured path")
			if runtime.GOOS != "windows" {
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
		})
	}
}
