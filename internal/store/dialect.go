// Package store's generic SQL implementation (this file and its
// sibling query files) routes every statement through a Dialect, so
// internal/store/sqlite - and any future backend implementing this
// interface - is the only place a driver name, a native placeholder
// syntax, or a legacy-version probe appears. See migrate.go for the
// portable migration ledger this seam feeds, and internal/store/sqlite's
// dialect.go for today's only implementation.
//
// The interface is deliberately minimal: a method exists only where
// today's single production backend already varies by driver. Three
// portability questions came up while designing this seam and were
// resolved by policy instead of by adding a method, so a future reader
// does not reinvent them:
//
//   - Upsert form. "INSERT ... ON CONFLICT (...) DO UPDATE ... RETURNING"
//     is shared by SQLite >=3.35 and Postgres, and sessions.go's Ensure
//     already proves it works against this package's pure-Go SQLite
//     driver in production. One portable statement beats a renderer with
//     a single caller.
//   - Identity-insert strategy. Every insert that needs its generated id
//     back uses "RETURNING id" (see audit.go, cron_runs.go), rather than
//     database/sql's driver-specific last-insert-id accessor. This
//     forecloses MySQL/MariaDB, which is an accepted trade-off since
//     MySQL is not a target (see plan.md's R13).
//   - Limit clause. "No limit" is spelled by omitting the LIMIT clause
//     entirely, rather than by a driver-specific negative-count sentinel
//     - omitting the clause is portable everywhere by construction.
package store

import (
	"context"
	"database/sql"
	"strings"
)

// Querier is satisfied by both *sql.DB and *sql.Tx, letting
// Dialect.LegacyVersion run either inside migrate.go's bootstrap
// transaction (so its result is read consistently with the ledger read
// taken in that same transaction) or directly against a handle on the
// read-only path, which never opens a transaction of its own.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Dialect is the seam between this package's generic SQL and one
// backend's driver-specific behaviour. internal/store/sqlite is the only
// implementation today; a second backend is one new package implementing
// this interface and registering itself with internal/store/factory.go's
// driver registry (phase 3) - nothing above this file changes.
type Dialect interface {
	// Name is the driver name a config's storage.driver selects (the
	// registry key a factory looks up), and the name the underlying
	// database/sql driver registers itself under.
	Name() string

	// Rebind rewrites this package's '?' placeholders into the driver's
	// native bind-variable syntax. SQLite and MySQL both accept '?'
	// verbatim; a Postgres dialect would rewrite to '$1', '$2', ...
	Rebind(query string) string

	// DDLTokens maps migration template tokens to this dialect's SQL
	// types. Required keys: "{{IDENTITY}}" (an auto-incrementing primary
	// key column) and "{{INT64}}" (a 64-bit integer column, used for
	// every unix-millis timestamp column in the schema).
	DDLTokens() map[string]string

	// LegacyVersion reports a schema version recorded by a pre-cutover
	// MTClaw binary that predates schema_migrations (SQLite: the
	// user_version counter). 0 means "no legacy state": either a brand
	// new database, or one whose ledger is already populated. See
	// migrate.go's bootstrapLedger for how this combines with an empty
	// ledger to adopt an existing database without re-running its schema.
	LegacyVersion(ctx context.Context, q Querier) (int, error)

	// AfterMigrate runs once after a successful write-mode Migrate, for
	// driver bookkeeping that is not part of the portable schema (SQLite:
	// syncing the user_version counter to the highest applied migration,
	// so an old binary's own downgrade guard still fires against a
	// post-cutover database - see plan.md's "Decision: migration-ledger
	// cutover").
	AfterMigrate(ctx context.Context, db *sql.DB, applied int) error
}

// renderDDL substitutes tokens (dialect.DDLTokens()) into a migration's
// SQL text. A strings.NewReplacer is enough: token substitution is the
// only portability transform a migration file needs, and every token is a
// literal, non-overlapping placeholder.
func renderDDL(sqlText string, tokens map[string]string) string {
	pairs := make([]string, 0, len(tokens)*2)
	for token, replacement := range tokens {
		pairs = append(pairs, token, replacement)
	}
	return strings.NewReplacer(pairs...).Replace(sqlText)
}
