// Package tools implements MTClaw's tool registry and policy engine: the
// filesystem, web_fetch, and exec tools the agent loop can call, and the
// deny-list -> allow-list -> mode pipeline that gates exec. See
// plans/260731-2219-mtclaw-core-system/phase-05-tools-and-policy-engine.md
// for the security model this package is required to implement, in
// particular that the deny-list - not the auto-mode classifier - is the
// only real enforcement boundary.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store"
)

// ToolFunc executes one tool call. Like agent.ToolRunner.Run, it never
// returns a Go error for a tool-level failure (bad arguments, a path
// outside the confined roots, a non-zero exit code): those become result
// strings so the model can react. A non-nil error is reserved for the turn
// itself ending - typically ctx cancellation surfacing out of a running
// exec command - which the agent loop treats as an abort, not a retryable
// tool failure.
type ToolFunc func(ctx context.Context, args json.RawMessage, meta agent.Meta) (string, error)

// Tool pairs one provider.ToolSpec with the function that implements it.
type Tool struct {
	Spec provider.ToolSpec
	Run  ToolFunc
}

// Registry implements agent.ToolRunner over a fixed set of Tools assembled
// once at process startup by New.
type Registry struct {
	tools map[string]Tool
	names []string // registration order, so Specs() output is stable
}

var _ agent.ToolRunner = (*Registry)(nil)

// NewRegistry returns an empty Registry. Most callers want New, which
// builds and registers every tool from config; NewRegistry exists mainly
// for tests that register a handful of fakes directly.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds t under name, overwriting any previous registration for
// the same name (used only by tests; New never registers the same name
// twice).
func (r *Registry) Register(name string, t Tool) {
	if _, exists := r.tools[name]; !exists {
		r.names = append(r.names, name)
	}
	r.tools[name] = t
}

// Specs returns every registered tool's provider.ToolSpec, in registration
// order.
func (r *Registry) Specs() []provider.ToolSpec {
	specs := make([]provider.ToolSpec, 0, len(r.names))
	for _, name := range r.names {
		specs = append(specs, r.tools[name].Spec)
	}
	return specs
}

// Run dispatches call to the matching Tool. An unknown tool name or
// malformed arguments are never a Go error here: they are the model's
// mistake, reported back as a result string so it can self-correct on the
// next turn instead of aborting the whole conversation.
func (r *Registry) Run(ctx context.Context, call provider.ToolCall, meta agent.Meta) (string, error) {
	t, ok := r.tools[call.Name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q; available tools: %s", call.Name, strings.Join(r.names, ", ")), nil
	}
	return t.Run(ctx, call.Args, meta)
}

// New builds the full tool registry for one process from cfg: filesystem
// tools when tools.filesystem.enabled, web_fetch when tools.web_fetch.enabled,
// and exec when tools.exec.enabled and its mode is not "off" (mode: off is
// deliberately implemented as "never register the tool", not as a runtime
// check inside it). approver is used only by the exec tool; a nil approver
// falls back to DenyAllApprover so a caller that forgets to wire one fails
// safe instead of panicking on first use.
func New(cfg config.Config, st store.Store, approver Approver, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	if approver == nil {
		approver = DenyAllApprover{}
	}

	r := NewRegistry()

	if cfg.Tools.Filesystem.Enabled {
		registerFilesystemTools(r, cfg.Tools.Filesystem)
	}
	if cfg.Tools.WebFetch.Enabled {
		registerWebFetchTool(r, cfg.Tools.WebFetch)
	}
	if cfg.Tools.Exec.Enabled && cfg.Tools.Exec.Mode != "off" {
		if err := registerExecTool(r, cfg, st, approver, log); err != nil {
			return nil, err
		}
	}

	return r, nil
}
