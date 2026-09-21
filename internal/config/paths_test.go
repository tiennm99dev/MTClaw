package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setHome points os.UserHomeDir (via $HOME) at a fresh temp dir for the
// duration of the test, so ConfigPath/StateDir/ExpandPath's home-relative
// behavior is deterministic and never touches the real ~/.mtclaw.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows' os.UserHomeDir source
	return home
}

func TestConfigPath_FlagWinsOverEverything(t *testing.T) {
	home := setHome(t)
	t.Setenv(EnvConfigVar, filepath.Join(home, "from-env.yaml"))

	path, source, err := ConfigPath(filepath.Join(home, "from-flag.yaml"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "from-flag.yaml"), path)
	assert.Equal(t, "flag", source)
}

func TestConfigPath_EnvWinsOverDefault(t *testing.T) {
	home := setHome(t)
	t.Setenv(EnvConfigVar, filepath.Join(home, "from-env.yaml"))

	path, source, err := ConfigPath("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "from-env.yaml"), path)
	assert.Equal(t, "env:"+EnvConfigVar, source)
}

func TestConfigPath_DefaultWhenNeitherSet(t *testing.T) {
	home := setHome(t)
	t.Setenv(EnvConfigVar, "")

	path, source, err := ConfigPath("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".mtclaw", "config.yaml"), path)
	assert.Equal(t, "default", source)
}

func TestConfigPath_FlagExpandsTilde(t *testing.T) {
	home := setHome(t)
	t.Setenv(EnvConfigVar, "")

	path, source, err := ConfigPath("~/custom-config.yaml")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "custom-config.yaml"), path)
	assert.Equal(t, "flag", source)
}

func TestStateDir(t *testing.T) {
	home := setHome(t)

	dir, err := StateDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".mtclaw"), dir)
}

func TestExpandPath(t *testing.T) {
	home := setHome(t)
	baseDir := t.TempDir()

	t.Run("empty is unchanged", func(t *testing.T) {
		v, err := ExpandPath("", baseDir)
		require.NoError(t, err)
		assert.Equal(t, "", v)
	})

	t.Run("tilde expands to home", func(t *testing.T) {
		v, err := ExpandPath("~/workspace", baseDir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(home, "workspace"), v)
	})

	t.Run("relative resolves against baseDir, not cwd", func(t *testing.T) {
		v, err := ExpandPath("sub/dir", baseDir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(baseDir, "sub", "dir"), v)
	})

	t.Run("absolute is cleaned but otherwise unchanged", func(t *testing.T) {
		abs := filepath.Join(baseDir, "already", "..", "already-abs")
		v, err := ExpandPath(abs, baseDir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean(abs), v)
	})
}

func TestExpandTilde(t *testing.T) {
	home := setHome(t)

	t.Run("bare tilde", func(t *testing.T) {
		v, err := expandTilde("~")
		require.NoError(t, err)
		assert.Equal(t, home, v)
	})

	t.Run("tilde slash prefix", func(t *testing.T) {
		v, err := expandTilde("~/foo/bar")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(home, "foo", "bar"), v)
	})

	t.Run("other user's home is left untouched", func(t *testing.T) {
		v, err := expandTilde("~otheruser/foo")
		require.NoError(t, err)
		assert.Equal(t, "~otheruser/foo", v)
	})

	t.Run("no leading tilde is unchanged", func(t *testing.T) {
		v, err := expandTilde("/already/absolute")
		require.NoError(t, err)
		assert.Equal(t, "/already/absolute", v)
	})
}
