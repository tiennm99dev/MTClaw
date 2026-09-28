package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newSessionID returns a lexicographically sortable id: an 11-hex-digit
// zero-padded millisecond timestamp (sorts by creation order, good for
// ~1.7e13 ms - well past this century) followed by 10 random hex digits
// for collision resistance within the same millisecond. A dependency-free
// stand-in for a ULID: it satisfies the schema comment's "ulid-ish,
// sortable" contract without pulling in a module for one property.
func newSessionID() (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return fmt.Sprintf("%011x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:])), nil
}

// newApprovalNonce returns a random 32-hex-digit token, used both as the
// approvals.id primary key and as the Telegram inline button's
// callback_data payload, so a nonce is exactly what the callback carries
// back.
func newApprovalNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate approval id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
