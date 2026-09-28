package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/channel"
)

func TestTrySend_SucceedsWithCapacityRemaining(t *testing.T) {
	ch := make(chan channel.Inbound, 1)
	assert.True(t, trySend(ch, channel.Inbound{Text: "a"}))
}

func TestTrySend_FailsWhenFull(t *testing.T) {
	ch := make(chan channel.Inbound, 1)
	require.True(t, trySend(ch, channel.Inbound{Text: "a"}))
	assert.False(t, trySend(ch, channel.Inbound{Text: "b"}), "a full channel must report a miss, not block")
}
