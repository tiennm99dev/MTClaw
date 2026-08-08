package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type cronRunStore struct {
	db *sql.DB
	d  Dialect
}

// Append inserts r. The generated id comes back via RETURNING id rather
// than database/sql's driver-specific last-insert-id accessor - see
// audit.go's Append for why that is a package-wide policy.
func (c *cronRunStore) Append(ctx context.Context, r *CronRun) error {
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}

	err := c.db.QueryRowContext(ctx, c.d.Rebind(`
		INSERT INTO cron_runs (job_name, session_id, status, error, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id`),
		r.JobName, r.SessionID, r.Status, r.Error, toMillis(r.StartedAt), toNullMillis(r.FinishedAt),
	).Scan(&r.ID)
	if err != nil {
		return fmt.Errorf("append cron run: %w", err)
	}
	return nil
}

func (c *cronRunStore) Finish(ctx context.Context, id int64, status, errMsg string, finishedAt time.Time) error {
	res, err := c.db.ExecContext(ctx, c.d.Rebind(`
		UPDATE cron_runs SET status = ?, error = ?, finished_at = ? WHERE id = ?`),
		status, errMsg, toMillis(finishedAt), id,
	)
	if err != nil {
		return fmt.Errorf("finish cron run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish cron run: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns the most recent runs first, optionally filtered to one job
// name. limit <= 0 omits the LIMIT clause entirely (see sessions.go's
// List for why).
func (c *cronRunStore) List(ctx context.Context, jobName string, limit int) ([]*CronRun, error) {
	query := `SELECT id, job_name, session_id, status, error, started_at, finished_at FROM cron_runs`
	args := make([]any, 0, 2)
	if jobName != "" {
		query += ` WHERE job_name = ?`
		args = append(args, jobName)
	}
	query += ` ORDER BY started_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := c.db.QueryContext(ctx, c.d.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list cron runs: %w", err)
	}
	defer rows.Close()

	var out []*CronRun
	for rows.Next() {
		var r CronRun
		var finishedAt sql.NullInt64
		var startedAt int64
		if err := rows.Scan(&r.ID, &r.JobName, &r.SessionID, &r.Status, &r.Error, &startedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("list cron runs: %w", err)
		}
		r.StartedAt = fromMillis(startedAt)
		r.FinishedAt = fromNullMillis(finishedAt)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cron runs: %w", err)
	}
	return out, nil
}
