package gateway

import (
	"github.com/tiennm99/MTClaw/internal/channel"
)

// sessionQueueSize is each session worker's own buffered queue: enough to
// absorb a burst without unbounded growth. A chat that fills this is
// legitimately spamming the bot faster than the model can answer, so
// overflow gets an honest reply instead of quietly growing forever.
const sessionQueueSize = 8

// globalQueueSize bounds the gateway's one inbound channel, shared by every
// session, independent of how many sessions are active. Once full, the
// channel's own blocking send in Channel.Start pushes back on the channel
// implementation's own buffering instead of anything here dropping a
// message.
const globalQueueSize = 256

// trySend attempts a non-blocking send of v on ch, reporting whether it
// succeeded. It never blocks: a full channel is reported as a miss so
// callers can apply their own overflow policy instead of silently
// stalling.
func trySend(ch chan<- channel.Inbound, v channel.Inbound) bool {
	select {
	case ch <- v:
		return true
	default:
		return false
	}
}
