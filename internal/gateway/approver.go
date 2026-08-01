package gateway

import (
	"context"
	"sync"

	"github.com/tiennm99/MTClaw/internal/tools"
)

// approverMux implements tools.Approver, selecting the real approver by
// req.Channel: "telegram" gets the Telegram inline-button approver;
// anything else (cron in phase 8, or an unrecognized channel) gets
// tools.DenyAllApprover, since no interactive surface exists to ask on. The
// registry is built once with a mux value; setTelegram fills in the
// Telegram approver once the channel exists (see gateway.go's startup
// order), so registry construction does not have to wait on it.
type approverMux struct {
	mu       sync.RWMutex
	telegram tools.Approver
}

var _ tools.Approver = (*approverMux)(nil)

func newApproverMux() *approverMux {
	return &approverMux{}
}

// setTelegram wires the Telegram approver in. Must be called before any
// turn can reach Ask for channel "telegram" - true by construction, since
// Ask only runs once the gateway is fully wired and channel.Start begins
// pumping updates.
func (m *approverMux) setTelegram(a tools.Approver) {
	m.mu.Lock()
	m.telegram = a
	m.mu.Unlock()
}

// Ask dispatches by req.Channel.
func (m *approverMux) Ask(ctx context.Context, req tools.Request) (bool, error) {
	if req.Channel == "telegram" {
		m.mu.RLock()
		a := m.telegram
		m.mu.RUnlock()
		if a != nil {
			return a.Ask(ctx, req)
		}
	}
	return tools.DenyAllApprover{}.Ask(ctx, req)
}
