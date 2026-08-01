package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tiennm99/MTClaw/internal/store"
)

type auditStore struct {
	db *DB
}

// Append inserts a. exec_audit has no foreign key to sessions - deleting a
// session must never erase the record that a command ran - so this insert
// never fails on a missing or already-deleted session_id.
func (au *auditStore) Append(ctx context.Context, a *store.ExecAudit) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}

	res, err := au.db.ExecContext(ctx, `
		INSERT INTO exec_audit (session_id, command, cwd, decision, rule, exit_code, duration_ms, truncated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.SessionID, a.Command, a.CWD, a.Decision, a.Rule,
		toNullInt(a.ExitCode), toNullInt64(a.DurationMS), boolToInt(a.Truncated), toMillis(a.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("append exec audit: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("append exec audit: %w", err)
	}
	a.ID = id
	return nil
}

func (au *auditStore) List(ctx context.Context, limit int) ([]*store.ExecAudit, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := au.db.QueryContext(ctx, `
		SELECT id, session_id, command, cwd, decision, rule, exit_code, duration_ms, truncated, created_at
		FROM exec_audit ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list exec audit: %w", err)
	}
	defer rows.Close()

	var out []*store.ExecAudit
	for rows.Next() {
		var a store.ExecAudit
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
