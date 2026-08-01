package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tiennm99/MTClaw/internal/store"
)

const sessionColumns = "id, channel, chat_id, thread_id, title, model, summary, prompt_tokens, completion_tokens, created_at, updated_at"

type sessionStore struct {
	db *DB
}

func (s *sessionStore) Ensure(ctx context.Context, channel, chatID, threadID string) (*store.Session, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	now := toMillis(time.Now())

	row := s.db.QueryRowContext(ctx, `
		INSERT INTO sessions (id, channel, chat_id, thread_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel, chat_id, thread_id) DO UPDATE SET updated_at = excluded.updated_at
		RETURNING `+sessionColumns,
		id, channel, chatID, threadID, now, now,
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("ensure session: %w", err)
	}
	return sess, nil
}

func (s *sessionStore) Get(ctx context.Context, id string) (*store.Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("get session %s: %w", id, err)
	}
	return sess, nil
}

func (s *sessionStore) List(ctx context.Context, limit int) ([]*store.Session, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []*store.Session
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *sessionStore) SetSummary(ctx context.Context, id, summary string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET summary = ?, updated_at = ? WHERE id = ?`, summary, toMillis(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set summary for session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set summary for session %s: %w", id, err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *sessionStore) AddUsage(ctx context.Context, id string, prompt, completion int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ?, updated_at = ?
		WHERE id = ?`,
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
		return store.ErrNotFound
	}
	return nil
}

func scanSession(row rowScanner) (*store.Session, error) {
	var s store.Session
	var createdAt, updatedAt int64
	err := row.Scan(
		&s.ID, &s.Channel, &s.ChatID, &s.ThreadID, &s.Title, &s.Model, &s.Summary,
		&s.PromptTokens, &s.CompletionTokens, &createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	s.CreatedAt = fromMillis(createdAt)
	s.UpdatedAt = fromMillis(updatedAt)
	return &s, nil
}
