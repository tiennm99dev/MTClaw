package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/store"
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

	db, err := Open(ctx, path, false)
	require.NoError(t, err)
	defer db.Close()

	st := New(db)
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

	writer, err := Open(ctx, path, false)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	reader, err := Open(ctx, path, true)
	require.NoError(t, err)
	defer reader.Close()
	assert.True(t, reader.ReadOnly)
}

// TestOpen_ReadOnlyOpenOfBehindSchemaDatabaseFails pins the refusal message
// for a database whose user_version has not caught up to the binary's
// latest migration, so the behavior stays intentional rather than
// accidental if another migration is ever added.
func TestOpen_ReadOnlyOpenOfBehindSchemaDatabaseFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "behind.db")

	writer, err := Open(ctx, path, false)
	require.NoError(t, err)
	_, err = writer.ExecContext(ctx, "PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	_, err = Open(ctx, path, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is behind the latest migration")
	assert.Contains(t, err.Error(), "open it for writing")
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
