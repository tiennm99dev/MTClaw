package agent

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
)

// Build assembles the system prompt fresh for one turn, in the fixed order
// documented in phase 4: identity, workspace, tool guidance (generated from
// tools, never hand-maintained), exec policy posture, the contents of every
// agent.system_prompt_files entry, then conventions. log may be nil.
func Build(cfg config.Config, tools []provider.ToolSpec, now time.Time, log *slog.Logger) string {
	if log == nil {
		log = slog.Default()
	}

	var b strings.Builder
	writeIdentity(&b, cfg.Agent, now)
	writeWorkspace(&b, cfg)
	writeToolGuidance(&b, tools)
	writeExecPolicy(&b, cfg.Tools.Exec.Mode)
	writePromptFiles(&b, cfg.Agent.SystemPromptFiles, log)
	writeConventions(&b)
	return b.String()
}

func writeIdentity(b *strings.Builder, agentCfg config.AgentConfig, now time.Time) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}
	fmt.Fprintf(b, "# Identity\n")
	fmt.Fprintf(b, "You are %s, a personal AI agent gateway running as a single process.\n", agentCfg.Name)
	fmt.Fprintf(b, "Current date/time: %s\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(b, "Host: %s (%s/%s)\n\n", hostname, runtime.GOOS, runtime.GOARCH)
}

func writeWorkspace(b *strings.Builder, cfg config.Config) {
	fmt.Fprintf(b, "# Workspace\n")
	fmt.Fprintf(b, "Your working directory is %s.\n", cfg.Agent.Workspace)
	if len(cfg.Tools.Filesystem.Roots) > 0 {
		fmt.Fprintf(b, "File tools are confined to: %s.\n", strings.Join(cfg.Tools.Filesystem.Roots, ", "))
	} else {
		b.WriteString("File tools are disabled: no roots are configured.\n")
	}
	b.WriteString("\n")
}

// writeToolGuidance renders one line per registered tool, generated from
// the registry passed in rather than hand-maintained here, so the prompt
// never drifts from what the model can actually call.
func writeToolGuidance(b *strings.Builder, tools []provider.ToolSpec) {
	b.WriteString("# Tools\n")
	if len(tools) == 0 {
		b.WriteString("No tools are registered for this turn.\n\n")
		return
	}
	for _, t := range tools {
		fmt.Fprintf(b, "- %s: %s\n", t.Name, t.Description)
	}
	b.WriteString("\n")
}

func writeExecPolicy(b *strings.Builder, execMode string) {
	fmt.Fprintf(b, "# Exec policy\n")
	fmt.Fprintf(b, "The exec tool's active mode is %q. A command denied by policy cannot be retried by rephrasing it.\n\n", execMode)
}

// maxSystemPromptFileSize caps how much of one system_prompt_files entry
// gets read into every request. The system prompt sits outside history and
// the in-turn buffer, so neither HardTrim nor the ErrContextLength retry
// can shed it - an oversized file (a user pointing the setting at a large
// generated file, or accidentally at a log) would otherwise make the
// session permanently un-completable with no diagnostic.
const maxSystemPromptFileSize = 256 * 1024 // 256 KiB

// writePromptFiles reads each configured system_prompt_files entry in
// order, under a header naming its source path. A missing, unreadable,
// non-regular, or oversized file is a warning, not a fatal error: a user
// editing e.g. AGENTS.md should not brick the gateway.
func writePromptFiles(b *strings.Builder, paths []string, log *slog.Logger) {
	for _, p := range paths {
		content, err := readPromptFile(p)
		if err != nil {
			log.Warn(err.Error(), "path", p)
			continue
		}
		if content == nil {
			log.Warn("system prompt file too large, skipping", "path", p, "max", maxSystemPromptFileSize)
			continue
		}
		fmt.Fprintf(b, "# %s\n%s\n\n", p, string(content))
	}
}

// readPromptFile bounds what one system_prompt_files entry can do to the
// turn. The path-based Stat happens before any Open call and never blocks,
// even on a FIFO with no writer connected - the IsRegular check it gates
// must reject a FIFO, character device (/dev/urandom, /dev/zero), or /proc
// entry before Open ever runs, since opening a FIFO for reading is itself
// the call that blocks until a writer shows up, and opening a device like
// /dev/urandom succeeds and then reads until OOM. Once a file is known
// regular, io.LimitReader caps the read at maxSystemPromptFileSize+1 so an
// oversized regular file is still detected without reading all of it.
// There is a benign TOCTOU between this Stat and the Open a few lines
// below - irrelevant to the actual threat here, a misconfigured path (an
// accidental /dev/urandom or log file), not an attacker racing a symlink
// swap against the gateway's own local config on its own machine. A nil,
// nil return means "too large"; the caller logs that case so it can also
// report the size actually read.
func readPromptFile(p string) ([]byte, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("system prompt file unreadable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("system prompt file is not a regular file (mode %s), skipping", info.Mode())
	}

	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("system prompt file unreadable: %w", err)
	}
	defer f.Close()

	content, err := io.ReadAll(io.LimitReader(f, maxSystemPromptFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("system prompt file unreadable: %w", err)
	}
	if len(content) > maxSystemPromptFileSize {
		return nil, nil
	}
	return content, nil
}

func writeConventions(b *strings.Builder) {
	b.WriteString("# Conventions\n")
	b.WriteString("Reply in the user's language. Reply with exactly \"NO_REPLY\" (nothing else) when no response is warranted. Keep chat replies short: the surface is a phone.\n")
}
