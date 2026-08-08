package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const sessionColumns = "id, channel, chat_id, thread_id, title, model, summary, prompt_tokens, completion_tokens, created_at, updated_at"

type sessionStore struct {
	db *sql.DB
	d  Dialect
}

func (s *sessionStore) Ensure(ctx context.Context, channel, chatID, threadID string) (*Session, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	now := toMillis(time.Now())

	row := s.db.QueryRowContext(ctx, s.d.Rebind(`
		INSERT INTO sessions (id, channel, chat_id, thread_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel, chat_id, thread_id) DO UPDATE SET updated_at = excluded.updated_at
		RETURNING `+sessionColumns),
		id, channel, chatID, threadID, now, now,
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("ensure session: %w", err)
	}
	return sess, nil
}

func (s *sessionStore) Get(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, s.d.Rebind(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`), id)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("get session %s: %w", id, err)
	}
	return sess, nil
}

// List returns the most recently updated sessions first. limit <= 0 omits
// the LIMIT clause entirely rather than passing a driver-specific
// negative-count "unlimited" sentinel, which is portable everywhere by
// construction.
func (s *sessionStore) List(ctx context.Context, limit int) ([]*Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions ORDER BY updated_at DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, s.d.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

func (s *sessionStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.d.Rebind(`DELETE FROM sessions WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sessionStore) SetSummary(ctx context.Context, id, summary string) error {
	res, err := s.db.ExecContext(ctx, s.d.Rebind(`UPDATE sessions SET summary = ?, updated_at = ? WHERE id = ?`), summary, toMillis(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set summary for session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set summary for session %s: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sessionStore) AddUsage(ctx context.Context, id string, prompt, completion int) error {
	res, err := s.db.ExecContext(ctx, s.d.Rebind(`
		UPDATE sessions
		SET prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ?, updated_at = ?
		WHERE id = ?`),
		prompt, completion, toMillis(time.Now()), id,
	)
	if err != nil {
		return fmt.Errorf("add usage for session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("add usage for session %s: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSession(row rowScanner) (*Session, error) {
	var s Session
	var createdAt, updatedAt int64
	err := row.Scan(
		&s.ID, &s.Channel, &s.ChatID, &s.ThreadID, &s.Title, &s.Model, &s.Summary,
		&s.PromptTokens, &s.CompletionTokens, &createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.CreatedAt = fromMillis(createdAt)
	s.UpdatedAt = fromMillis(updatedAt)
	return &s, nil
}
