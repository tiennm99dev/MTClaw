// Package logging builds the process-wide *slog.Logger from resolved
// config.LogConfig.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiennm99/MTClaw/internal/config"
)

// New builds an *slog.Logger from cfg. An unrecognized level falls back to
// info rather than failing, so a typo'd --log-level flag degrades
// gracefully instead of blocking startup; the only real failure mode is an
// unwritable log file.
func New(cfg config.LogConfig) (*slog.Logger, error) {
	var w io.Writer = os.Stderr
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o755); err != nil {
			return nil, fmt.Errorf("create log directory for %s: %w", cfg.File, err)
		}
		f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open log file %s: %w", cfg.File, err)
		}
		w = f
	}

	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}
	var handler slog.Handler
	if strings.EqualFold(cfg.Format, "json") {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler), nil
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
