package gateway

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tiennm99/MTClaw/internal/channel"
)

// errGlobalQueueFull is the error OnDone receives when a message is dropped
// because the global inbound queue is already full - relayInbound's own
// overflow policy, not an infrastructure fault.
var errGlobalQueueFull = errors.New("gateway: global inbound queue full")

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
// in closes or ctx is done. ch, when non-nil, is used to tell a dropped
// message's sender the gateway is overloaded - this should never actually
// happen (globalQueueSize is generous), but silence would look
// indistinguishable from the bot being broken.
func relayInbound(ctx context.Context, in <-chan channel.Inbound, out chan<- channel.Inbound, ch Channel, log *slog.Logger) {
	for {
		select {
		case msg, ok := <-in:
			if !ok {
				return
			}
			if !trySend(out, msg) {
				log.Warn("gateway: global inbound queue full; dropping message",
					"channel", msg.Channel, "chat_id", msg.ChatID, "thread_id", msg.ThreadID)
				callOnDone(msg, errGlobalQueueFull)
				notifyGlobalQueueFull(ctx, ch, msg, log)
			}
		case <-ctx.Done():
			return
		}
	}
}

// notifyGlobalQueueFull sends the same honest "still working" reply the
// per-session overflow path uses. Runs on its own goroutine so a slow or
// dead channel cannot stall relayInbound's loop. Unlike
// dispatch.replySessionBusy, there is no cron special case here: cron fires
// go straight to the dispatcher (disp.dispatch, wired in gateway.go) and
// never traverse this relay, so every msg reaching this function carries a
// real chat id.
func notifyGlobalQueueFull(ctx context.Context, ch Channel, msg channel.Inbound, log *slog.Logger) {
	if ch == nil {
		return
	}
	go func() {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := ch.Send(sendCtx, msg.ChatID, msg.ThreadID, sessionBusyReply, msg.MessageID); err != nil {
			log.Error("gateway: send global-queue-full reply failed", "chat_id", msg.ChatID, "error", err)
		}
	}()
}
