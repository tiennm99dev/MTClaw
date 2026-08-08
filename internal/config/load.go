package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	yaml "github.com/goccy/go-yaml"
)

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

// Load decodes YAML bytes onto Default(), resolves secret indirection,
// expands paths relative to baseDir (the config file's directory) -
// folding the deprecated storage.path alias into storage.dsn along the
// way, see expandStorage - and validates the result. env stands in for
// the process environment so the whole pipeline is a pure function of its
// inputs.
func Load(data []byte, baseDir string, env map[string]string) (*Config, error) {
	cfg := Default()

	dec := yaml.NewDecoder(bytes.NewReader(data), yaml.DisallowUnknownField(), yaml.Strict())
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

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
// time - a later phase (doctor / provider construction) is responsible for
// deciding whether the assistant can actually run without it.
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

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", "", fmt.Errorf("read secret file %s: %w", filePath, err)
	}
	return strings.TrimRight(string(data), "\r\n"), "file:" + filePath, nil
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
	if err := expandStorage(cfg, baseDir); err != nil {
		return err
	}
	if err := expand(&cfg.Log.File); err != nil {
		return err
	}
	return nil
}

// expandStorage resolves storage.dsn / storage.path and folds the
// deprecated storage.path alias into storage.dsn, but only when
// cfg.Storage.Driver is "sqlite" - a DSN is a filesystem path for this
// driver and nothing here has a defined meaning for any other (a second
// driver's DSN would be a connection string, not a path to expand); see
// plan.md's R7. validateStorage's driver whitelist rejects anything but
// "sqlite" regardless, so an unsupported driver is simply left untouched
// here and reported there instead.
//
// The fold - and the deprecation warning it prints - must happen here,
// before ExpandPath ever touches either field, not in validateStorage:
// Load always decodes the user's YAML onto Default()'s already-populated
// Storage.DSN, so a config that sets only storage.path is
// indistinguishable, by field emptiness alone, from one that also
// explicitly repeats DSN's own default value - comparing the still-literal,
// unexpanded DSN against defaultStorageDSN is what makes that distinction
// possible. This is also why validateStorage's own both-set check can
// afford to be the simple, unambiguous "both fields are non-empty" test:
// by the time it runs (via Load), a legitimate path-only config already
// had Path folded away and cleared.
//
// The one case this cannot resolve: a user who sets storage.dsn to
// exactly its own default value while also setting storage.path is
// treated as path-only, not as a conflict. Accepted deliberately - see
// plan.md's risk table entry on "both-set becomes an error for someone
// who set both harmlessly" - in exchange for never false-positiving on
// the vastly more common case this whole alias exists for: an untouched,
// pre-existing config that only ever set storage.path.
func expandStorage(cfg *Config, baseDir string) error {
	if cfg.Storage.Driver != "sqlite" {
		return nil
	}

	if cfg.Storage.Path != "" {
		dsnExplicitlySet := cfg.Storage.DSN != "" && cfg.Storage.DSN != defaultStorageDSN
		if !dsnExplicitlySet {
			warnStoragePathDeprecated()
			cfg.Storage.DSN = cfg.Storage.Path
			cfg.Storage.Path = ""
		}
		// dsnExplicitlySet: leave both fields set. validateStorage reports
		// the conflict naming both keys; expanding both below keeps that
		// error's paths absolute either way.
	} else if cfg.Storage.DSN == "" {
		cfg.Storage.DSN = defaultStorageDSN
	}

	dsn, err := ExpandPath(cfg.Storage.DSN, baseDir)
	if err != nil {
		return err
	}
	cfg.Storage.DSN = dsn

	path, err := ExpandPath(cfg.Storage.Path, baseDir)
	if err != nil {
		return err
	}
	cfg.Storage.Path = path
	return nil
}

// warnStoragePathDeprecated prints storage.path's deprecation notice once
// per Load call that actually uses the alias, via the same
// fmt.Fprintf(os.Stderr, ...) mechanism warnIfWorldReadable already uses
// for a load-time warning that must not depend on a logger existing yet
// (the logger itself is built from the config this function is helping to
// load).
func warnStoragePathDeprecated() {
	fmt.Fprintln(os.Stderr, "warning: storage.path is deprecated; use storage.dsn instead (storage.path keeps working, with no removal planned)")
}

// ensureDirCreatable verifies storage.EffectiveDSN()'s parent directory
// either already exists or can be created, by actually creating it.
// Idempotent: safe to call on every load, including read-only commands
// like `config show`.
func ensureDirCreatable(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// MarshalRedacted renders cfg back to YAML with every resolved secret
// replaced by a placeholder describing its source ("<set:env:NAME>",
// "<set:file:PATH>") or "<unset>" - never the secret value itself, which
// lives only in unexported fields that yaml.Marshal cannot reach.
func MarshalRedacted(cfg *Config) ([]byte, error) {
	redacted := *cfg
	redacted.OpenAI.APIKeyInline = redactedSecretLabel(cfg.OpenAI.APIKeySource())
	redacted.Channels.Telegram.TokenInline = redactedSecretLabel(cfg.Channels.Telegram.TokenSource())
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
