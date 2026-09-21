package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tiennm99/MTClaw/internal/store"
)

type messageStore struct {
	db *DB
}

// Append assigns seq atomically and commits the whole slice in one
// transaction, on the writer's BEGIN IMMEDIATE handle (see dsn's
// _txlock=immediate): reading COALESCE(MAX(seq),0) and inserting the batch
// must not interleave with another writer's Append on the same session, or
// two callers could compute the same next seq and collide on the
// UNIQUE(session_id, seq) constraint - or worse, silently skip a seq if
// the constraint were ever relaxed. Any failure mid-batch rolls back every
// row this call attempted, including ones inserted earlier in the same
// call, so a turn's assistant tool_calls row can never persist without its
// matching tool rows.
func (m *messageStore) Append(ctx context.Context, sessionID string, msgs []store.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append messages: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
		return fmt.Errorf("append messages: read max seq: %w", err)
	}
	next := maxSeq.Int64 // 0 when the session has no messages yet (NULL scans as 0, invalid)

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, tool_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
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

	// A session that only ever exchanges zero-usage turns (mock provider,
	// cached responses) would otherwise never bump updated_at, sinking it
	// in `sessions list`'s ORDER BY updated_at DESC despite active
	// traffic. Same tx as the inserts above, so this can never observably
	// land without them.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return fmt.Errorf("append messages: bump session updated_at: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append messages: commit: %w", err)
	}
	return nil
}

func (m *messageStore) Recent(ctx context.Context, sessionID string, n int) ([]store.Message, error) {
	if n <= 0 {
		n = -1
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, session_id, seq, role, content, tool_calls, tool_call_id, tool_name, created_at
		FROM messages WHERE session_id = ? ORDER BY seq DESC LIMIT ?`,
		sessionID, n,
	)
	if err != nil {
		return nil, fmt.Errorf("recent messages for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	var desc []store.Message
	for rows.Next() {
		var msg store.Message
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
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages for session %s: %w", sessionID, err)
	}
	return n, nil
}

func (m *messageStore) DeleteBySession(ctx context.Context, sessionID string) error {
	if _, err := m.db.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete messages for session %s: %w", sessionID, err)
	}
	return nil
}
