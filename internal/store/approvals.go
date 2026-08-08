package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const approvalColumns = "id, session_id, channel, chat_id, tool, command, reason, state, message_id, created_at, expires_at, decided_at, decided_by"

type approvalStore struct {
	db *sql.DB
	d  Dialect
}

func (a *approvalStore) Create(ctx context.Context, ap *Approval) error {
	if ap.ID == "" {
		id, err := newApprovalNonce()
		if err != nil {
			return err
		}
		ap.ID = id
	}
	if ap.CreatedAt.IsZero() {
		ap.CreatedAt = time.Now()
	}
	if ap.State == "" {
		ap.State = "pending"
	}

	_, err := a.db.ExecContext(ctx, a.d.Rebind(`
		INSERT INTO approvals (id, session_id, channel, chat_id, tool, command, reason, state, message_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		ap.ID, ap.SessionID, ap.Channel, ap.ChatID, ap.Tool, ap.Command, ap.Reason, ap.State, ap.MessageID,
		toMillis(ap.CreatedAt), toMillis(ap.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("create approval: %w", err)
	}
	return nil
}

// SetMessageID records id's channel message id after the prompt carrying it
// has actually been sent. It is a no-op (not an error) if id does not
// exist: the row may have already expired or been decided by the time the
// send confirms, and that race must never turn into a failed Ask call.
func (a *approvalStore) SetMessageID(ctx context.Context, id, messageID string) error {
	_, err := a.db.ExecContext(ctx, a.d.Rebind(`UPDATE approvals SET message_id = ? WHERE id = ?`), messageID, id)
	if err != nil {
		return fmt.Errorf("set approval message id %s: %w", id, err)
	}
	return nil
}

func (a *approvalStore) Get(ctx context.Context, id string) (*Approval, error) {
	row := a.db.QueryRowContext(ctx, a.d.Rebind(`SELECT `+approvalColumns+` FROM approvals WHERE id = ?`), id)
	ap, err := scanApproval(row)
	if err != nil {
		return nil, fmt.Errorf("get approval %s: %w", id, err)
	}
	return ap, nil
}

// Decide moves a pending approval to state, guarded by WHERE
// state='pending' so a double-tap on an inline button (two Decide calls
// racing for the same id) cannot flip a decision that already landed: only
// the first UPDATE matches a row, the second affects zero rows and returns
// ErrAlreadyDecided.
func (a *approvalStore) Decide(ctx context.Context, id, state, by string) error {
	res, err := a.db.ExecContext(ctx, a.d.Rebind(`
		UPDATE approvals SET state = ?, decided_at = ?, decided_by = ?
		WHERE id = ? AND state = 'pending'`),
		state, toMillis(time.Now()), by, id,
	)
	if err != nil {
		return fmt.Errorf("decide approval %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("decide approval %s: %w", id, err)
	}
	if n == 1 {
		return nil
	}

	// Zero rows matched: either id does not exist, or it is no longer
	// pending. Distinguish the two so callers can tell "unknown approval"
	// from "already decided".
	if _, err := a.Get(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("decide approval %s: %w", id, err)
	}
	return ErrAlreadyDecided
}

func (a *approvalStore) ExpirePending(ctx context.Context, now time.Time) (int, error) {
	res, err := a.db.ExecContext(ctx, a.d.Rebind(`UPDATE approvals SET state = 'expired' WHERE state = 'pending' AND expires_at < ?`), toMillis(now))
	if err != nil {
		return 0, fmt.Errorf("expire pending approvals: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("expire pending approvals: %w", err)
	}
	return int(n), nil
}

func scanApproval(row rowScanner) (*Approval, error) {
	var a Approval
	var createdAt, expiresAt int64
	var decidedAt sql.NullInt64
	err := row.Scan(
		&a.ID, &a.SessionID, &a.Channel, &a.ChatID, &a.Tool, &a.Command, &a.Reason, &a.State, &a.MessageID,
		&createdAt, &expiresAt, &decidedAt, &a.DecidedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt = fromMillis(createdAt)
	a.ExpiresAt = fromMillis(expiresAt)
	a.DecidedAt = fromNullMillis(decidedAt)
	return &a, nil
}
