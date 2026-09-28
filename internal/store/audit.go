package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type auditStore struct {
	db *sql.DB
	d  Dialect
}

// Append inserts a. exec_audit has no foreign key to sessions - deleting a
// session must never erase the record that a command ran - so this insert
// never fails on a missing or already-deleted session_id.
//
// The generated id comes back via RETURNING id rather than
// database/sql's driver-specific last-insert-id accessor, which the
// standard Postgres driver does not implement; RETURNING is supported by
// SQLite >=3.35 (see dialect.go for why this is a package-wide policy,
// not a per-call choice).
func (au *auditStore) Append(ctx context.Context, a *ExecAudit) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}

	err := au.db.QueryRowContext(ctx, au.d.Rebind(`
		INSERT INTO exec_audit (session_id, command, cwd, decision, rule, exit_code, duration_ms, truncated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`),
		a.SessionID, a.Command, a.CWD, a.Decision, a.Rule,
		toNullInt(a.ExitCode), toNullInt64(a.DurationMS), boolToInt(a.Truncated), toMillis(a.CreatedAt),
	).Scan(&a.ID)
	if err != nil {
		return fmt.Errorf("append exec audit: %w", err)
	}
	return nil
}

// List returns the most recent audit rows first. limit <= 0 omits the
// LIMIT clause entirely (see sessions.go's List for why).
func (au *auditStore) List(ctx context.Context, limit int) ([]*ExecAudit, error) {
	query := `SELECT id, session_id, command, cwd, decision, rule, exit_code, duration_ms, truncated, created_at
		FROM exec_audit ORDER BY created_at DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := au.db.QueryContext(ctx, au.d.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list exec audit: %w", err)
	}
	defer rows.Close()

	var out []*ExecAudit
	for rows.Next() {
		var a ExecAudit
		var exitCode, durationMS sql.NullInt64
		var truncated int
		var createdAt int64
		if err := rows.Scan(&a.ID, &a.SessionID, &a.Command, &a.CWD, &a.Decision, &a.Rule, &exitCode, &durationMS, &truncated, &createdAt); err != nil {
			return nil, fmt.Errorf("list exec audit: %w", err)
		}
		a.ExitCode = fromNullInt(exitCode)
		a.DurationMS = fromNullInt64(durationMS)
		a.Truncated = truncated != 0
		a.CreatedAt = fromMillis(createdAt)
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list exec audit: %w", err)
	}
	return out, nil
}
