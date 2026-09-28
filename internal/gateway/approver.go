package gateway

import (
	"context"

	"github.com/tiennm99/MTClaw/internal/tools"
)

// approverMux implements tools.Approver, selecting the real approver by
// req.Channel: "telegram" gets the Telegram inline-button approver;
// anything else (cron, or an unrecognized channel) gets
// tools.DenyAllApprover, since no interactive surface exists to ask on.
// telegram is fixed at construction (see gateway.go's startup order, which
// builds the Telegram channel before the tool registry that will hold
// this mux), so there is no mutable-setter or nil-until-wired state to
// guard with a mutex.
type approverMux struct {
	telegram tools.Approver
}

var _ tools.Approver = (*approverMux)(nil)

func newApproverMux(telegram tools.Approver) *approverMux {
	return &approverMux{telegram: telegram}
}

// Ask dispatches by req.Channel.
func (m *approverMux) Ask(ctx context.Context, req tools.Request) (bool, error) {
	if req.Channel == "telegram" && m.telegram != nil {
		return m.telegram.Ask(ctx, req)
	}
	return tools.DenyAllApprover{}.Ask(ctx, req)
}
