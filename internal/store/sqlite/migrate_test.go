package sqlite

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_FreshDatabaseMigratesToLatest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.db")

	db, err := Open(ctx, path, false)
	require.NoError(t, err)
	defer db.Close()

	assert.False(t, db.ReadOnly)

	var version int
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	migrations, err := loadMigrations()
	require.NoError(t, err)
	assert.Equal(t, migrations[len(migrations)-1].version, version)

	// The schema from 001_init.sql actually exists.
	var tableCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&tableCount))
	assert.Equal(t, 1, tableCount)
}

func TestOpen_ReopenUpToDateDatabaseAppliesNoMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")

	db1, err := Open(ctx, path, false)
	require.NoError(t, err)
	// Insert a row so we can tell a re-migration didn't wipe or recreate
	// the table.
	_, err = db1.ExecContext(ctx, `INSERT INTO sessions (id, channel, chat_id, thread_id, created_at, updated_at) VALUES ('s1','cli','c1','',1,1)`)
	require.NoError(t, err)
	require.NoError(t, db1.Close())

	db2, err := Open(ctx, path, false)
	require.NoError(t, err)
	defer db2.Close()

	var count int
	require.NoError(t, db2.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestOpen_NewerSchemaVersionIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")

	db, err := Open(ctx, path, false)
	require.NoError(t, err)
	migrations, err := loadMigrations()
	require.NoError(t, err)
	latest := migrations[len(migrations)-1].version
	_, err = db.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(latest+1))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(ctx, path, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than this binary supports")

	// Read-only open must refuse for the same reason.
	_, err = Open(ctx, path, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than this binary supports")
}

func TestForeignKeysAreOnForWriterAndReader(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fk.db")

	writer, err := Open(ctx, path, false)
	require.NoError(t, err)
	defer writer.Close()

	var fk int
	require.NoError(t, writer.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk, "foreign_keys must be ON on the writer connection")

	reader, err := Open(ctx, path, true)
	require.NoError(t, err)
	defer reader.Close()

	require.NoError(t, reader.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk, "foreign_keys must be ON on the reader connection")
}
