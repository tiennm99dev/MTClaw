package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type messageStore struct {
	db *sql.DB
	d  Dialect
}

// Append assigns seq atomically and commits the whole slice in one
// transaction, on the writer's BEGIN IMMEDIATE handle (see the sqlite
// dialect's dsn helper for _txlock=immediate): reading
// COALESCE(MAX(seq),0) and inserting the batch must not interleave with
// another writer's Append on the same session, or two callers could
// compute the same next seq and collide on the UNIQUE(session_id, seq)
// constraint - or worse, silently skip a seq if the constraint were ever
// relaxed. Any failure mid-batch rolls back every row this call
// attempted, including ones inserted earlier in the same call, so a
// turn's assistant tool_calls row can never persist without its matching
// tool rows.
func (m *messageStore) Append(ctx context.Context, sessionID string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append messages: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, m.d.Rebind(`SELECT MAX(seq) FROM messages WHERE session_id = ?`), sessionID).Scan(&maxSeq); err != nil {
		return fmt.Errorf("append messages: read max seq: %w", err)
	}
	next := maxSeq.Int64 // 0 when the session has no messages yet (NULL scans as 0, invalid)

	stmt, err := tx.PrepareContext(ctx, m.d.Rebind(`
		INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, tool_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`))
	if err != nil {
		return fmt.Errorf("append messages: prepare insert: %w", err)
	}
	defer stmt.Close()

	now := toMillis(time.Now())
	for i, msg := range msgs {
		next++
		if _, err := stmt.ExecContext(ctx, sessionID, next, msg.Role, msg.Content, msg.ToolCalls, msg.ToolCallID, msg.ToolName, now); err != nil {
			return fmt.Errorf("append messages: insert message %d/%d: %w", i+1, len(msgs), err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append messages: commit: %w", err)
	}
	return nil
}

// Recent returns the last n messages in ascending seq order. n <= 0 omits
// the LIMIT clause entirely (see sessions.go's List for why that, not a
// driver-specific sentinel, is how this package spells "no limit").
func (m *messageStore) Recent(ctx context.Context, sessionID string, n int) ([]Message, error) {
	query := `SELECT id, session_id, seq, role, content, tool_calls, tool_call_id, tool_name, created_at
		FROM messages WHERE session_id = ? ORDER BY seq DESC`
	args := []any{sessionID}
	if n > 0 {
		query += ` LIMIT ?`
		args = append(args, n)
	}

	rows, err := m.db.QueryContext(ctx, m.d.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("recent messages for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	var desc []Message
	for rows.Next() {
		var msg Message
		var createdAt int64
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Seq, &msg.Role, &msg.Content, &msg.ToolCalls, &msg.ToolCallID, &msg.ToolName, &createdAt); err != nil {
			return nil, fmt.Errorf("recent messages for session %s: %w", sessionID, err)
		}
		msg.CreatedAt = fromMillis(createdAt)
		desc = append(desc, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent messages for session %s: %w", sessionID, err)
	}

	// desc is newest-first (ORDER BY seq DESC); reverse in place to return
	// ascending seq order as documented.
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}

func (m *messageStore) CountBySession(ctx context.Context, sessionID string) (int, error) {
	var n int
	if err := m.db.QueryRowContext(ctx, m.d.Rebind(`SELECT COUNT(*) FROM messages WHERE session_id = ?`), sessionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages for session %s: %w", sessionID, err)
	}
	return n, nil
}

func (m *messageStore) DeleteBySession(ctx context.Context, sessionID string) error {
	if _, err := m.db.ExecContext(ctx, m.d.Rebind(`DELETE FROM messages WHERE session_id = ?`), sessionID); err != nil {
		return fmt.Errorf("delete messages for session %s: %w", sessionID, err)
	}
	return nil
}
