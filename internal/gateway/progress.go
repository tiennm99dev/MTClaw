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

	// noticeCtx bounds slow-tool notice sends and is canceled by stop, so a
	// notice send still in flight is aborted rather than awaited. noticeMu
	// serializes those sends against stop: once stop has returned, no
	// notice is in flight and none can start - see onEvent.
	noticeCtx    context.Context
	noticeCancel context.CancelFunc
	noticeMu     sync.Mutex
	stopped      bool // guarded by noticeMu

	mu    sync.Mutex
	timer map[string]*time.Timer // toolCallID -> pending slow-notice timer
}

// newProgressReporter builds a reporter for one turn. ctx bounds every send
// it issues, so a canceled turn stops producing typing/notice traffic
// promptly instead of lingering.
func newProgressReporter(ctx context.Context, ch Channel, chatID, threadID string, log *slog.Logger) *progressReporter {
	noticeCtx, noticeCancel := context.WithCancel(ctx)
	return &progressReporter{
		noticeCtx:       noticeCtx,
		noticeCancel:    noticeCancel,
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
// notice timers that never fired. When it returns, no notice is in flight
// and none will start, so nothing can be sent after the turn's reply.
func (p *progressReporter) stop() {
	close(p.stopTyping)
	<-p.typingDone

	p.noticeCancel() // abort a notice send already in flight, so the lock below is quick
	p.noticeMu.Lock()
	p.stopped = true
	p.noticeMu.Unlock()

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
			// Time.Stop does not wait for a callback that already started,
			// so stop() may have run between this timer firing and this
			// closure getting here. noticeMu makes that ordering decisive:
			// either the notice is sent before stop() returns, or it is not
			// sent at all - never after the turn's own reply.
			p.noticeMu.Lock()
			defer p.noticeMu.Unlock()
			if p.stopped || p.ctx.Err() != nil {
				return
			}
			if err := p.ch.Send(p.noticeCtx, p.chatID, p.threadID, fmt.Sprintf("running `%s`...", ev.ToolName), ""); err != nil {
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
