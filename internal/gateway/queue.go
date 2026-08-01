package gateway

import (
	"context"
	"log/slog"

	"github.com/tiennm99/MTClaw/internal/channel"
)

// sessionQueueSize is each session worker's own buffered queue: enough to
// absorb a burst without unbounded growth. A chat that fills this is
// legitimately spamming the bot faster than the model can answer, so
// overflow gets an honest reply instead of quietly growing forever.
const sessionQueueSize = 8

// globalQueueSize bounds total pending inbound messages across every
// session, independent of how many sessions are active. A drop here means
// something upstream is badly wrong (a stuck relay, an unresponsive
// dispatcher); the warn log is the signal to look, not something a user
// should ever cause under normal use.
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

// relayInbound forwards every message from in to out with a non-blocking
// send, warning and dropping on overflow instead of blocking. It exists so
// channel.Channel.Start - which does a blocking send into whatever channel
// it is given - never becomes the thing that enforces globalQueueSize: in
// is an unbuffered handoff a producer's blocking send resolves against
// immediately, while out (bounded at globalQueueSize) is where real
// backlog bounds and drop-on-full actually apply. relayInbound returns once
// in closes or ctx is done.
func relayInbound(ctx context.Context, in <-chan channel.Inbound, out chan<- channel.Inbound, log *slog.Logger) {
	for {
		select {
		case msg, ok := <-in:
			if !ok {
				return
			}
			if !trySend(out, msg) {
				log.Warn("gateway: global inbound queue full; dropping message",
					"channel", msg.Channel, "chat_id", msg.ChatID, "thread_id", msg.ThreadID)
			}
		case <-ctx.Done():
			return
		}
	}
}
