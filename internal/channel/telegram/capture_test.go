package telegram

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestCaptureWindowStart_SameSecondMessageCounts proves a message Telegram
// timestamps to the same whole second this capture window started in is
// not dropped: update.Message.Date has only second resolution, while
// time.Now() (what captureWindowStart is fed) almost never lands exactly
// on a second boundary, so comparing them without truncation would compare
// the message's :00 timestamp against a windowStart already a fraction of
// a second into the same second - reading the message as "before" the
// window even though it arrived after CaptureSenders started polling.
func TestCaptureWindowStart_SameSecondMessageCounts(t *testing.T) {
	now := time.Date(2024, 1, 1, 10, 0, 0, 900_000_000, time.UTC) // .9s into the second
	windowStart := captureWindowStart(now)

	msgDate := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC).Unix() // Telegram's own whole-second Date for the same second

	assert.False(t, time.Unix(msgDate, 0).Before(windowStart),
		"a message timestamped in the same second the capture window started must count, not be dropped for arriving before this call's own sub-second start instant")
}

// TestCaptureWindowStart_BackdatesByTheGraceWindow proves the cutoff is
// pushed back by captureWindowGrace (absorbing ordinary clock skew between
// this host and Telegram's servers), not merely truncated to the second.
func TestCaptureWindowStart_BackdatesByTheGraceWindow(t *testing.T) {
	now := time.Date(2024, 1, 1, 10, 0, 5, 0, time.UTC)
	windowStart := captureWindowStart(now)

	assert.Equal(t, now.Add(-captureWindowGrace), windowStart)
}
