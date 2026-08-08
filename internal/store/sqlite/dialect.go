// Package sqlite implements internal/store.Dialect on top of
// modernc.org/sqlite, a pure-Go (CGo-free) SQLite driver, so MTClaw stays
// a single static binary. Every MTClaw table name, every generic query,
// and the migration ledger algorithm itself live in internal/store; this
// package holds nothing but what genuinely varies by driver: the DSN,
// the pragma set, the connection pool shape, read-only-recovery
// handling, and how a pre-cutover ("legacy") schema version is read and
// synced.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	sqlitedriver "modernc.org/sqlite"

	"github.com/tiennm99/MTClaw/internal/store"
)

// driverName is the name modernc.org/sqlite registers itself under via its
// package init().
const driverName = "sqlite"

// init registers this package as internal/store's "sqlite" backend, the
// database/sql-style pattern the factory (internal/store/factory.go) uses
// to dispatch on storage.driver without importing this package directly -
// that import would cycle, since this file already imports internal/store.
// A caller that only ever imports internal/store therefore needs a
// deliberate `_ "github.com/tiennm99/MTClaw/internal/store/sqlite"` blank
// import to make "sqlite" resolvable at all; forgetting it fails at
// runtime (store.ErrUnknownDriver), not at compile time (see plan.md's
// R8 and the three production wiring sites it names).
func init() {
	store.Register(driverName, Open)
}

// sqliteReadOnlyRecovery is SQLite's extended result code 264
// (SQLITE_READONLY | (1<<8)): a read-only connection cannot roll a WAL
// file forward after the last writer crashed mid-checkpoint, because doing
// so requires write access. Open falls back to a read-write open when it
// sees this code on a read-only attempt.
const sqliteReadOnlyRecovery = 264

// dialect is internal/store's Dialect for modernc.org/sqlite. It carries
// no state - every method is a pure function of its arguments - so the
// zero value is always ready to use.
type dialect struct{}

func (dialect) Name() string { return driverName }

// Rebind is the identity function: SQLite accepts '?' bind variables
// verbatim, the same syntax internal/store's generic SQL already writes,
// so there is nothing to rewrite. Kept as an explicit method - rather
// than omitted - so store.Dialect has exactly one implementation to
// compare a future, non-identity Rebind (e.g. Postgres's '$1', '$2', ...)
// against.
func (dialect) Rebind(query string) string { return query }

// DDLTokens is deliberately identical to what a pre-cutover MTClaw wrote
// by hand in migrations/001_init.sql, so a freshly migrated database is
// byte-comparable with a legacy one (see
// internal/store/sqlite/testdata/v1_legacy_schema.sql and its adoption
// test).
func (dialect) DDLTokens() map[string]string {
	return map[string]string{
		"{{IDENTITY}}": "INTEGER PRIMARY KEY AUTOINCREMENT",
		"{{INT64}}":    "INTEGER",
	}
}

// LegacyVersion reads PRAGMA user_version, the migration ledger a
// pre-cutover MTClaw kept. It runs against whatever Querier the caller
// passes - migrate.go's bootstrap transaction during a write-mode open,
// or the plain *sql.DB during a read-only one - since the pragma is a
// property of the database file, not of any particular connection or
// transaction.
func (dialect) LegacyVersion(ctx context.Context, q store.Querier) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read legacy schema version: %w", err)
	}
	return v, nil
}

// AfterMigrate keeps PRAGMA user_version in sync with the ledger's
// highest applied version after every successful write-mode migrate.
// Nothing in this codebase ever reads it back - migrate.go's own
// bookkeeping lives entirely in schema_migrations - but a pre-cutover
// binary still does: its own downgrade guard compares its highest known
// migration against this pragma, and syncing it here is the only reason
// that guard keeps working against a post-cutover database. See
// plan.md's "Decision: migration-ledger cutover".
func (dialect) AfterMigrate(ctx context.Context, db *sql.DB, applied int) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", applied)); err != nil {
		return fmt.Errorf("sync user_version to %d: %w", applied, err)
	}
	return nil
}

// Open opens the SQLite database at dsn (a filesystem path), applies the
// standard pragma set, and runs any pending migrations when opened for
// writing. It returns the store.Dialect a caller passes to store.New
// alongside the returned handle.
//
// readOnly is for CLI read commands (`sessions list`, `sessions show`);
// the writer (gateway, `sessions rm`, `prompt`, `cron run`) must pass
// false. When a read-only open fails with SQLITE_READONLY_RECOVERY, Open
// transparently retries read-write, since only a writer can finish that
// recovery; the returned bool reports which mode actually won.
//
// A schema newer than this binary's highest embedded migration - an
// older binary opened a database written by a newer one - is refused in
// either mode, since continuing could corrupt data the binary does not
// understand.
func Open(ctx context.Context, dsn string, readOnly bool) (*sql.DB, store.Dialect, bool, error) {
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(dsn), 0o755); err != nil {
			return nil, nil, false, fmt.Errorf("create db directory: %w", err)
		}
	}

	db, err := openHandle(ctx, dsn, readOnly)
	if err != nil && readOnly && isReadOnlyRecovery(err) {
		readOnly = false
		if mkErr := os.MkdirAll(filepath.Dir(dsn), 0o755); mkErr != nil {
			return nil, nil, false, fmt.Errorf("create db directory: %w", mkErr)
		}
		db, err = openHandle(ctx, dsn, readOnly)
	}
	if err != nil {
		return nil, nil, false, err
	}

	if !readOnly {
		// One physical connection for the whole process: SQLite itself
		// serializes writers, and capping the pool at 1 sidesteps
		// SQLITE_BUSY under the gateway's own goroutine concurrency
		// entirely rather than relying on busy_timeout to paper over it.
		db.SetMaxOpenConns(1)
	}

	dia := dialect{}
	if err := store.Migrate(ctx, db, dia, !readOnly); err != nil {
		db.Close()
		return nil, nil, false, err
	}

	return db, dia, readOnly, nil
}

// dsn builds the sqlite driver DSN for path.
//
// Every _pragma value is executed verbatim as "PRAGMA <value>" by the
// driver on every new physical connection it opens, which is what makes
// foreign_keys=ON reliable across a read-only handle's connection pool,
// not just the one connection applyPragmas touches directly after Open.
//
// _txlock=immediate makes every database/sql transaction started on a
// write handle begin with BEGIN IMMEDIATE instead of the default BEGIN
// DEFERRED, so a read-then-write transaction (Append, Decide, ...) fails
// fast under busy_timeout instead of silently upgrading to a write
// mid-transaction - which fails SQLITE_BUSY at upgrade time without
// honoring busy_timeout at all.
func dsn(path string, readOnly bool) string {
	q := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if readOnly {
		q += "&mode=ro"
	} else {
		q += "&_txlock=immediate"
	}
	return "file:" + filepath.ToSlash(path) + "?" + q
}

func openHandle(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn(path, readOnly))
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := applyPragmas(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// applyPragmas re-issues the pragma set via ExecContext on the connection
// database/sql just established, so correctness never depends solely on
// the driver's DSN-parameter parsing.
//
// Setting journal_mode on a read-only connection is a documented no-op
// (verified empirically against modernc.org/sqlite v1.55.0 on Windows: it
// neither errors nor changes anything), so the same statement list runs
// unconditionally for both read-only and read-write connections.
func applyPragmas(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply pragma (%s): %w", stmt, err)
		}
	}
	return nil
}

func isReadOnlyRecovery(err error) bool {
	var sqliteErr *sqlitedriver.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteReadOnlyRecovery
}
