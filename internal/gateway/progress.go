package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tiennm99/MTClaw/internal/agent"
)

// typingRefresh is how often the typing indicator is re-sent while a turn
// runs: Telegram's own indicator expires after ~5s, so this must be shorter
// than that to look continuous.
const typingRefresh = 4 * time.Second

// slowToolNotice is how long a tool call must still be running before the
// user is told about it. Below this, most tools return fast enough that
// the notice would be more noise than the silence it replaces.
const slowToolNotice = 8 * time.Second

// progressReporter turns one turn's agent.Progress events into a typing
// indicator refreshed every typingRefresh, and a "running <tool>..." notice
// for any tool call still in flight after slowToolNotice. It is scoped to
// exactly one turn: newProgressReporter/start/stop bracket one runTurn call.
type progressReporter struct {
	ctx      context.Context
	ch       Channel
	chatID   string
	threadID string
	log      *slog.Logger

	// slowNoticeDelay is slowToolNotice by default; tests shrink it so
	// onEvent's real closure - the one that guards against sending a notice
	// after the turn's own ctx is already done - can be exercised directly
	// instead of a hand-copied stand-in.
	slowNoticeDelay time.Duration

	stopTyping chan struct{}
	typingDone chan struct{}

	mu    sync.Mutex
	timer map[string]*time.Timer // toolCallID -> pending slow-notice timer
}

// newProgressReporter builds a reporter for one turn. ctx bounds every send
// it issues, so a canceled turn stops producing typing/notice traffic
// promptly instead of lingering.
func newProgressReporter(ctx context.Context, ch Channel, chatID, threadID string, log *slog.Logger) *progressReporter {
	return &progressReporter{
		ctx:             ctx,
		ch:              ch,
		chatID:          chatID,
		threadID:        threadID,
		log:             log,
		slowNoticeDelay: slowToolNotice,
		timer:           make(map[string]*time.Timer),
	}
}

// start begins the typing-refresh loop. Call stop when the turn ends.
func (p *progressReporter) start() {
	p.stopTyping = make(chan struct{})
	p.typingDone = make(chan struct{})

	go func() {
		defer close(p.typingDone)
		p.sendTyping()

		ticker := time.NewTicker(typingRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.sendTyping()
			case <-p.stopTyping:
				return
			case <-p.ctx.Done():
				return
			}
		}
	}()
}

// stop ends the typing-refresh loop and cancels any pending slow-tool
// notice timers that never fired.
func (p *progressReporter) stop() {
	close(p.stopTyping)
	<-p.typingDone

	p.mu.Lock()
	for id, t := range p.timer {
		t.Stop()
		delete(p.timer, id)
	}
	p.mu.Unlock()
}

// onEvent is the agent.Progress callback wired into Loop.Run for this turn.
func (p *progressReporter) onEvent(ev agent.Event) {
	switch ev.Kind {
	case agent.EventToolStarted:
		timer := time.AfterFunc(p.slowNoticeDelay, func() {
			select {
			case <-p.ctx.Done():
				// The turn ended (and stop() already tried to cancel this
				// timer) between it firing and this closure actually
				// running - do not send a "running X..." notice after the
				// turn's own reply already went out.
				return
			default:
			}
			if err := p.ch.Send(p.ctx, p.chatID, p.threadID, fmt.Sprintf("running `%s`...", ev.ToolName), ""); err != nil {
				p.log.Debug("gateway: send slow-tool notice failed", "tool", ev.ToolName, "error", err)
			}
		})
		p.mu.Lock()
		if old, ok := p.timer[ev.ToolCallID]; ok {
			// A tool call id reused within one turn (defensive - providers
			// are not expected to do this) must not leak the earlier
			// timer: stop it before this one replaces it in the map.
			old.Stop()
		}
		p.timer[ev.ToolCallID] = timer
		p.mu.Unlock()

	case agent.EventToolFinished:
		p.mu.Lock()
		if t, ok := p.timer[ev.ToolCallID]; ok {
			t.Stop()
			delete(p.timer, ev.ToolCallID)
		}
		p.mu.Unlock()
	}
}

func (p *progressReporter) sendTyping() {
	if err := p.ch.SendTyping(p.ctx, p.chatID, p.threadID); err != nil {
		p.log.Debug("gateway: send typing indicator failed", "error", err)
	}
}
