package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvConfigVar is the environment variable that overrides the default config
// file location.
const EnvConfigVar = "MTCLAW_CONFIG"

// ConfigPath resolves the config file path with precedence
// flag > MTCLAW_CONFIG env var > ~/.mtclaw/config.{yaml,yml}, and reports
// which source won ("flag", "env:MTCLAW_CONFIG", or "default"). The flag
// and env values are tilde-expanded and made absolute relative to the
// process's current working directory, matching normal shell path
// semantics; only paths found *inside* a config file are resolved relative
// to that file's directory instead (see ExpandPath). An explicit flag or
// env value is used byte-for-byte as given and is never extension-rewritten
// - only the default (no flag, no env var) branch chooses between
// config.yaml and config.yml, so a typo'd --config path is an honest "file
// not found" rather than a surprise fallback to a different extension.
//
// The default branch is the one place ConfigPath touches the filesystem
// (via defaultConfigPath's os.Stat calls) to decide between the two
// extensions - a deliberate exception to the otherwise-pure contract this
// function used to have (resolving only via os.UserHomeDir).
func ConfigPath(flagValue string) (path, source string, err error) {
	raw := flagValue
	src := "flag"
	if raw == "" {
		if envValue := os.Getenv(EnvConfigVar); envValue != "" {
			raw = envValue
			src = "env:" + EnvConfigVar
		}
	}
	if raw == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", "", fmt.Errorf("resolve home directory: %w", herr)
		}
		return defaultConfigPath(home), "default", nil
	}

	expanded, eerr := expandTilde(raw)
	if eerr != nil {
		return "", "", eerr
	}
	abs, aerr := filepath.Abs(expanded)
	if aerr != nil {
		return "", "", fmt.Errorf("resolve config path %q: %w", flagValue, aerr)
	}
	return abs, src, nil
}

// defaultConfigPath resolves ~/.mtclaw/config.{yaml,yml} for the no-flag,
// no-env-var case: config.yaml wins whenever it exists (so a fresh
// machine's not-found error and `onboard`'s write both name the canonical
// extension); config.yml is used only when config.yaml is absent; and if
// both exist, config.yaml still wins but a one-line warning to stderr names
// the ignored config.yml so an edit to the wrong file is never silent.
func defaultConfigPath(home string) string {
	dir := filepath.Join(home, ".mtclaw")
	yamlPath := filepath.Join(dir, "config.yaml")
	ymlPath := filepath.Join(dir, "config.yml")

	yamlExists := fileExists(yamlPath)
	ymlExists := fileExists(ymlPath)

	switch {
	case yamlExists && ymlExists:
		fmt.Fprintf(os.Stderr, "warning: both %s and %s exist; using %s (ignoring %s)\n", yamlPath, ymlPath, yamlPath, ymlPath)
		return yamlPath
	case ymlExists:
		return ymlPath
	default:
		return yamlPath
	}
}

// fileExists reports whether path names something os.Stat can see. Any
// error other than "does not exist" - a permission hiccup, for instance -
// is treated as absent rather than propagated, so a transient stat error
// never fails config resolution outright; ConfigPath's own doc comment
// documents this as the exact scope of its new filesystem dependency.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// StateDir returns MTClaw's per-user state directory (~/.mtclaw), which
// holds the default database file and any runtime lock files.
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".mtclaw"), nil
}

// ExpandPath resolves a config value that names a filesystem path: a
// leading "~" expands to the user's home directory, and any path that is
// still relative afterwards is resolved against baseDir (the config file's
// own directory), not the process's current working directory, so mtclaw
// behaves identically regardless of where it is invoked from. An empty
// input is returned unchanged.
func ExpandPath(path, baseDir string) (string, error) {
	if path == "" {
		return "", nil
	}
	expanded, err := expandTilde(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(baseDir, expanded)
	}
	return filepath.Clean(expanded), nil
}

// expandTilde replaces a leading "~" (or "~/", "~\") with the user's home
// directory. Paths like "~otheruser" are left untouched since Go has no
// portable way to resolve another user's home directory.
func expandTilde(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
