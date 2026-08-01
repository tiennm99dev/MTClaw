package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tiennm99/MTClaw/internal/store"
)

type cronRunStore struct {
	db *DB
}

func (c *cronRunStore) Append(ctx context.Context, r *store.CronRun) error {
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}

	res, err := c.db.ExecContext(ctx, `
		INSERT INTO cron_runs (job_name, session_id, status, error, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.JobName, r.SessionID, r.Status, r.Error, toMillis(r.StartedAt), toNullMillis(r.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("append cron run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("append cron run: %w", err)
	}
	r.ID = id
	return nil
}

func (c *cronRunStore) Finish(ctx context.Context, id int64, status, errMsg string, finishedAt time.Time) error {
	res, err := c.db.ExecContext(ctx, `
		UPDATE cron_runs SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
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
		return store.ErrNotFound
	}
	return nil
}

func (c *cronRunStore) List(ctx context.Context, jobName string, limit int) ([]*store.CronRun, error) {
	if limit <= 0 {
		limit = -1
	}

	query := `SELECT id, job_name, session_id, status, error, started_at, finished_at FROM cron_runs`
	args := make([]any, 0, 2)
	if jobName != "" {
		query += ` WHERE job_name = ?`
		args = append(args, jobName)
	}
	query += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list cron runs: %w", err)
	}
	defer rows.Close()

	var out []*store.CronRun
	for rows.Next() {
		var r store.CronRun
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
