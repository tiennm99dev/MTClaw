package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
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
// would mean fabricating a crashed-writer WAL, which this phase's spec
// explicitly rules out for a unit test; a UNIQUE constraint violation is
// used instead purely to obtain a genuine *sqlitedriver.Error from the
// real driver to classify.
//
// This file stays package sqlite (white-box), not sqlite_test, because
// isReadOnlyRecovery is unexported - but it imports neither
// internal/store nor internal/store/sqlite's own store_test.go/
// migrate_test.go helpers, so it introduces none of the import-cycle risk
// that made those two files move to sqlite_test (see plan.md finding A7).
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
