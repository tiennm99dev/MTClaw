// Package mock provides a scripted provider.Provider for testing the agent
// loop and everything downstream of it (phases 4-9) without any network
// access.
package mock

import (
	"context"
	"fmt"
	"sync"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// Step scripts one Provider.Complete response: either a tool-call turn
// (ToolCalls set) or a final text turn (Content set, ToolCalls empty). Err,
// if set, makes Complete return that error instead of a response.
type Step struct {
	Content      string
	ToolCalls    []provider.ToolCall
	FinishReason string // defaults to "tool_calls" when ToolCalls is set, else "stop"
	Usage        provider.Usage
	Err          error
}

// Provider is a scripted provider.Provider: it returns each Step in order
// and records every Request it receives, so a test can assert the follow-up
// request (after a tool call) contains the tool's result.
type Provider struct {
	mu       sync.Mutex
	steps    []Step
	next     int
	requests []provider.Request
}

// New builds a Provider that returns steps in order, one per Complete call.
func New(steps ...Step) *Provider {
	return &Provider{steps: steps}
}

var _ provider.Provider = (*Provider)(nil)

// Complete returns the next scripted Step's response, recording req first.
// Calling Complete more times than there are Steps is a test-authoring bug,
// reported as an error rather than a panic or a silently repeated step.
func (p *Provider) Complete(ctx context.Context, req provider.Request) (*provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, provider.Classify(err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, req)

	if p.next >= len(p.steps) {
		return nil, fmt.Errorf("mock provider: no scripted step for call %d (only %d scripted)", p.next+1, len(p.steps))
	}
	step := p.steps[p.next]
	p.next++

	if step.Err != nil {
		return nil, step.Err
	}

	finish := step.FinishReason
	if finish == "" {
		if len(step.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}

	return &provider.Response{
		Message: provider.Message{
			Role:      provider.RoleAssistant,
			Content:   step.Content,
			ToolCalls: step.ToolCalls,
		},
		Usage:        step.Usage,
		FinishReason: finish,
	}, nil
}

// Requests returns every Request received so far, in call order.
func (p *Provider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.Request, len(p.requests))
	copy(out, p.requests)
	return out
}
