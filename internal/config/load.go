package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	yaml "github.com/goccy/go-yaml"
)

// maxSecretFileBytes bounds how much of a *_file secret (openai.api_key_file,
// channels.telegram.token_file) is ever read into memory: a config value
// pointed at an endless stream (/dev/zero, a FIFO nothing ever closes) must
// not hang or exhaust memory loading a value that is never more than a
// short line of text in practice.
const maxSecretFileBytes = 64 * 1024

// FileNotFoundError is returned by LoadFile when the config file does not
// exist, distinct from other read errors, so callers such as a future
// `onboard` command can offer to create it instead of failing outright.
type FileNotFoundError struct {
	Path string
}

func (e *FileNotFoundError) Error() string {
	return fmt.Sprintf("config file not found: %s", e.Path)
}

// LoadFile reads path from disk and loads it using the process environment.
// It is the convenience entry point for CLI commands; Load itself takes an
// explicit env map so the whole pipeline stays a pure, table-testable
// function of (file bytes, env map, OS).
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &FileNotFoundError{Path: path}
		}
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}
	return Load(data, filepath.Dir(path), processEnv())
}

func processEnv() map[string]string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env
}

// Load decodes YAML bytes onto Default(), normalizes a couple of
// intentionally-duplicated spellings, resolves secret indirection, expands
// paths relative to baseDir (the config file's directory), and validates
// the result. env stands in for the process environment; this is still not
// a pure function of (data, baseDir, env) alone, since resolving a *_file
// secret reads that file from disk when one is set - see resolveSecret and
// LoadFile, its impure os.ReadFile-based shim.
func Load(data []byte, baseDir string, env map[string]string) (*Config, error) {
	cfg := Default()

	dec := yaml.NewDecoder(bytes.NewReader(data), yaml.DisallowUnknownField(), yaml.Strict())
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	normalizeExecMode(cfg)

	if err := resolveSecrets(cfg, baseDir, env); err != nil {
		return nil, err
	}

	if err := expandConfigPaths(cfg, baseDir); err != nil {
		return nil, err
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// normalizeExecMode collapses the "mode: off" spelling into "enabled:
// false" right after decode, so every reader past this point (the tool
// registry, doctor's checks) only ever has to look at one field. "off" is
// still accepted in the YAML itself for backward compatibility and remains
// a valid tools.exec.mode value (see validateTools); it is just no longer
// authoritative once Load returns.
func normalizeExecMode(cfg *Config) {
	if cfg.Tools.Exec.Mode == "off" {
		cfg.Tools.Exec.Enabled = false
	}
}

// resolveSecrets fills in the unexported secret fields from *_env / *_file
// indirection. A literal inline secret (api_key/token) is intentionally
// left alone here - Validate is what rejects it - so a config with an
// inline secret still yields a decodable, inspectable Config for tests.
func resolveSecrets(cfg *Config, baseDir string, env map[string]string) error {
	apiKey, apiKeySource, err := resolveSecret("OPENAI_API_KEY", cfg.OpenAI.APIKeyEnv, cfg.OpenAI.APIKeyFile, baseDir, env)
	if err != nil {
		return fmt.Errorf("openai secret: %w", err)
	}
	cfg.OpenAI.setSecret(apiKey, apiKeySource)

	token, tokenSource, err := resolveSecret("TELEGRAM_BOT_TOKEN", cfg.Channels.Telegram.TokenEnv, cfg.Channels.Telegram.TokenFile, baseDir, env)
	if err != nil {
		return fmt.Errorf("telegram secret: %w", err)
	}
	cfg.Channels.Telegram.setSecret(token, tokenSource)

	return nil
}

// resolveSecret resolves one secret via *_env / *_file indirection: the
// named env var (falling back to defaultEnvName when envName is empty)
// wins if set to a non-empty value; otherwise the *_file path is read and
// trimmed of a trailing newline. Neither being set is not an error at load
// time - `mtclaw doctor` and provider construction are what decide whether
// the assistant can actually run without it. The file read
// is capped at maxSecretFileBytes and refuses anything that is not a
// regular file, so a *_file pointed at a FIFO or an endless device
// (/dev/zero, a pipe nothing ever closes) cannot hang or exhaust memory
// loading a value that is never more than a short line of text in practice.
func resolveSecret(defaultEnvName, envName, filePath, baseDir string, env map[string]string) (value, source string, err error) {
	name := envName
	if name == "" {
		name = defaultEnvName
	}
	if v, ok := env[name]; ok && v != "" {
		return v, "env:" + name, nil
	}

	if filePath == "" {
		return "", "unset", nil
	}

	resolvedPath, err := ExpandPath(filePath, baseDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve secret file %s: %w", filePath, err)
	}
	warnIfWorldReadable(resolvedPath)

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return "", "", fmt.Errorf("read secret file %s: %w", filePath, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("read secret file %s: not a regular file", filePath)
	}

	f, err := os.Open(resolvedPath)
	if err != nil {
		return "", "", fmt.Errorf("read secret file %s: %w", filePath, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes))
	if err != nil {
		return "", "", fmt.Errorf("read secret file %s: %w", filePath, err)
	}

	trimmed := strings.TrimRight(string(data), "\r\n")
	if trimmed == "" {
		// An empty (or all-newline) secret file is the same as none set: a
		// non-empty apiKeySource of "file:..." would otherwise render as
		// "<set:file:...>" in `config show`, misreporting a secret that
		// resolved to nothing.
		return "", "unset", nil
	}
	return trimmed, "file:" + filePath, nil
}

// warnIfWorldReadable enforces that a *_file secret is not readable by
// other users on POSIX systems. Windows' os.FileMode does not reliably
// reflect ACL-based permissions, so this is a documented no-op there rather
// than a check that would be either always-true or always-false noise.
func warnIfWorldReadable(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o004 != 0 {
		fmt.Fprintf(os.Stderr, "warning: secret file %s is world-readable; consider chmod 600 %s\n", path, path)
	}
}

// expandConfigPaths resolves every path-shaped config field: a leading "~"
// expands to the user's home directory, and anything still relative is
// resolved against baseDir rather than the process's working directory.
func expandConfigPaths(cfg *Config, baseDir string) error {
	expand := func(path *string) error {
		v, err := ExpandPath(*path, baseDir)
		if err != nil {
			return err
		}
		*path = v
		return nil
	}
	expandAll := func(paths []string) error {
		for i := range paths {
			if err := expand(&paths[i]); err != nil {
				return err
			}
		}
		return nil
	}

	if err := expand(&cfg.Agent.Workspace); err != nil {
		return err
	}
	if err := expandAll(cfg.Agent.SystemPromptFiles); err != nil {
		return err
	}
	if err := expandAll(cfg.Tools.Filesystem.Roots); err != nil {
		return err
	}
	if err := expand(&cfg.Tools.Exec.CWD); err != nil {
		return err
	}
	if err := expand(&cfg.Storage.Path); err != nil {
		return err
	}
	if err := expand(&cfg.Log.File); err != nil {
		return err
	}
	return nil
}

// ensureDirCreatable verifies storage.path's parent directory - or, if it
// does not exist yet, its nearest existing ancestor - exists as a
// directory, without creating anything on disk and without inspecting
// permission bits. A directory's owner-write bit says nothing about
// whether this process can actually write into it: a group/ACL-writable
// but not owner-writable directory would be a false reject, and an
// owner-writable directory owned by someone else would be a false accept.
// The fallible case (a directory that stats fine here but still refuses a
// later write) is handled correctly either way by the read-write caller
// that actually needs it to exist (state.openStore in internal/cli, via
// sqlite.Open's MkdirAll). Load must stay side-effect-free for read-only
// commands like `config show` and `config validate`.
func ensureDirCreatable(dir string) error {
	d := filepath.Clean(dir)
	for {
		info, err := os.Stat(d)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", d)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(d)
		if parent == d {
			return fmt.Errorf("no existing ancestor directory found for %s", dir)
		}
		d = parent
	}
}

// MarshalRedacted renders cfg back to YAML with every resolved secret
// replaced by a placeholder describing its source ("<set:env:NAME>",
// "<set:file:PATH>") or "<unset>" - never the secret value itself, which
// lives only in unexported fields that yaml.Marshal cannot reach. Every
// group's require_mention is also rendered as its effective value (see
// TelegramGroupConfig.MentionRequired): RequireMention's `omitempty` YAML
// tag means a group that omits the key prints no require_mention line at
// all, which a reader could easily mistake for an effective `false` instead
// of the documented default of `true`.
func MarshalRedacted(cfg *Config) ([]byte, error) {
	redacted := *cfg
	redacted.OpenAI.APIKeyInline = redactedSecretLabel(cfg.OpenAI.APIKeySource())
	redacted.Channels.Telegram.TokenInline = redactedSecretLabel(cfg.Channels.Telegram.TokenSource())

	if cfg.Channels.Telegram.Groups != nil {
		groups := make(map[string]TelegramGroupConfig, len(cfg.Channels.Telegram.Groups))
		for key, g := range cfg.Channels.Telegram.Groups {
			g.RequireMention = Bool(g.MentionRequired())
			groups[key] = g
		}
		redacted.Channels.Telegram.Groups = groups
	}

	return yaml.Marshal(&redacted)
}

func redactedSecretLabel(source string) string {
	switch {
	case strings.HasPrefix(source, "env:"):
		return "<set:" + source + ">"
	case strings.HasPrefix(source, "file:"):
		return "<set:" + source + ">"
	default:
		return "<unset>"
	}
}
