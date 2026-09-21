// Package sqlite implements internal/store.Store on top of
// modernc.org/sqlite, a pure-Go (CGo-free) SQLite driver, so MTClaw stays a
// single static binary.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	sqlitedriver "modernc.org/sqlite"

	"github.com/tiennm99/MTClaw/internal/store"
)

// driverName is the name modernc.org/sqlite registers itself under via its
// package init().
const driverName = "sqlite"

// sqliteReadOnlyRecovery is SQLite's extended result code 264
// (SQLITE_READONLY | (1<<8)): a read-only connection cannot roll a WAL
// file forward after the last writer crashed mid-checkpoint, because doing
// so requires write access. Open falls back to a read-write open when it
// sees this code on a read-only attempt.
const sqliteReadOnlyRecovery = 264

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationNamePattern = regexp.MustCompile(`^(\d+)_.+\.sql$`)

// DB wraps the *sql.DB opened against MTClaw's schema. ReadOnly reports the
// mode the connection actually ended up in: a requested read-only open
// silently upgrades to read-write when recovering a WAL file abandoned by
// a crashed writer (see Open).
type DB struct {
	*sql.DB
	ReadOnly bool
}

// Open opens the SQLite database at path, applies the standard pragma set,
// and runs any pending migrations when opened for writing.
//
// readOnly is for CLI read commands (`sessions list`, `sessions show`); the
// writer (gateway, `sessions rm`, `prompt`, `cron run`) must pass false.
// When a read-only open fails with SQLITE_READONLY_RECOVERY, Open
// transparently retries read-write, since only a writer can finish that
// recovery; check DB.ReadOnly to see which mode won.
//
// A user_version above the highest embedded migration - an older binary
// opened a database written by a newer one - is refused in either mode,
// since continuing could corrupt data the binary does not understand.
func Open(ctx context.Context, path string, readOnly bool) (*DB, error) {
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	db, err := openHandle(ctx, path, readOnly)
	if err != nil && readOnly && isReadOnlyRecovery(err) {
		readOnly = false
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
			return nil, fmt.Errorf("create db directory: %w", mkErr)
		}
		db, err = openHandle(ctx, path, readOnly)
	}
	if err != nil {
		return nil, err
	}

	if !readOnly {
		// One physical connection for the whole process: SQLite itself
		// serializes writers, and capping the pool at 1 sidesteps
		// SQLITE_BUSY under the gateway's own goroutine concurrency
		// entirely rather than relying on busy_timeout to paper over it.
		//
		// Invariant this pool size depends on: no store method may call
		// another store method - directly, via a callback, or by holding
		// a transaction or an open *sql.Rows open across the call - on
		// this same *DB. database/sql's pool wait blocks on the caller's
		// context, not on a deadlock detector, so a nested call would
		// hang until ctx expires instead of failing fast; several CLI
		// paths pass context.Background(), which never expires.
		db.SetMaxOpenConns(1)
	}

	if err := migrate(ctx, db, !readOnly); err != nil {
		db.Close()
		return nil, err
	}

	if !readOnly {
		chmodOwnerOnly(path)
	}

	return &DB{DB: db, ReadOnly: readOnly}, nil
}

// chmodOwnerOnly narrows the database file and its WAL sidecars to 0600.
// The database holds conversation history and exec output, so it must not
// be readable by other local users. Under WAL mode the freshest rows live
// in the -wal file until a checkpoint, so covering only the main file
// would leave today's conversation world-readable. Best-effort: the
// sidecars may not exist yet (ENOENT is expected), and an unsupported
// platform must not stop the gateway from starting. The pragma pass at
// open touches both sidecars, so by the time this runs they exist.
func chmodOwnerOnly(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}
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

// migrate reads the database's PRAGMA user_version and, when apply is
// true (the writer path), runs every embedded migration numbered above it,
// each in its own transaction, bumping user_version inside that same
// transaction so a crash mid-migration re-applies cleanly on next open.
func migrate(ctx context.Context, db *sql.DB, apply bool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return errors.New("no embedded migrations found")
	}
	latest := migrations[len(migrations)-1].version

	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (highest known migration is %d); refusing to open to avoid corrupting data", current, latest)
	}
	if current == latest {
		return nil
	}
	if !apply {
		return fmt.Errorf("database schema version %d is behind the latest migration %d; open it for writing (e.g. run the gateway) once to initialize it", current, latest)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// Store is the sqlite-backed store.Store implementation.
type Store struct {
	db *DB
}

// New wraps an already-opened DB (see Open) as a store.Store.
func New(db *DB) *Store {
	return &Store{db: db}
}

func (s *Store) Sessions() store.SessionStore   { return &sessionStore{db: s.db} }
func (s *Store) Messages() store.MessageStore   { return &messageStore{db: s.db} }
func (s *Store) Approvals() store.ApprovalStore { return &approvalStore{db: s.db} }
func (s *Store) Audit() store.AuditStore        { return &auditStore{db: s.db} }
func (s *Store) CronRuns() store.CronRunStore   { return &cronRunStore{db: s.db} }

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }
