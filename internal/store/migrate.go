package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationNamePattern = regexp.MustCompile(`^(\d+)_.+\.sql$`)

// schemaMigrationsDDL is the portable migration ledger that replaces the
// SQLite-only user_version counter as the durable record of which
// migrations have run. It is rendered through the dialect's DDL tokens
// before every CREATE, the same as every embedded migration -
// applied_at is a unix-millis timestamp exactly like the application
// schema's own created_at/updated_at columns.
//
// See plan.md's "Decision: migration-ledger cutover" for why
// Dialect.AfterMigrate still writes SQLite's user_version counter even
// though nothing in this package ever reads it back: an old,
// pre-cutover binary's own downgrade guard depends on that counter
// staying in sync with this table, and losing that for free would let
// an old binary silently misread a schema newer than it understands.
const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at {{INT64}} NOT NULL
);`

// migration is one embedded, numbered .sql file.
type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationNamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q does not match the required NNN_name.sql pattern", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration file %q: %w", e.Name(), err)
		}
		content, err := migrationFiles.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: e.Name(), sql: string(content)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

// Migrate brings db's schema_migrations ledger - and, in write mode, its
// schema - up to date with the embedded migrations, using dialect for
// every driver-specific step: DDL token rendering, legacy-version
// detection, and post-migrate bookkeeping.
//
// write selects the writer algorithm (bootstrap the ledger, adopt a
// pre-cutover legacy version if the ledger is empty, then apply anything
// newer) versus the read-only path, which never writes: a missing
// ledger falls back to dialect.LegacyVersion(), and both "newer than
// this binary supports" and "behind the latest migration" are refused
// with the same wording a write-mode Migrate would have used, so a
// caller cannot tell the difference from the error text alone.
func Migrate(ctx context.Context, db *sql.DB, dialect Dialect, write bool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return errors.New("no embedded migrations found")
	}

	if write {
		return migrateWrite(ctx, db, dialect, migrations)
	}
	return migrateReadOnly(ctx, db, dialect, migrations)
}

func migrateWrite(ctx context.Context, db *sql.DB, dialect Dialect, migrations []migration) error {
	current, err := bootstrapLedger(ctx, db, dialect, migrations)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, dialect, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		current = m.version
	}

	if err := dialect.AfterMigrate(ctx, db, current); err != nil {
		return fmt.Errorf("sync driver bookkeeping after migrate: %w", err)
	}
	return nil
}

// bootstrapLedger creates schema_migrations if it does not already exist
// and, when it is empty and the dialect reports a legacy version, adopts
// that version - all inside one transaction. The writer DSN's BEGIN
// IMMEDIATE plus busy_timeout serializes two writers racing this exact
// moment: one waits for the other's commit and then sees the ledger
// already populated, so adoption can never run twice. It executes no DDL
// beyond the ledger table itself - an existing table matching a legacy
// version is never touched, which is the whole point: adopting a
// pre-cutover database migrates its bookkeeping, never its schema.
func bootstrapLedger(ctx context.Context, db *sql.DB, dialect Dialect, migrations []migration) (int, error) {
	latest := migrations[len(migrations)-1].version

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin ledger bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, renderDDL(schemaMigrationsDDL, dialect.DDLTokens())); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, tx, dialect)
	if err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}

	current := maxVersion(applied)
	if len(applied) == 0 {
		legacy, err := dialect.LegacyVersion(ctx, tx)
		if err != nil {
			return 0, fmt.Errorf("read legacy schema version: %w", err)
		}
		if legacy > 0 {
			if err := adoptLegacyVersion(ctx, tx, dialect, migrations, legacy); err != nil {
				return 0, fmt.Errorf("adopt legacy schema version %d: %w", legacy, err)
			}
			current = legacy
		}
	}

	if current > latest {
		return 0, fmt.Errorf("database schema version %d is newer than this binary supports (highest known migration is %d); refusing to open to avoid corrupting data", current, latest)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit ledger bootstrap: %w", err)
	}
	return current, nil
}

// adoptLegacyVersion seeds one ledger row per pre-cutover version
// 1..legacy, naming each row after the embedded migration of the same
// version so the resulting ledger reads identically to one built by
// actually applying that migration. It runs no DDL: the schema a legacy
// database already has on disk is exactly what those migrations would
// have created.
func adoptLegacyVersion(ctx context.Context, tx *sql.Tx, dialect Dialect, migrations []migration, legacy int) error {
	names := make(map[int]string, len(migrations))
	for _, m := range migrations {
		names[m.version] = m.name
	}

	now := time.Now().UnixMilli()
	insert := dialect.Rebind(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`)
	for v := 1; v <= legacy; v++ {
		name := names[v]
		if name == "" {
			// No embedded migration matches this version. In practice
			// that means an older MTClaw wrote a schema this binary has
			// since forgotten, which bootstrapLedger's newer-than-binary
			// check (run immediately after this, in the same
			// uncommitted transaction) refuses before any of this
			// becomes visible - so a placeholder name here is harmless.
			name = fmt.Sprintf("legacy_version_%d", v)
		}
		if _, err := tx.ExecContext(ctx, insert, v, name, now); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, dialect Dialect, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, renderDDL(m.sql, dialect.DDLTokens())); err != nil {
		return err
	}
	insert := dialect.Rebind(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`)
	if _, err := tx.ExecContext(ctx, insert, m.version, m.name, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func appliedVersions(ctx context.Context, tx *sql.Tx, dialect Dialect) ([]int, error) {
	rows, err := tx.QueryContext(ctx, dialect.Rebind(`SELECT version FROM schema_migrations`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func maxVersion(versions []int) int {
	max := 0
	for _, v := range versions {
		if v > max {
			max = v
		}
	}
	return max
}

// migrateReadOnly never writes. A missing schema_migrations table (a
// pre-cutover database opened for the first time since this binary
// upgraded) falls back to dialect.LegacyVersion(); a write-mode open
// would have adopted it into the ledger already, so this fallback exists
// only for the read path.
func migrateReadOnly(ctx context.Context, db *sql.DB, dialect Dialect, migrations []migration) error {
	latest := migrations[len(migrations)-1].version

	current, err := currentVersionReadOnly(ctx, db, dialect)
	if err != nil {
		return err
	}
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (highest known migration is %d); refusing to open to avoid corrupting data", current, latest)
	}
	if current < latest {
		return fmt.Errorf("database schema version %d is behind the latest migration %d; open it for writing (e.g. run the gateway) once to initialize it", current, latest)
	}
	return nil
}

// currentVersionReadOnly treats any error reading schema_migrations -
// most commonly "no such table" on a pre-cutover database - as "the
// ledger does not exist yet" rather than inspecting a driver-specific
// error code, since the only action available either way is the same:
// fall back to the dialect's legacy probe. It never attempts to create
// the table itself; that would be a write against a handle this
// function must treat as read-only.
func currentVersionReadOnly(ctx context.Context, db *sql.DB, dialect Dialect) (int, error) {
	var v sql.NullInt64
	query := dialect.Rebind(`SELECT MAX(version) FROM schema_migrations`)
	if err := db.QueryRowContext(ctx, query).Scan(&v); err != nil {
		return dialect.LegacyVersion(ctx, db)
	}
	return int(v.Int64), nil
}
