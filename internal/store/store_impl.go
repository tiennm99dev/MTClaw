package store

import "database/sql"

// sqlStore is the generic, dialect-driven Store implementation: every
// query this package knows lives in sessions.go, messages.go,
// approvals.go, audit.go, and cron_runs.go, and sqlStore only wires db
// and d into whichever table-scoped sub-store a caller asks for. A
// concrete backend package (internal/store/sqlite is the only one today)
// does nothing more than open db, migrate it, and hand back a Dialect -
// see dialect.go for exactly what a Dialect may vary.
type sqlStore struct {
	db *sql.DB
	d  Dialect
}

// New wraps an already-migrated db (see a backend package's own Open,
// e.g. sqlite.Open) and its Dialect as a Store. It is exported so backend
// packages and the tests that construct a store directly can call it;
// the driver-dispatching, config-aware entry point (store.Open, added in
// a later phase) is built on top of this, not a replacement for it.
func New(db *sql.DB, d Dialect) Store {
	return &sqlStore{db: db, d: d}
}

func (s *sqlStore) Sessions() SessionStore   { return &sessionStore{db: s.db, d: s.d} }
func (s *sqlStore) Messages() MessageStore   { return &messageStore{db: s.db, d: s.d} }
func (s *sqlStore) Approvals() ApprovalStore { return &approvalStore{db: s.db, d: s.d} }
func (s *sqlStore) Audit() AuditStore        { return &auditStore{db: s.db, d: s.d} }
func (s *sqlStore) CronRuns() CronRunStore   { return &cronRunStore{db: s.db, d: s.d} }

// Close closes the underlying database handle.
func (s *sqlStore) Close() error { return s.db.Close() }
