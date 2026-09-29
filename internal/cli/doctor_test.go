package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/gateway"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
	// This package's own root.go/doctor_checks.go already carry the
	// blank import that registers "sqlite" with store.Open's driver
	// registry - which transitively pulls in the pure-Go SQLite driver,
	// so its database/sql driver name "sqlite" is available process-wide
	// by the time TestCheckDatabase below opens a raw *sql.DB with it
	// directly.
)

// testConfig returns a *config.Config that satisfies config.Validate, with
// every path field rooted under a fresh t.TempDir(), so individual doctor
// checks can be exercised against a known-good baseline and then mutated to
// trigger one failure at a time.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()

	cfg := config.Default()
	cfg.Agent.Model = "gpt-4o-mini"
	cfg.Agent.Workspace = root
	cfg.Channels.Telegram.Enabled = false
	cfg.Tools.Filesystem.Roots = []string{root}
	cfg.Tools.Exec.CWD = root
	cfg.Tools.Exec.Deny = []string{`\brm\s+-rf\b`}
	cfg.Storage.DSN = filepath.Join(root, "mtclaw.db")
	cfg.Cron.Timezone = "UTC"
	return cfg
}

func TestCheckConfigFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("world-readable check is POSIX-only")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))

	res := checkConfigFilePermissions(path)(context.Background(), nil)
	assert.Equal(t, StatusOK, res.Status)

	require.NoError(t, os.Chmod(path, 0o644))
	res = checkConfigFilePermissions(path)(context.Background(), nil)
	assert.Equal(t, StatusWarn, res.Status)
	assert.Contains(t, res.Message, "chmod 600")

	missing := filepath.Join(dir, "missing.yaml")
	res = checkConfigFilePermissions(missing)(context.Background(), nil)
	assert.Equal(t, StatusFail, res.Status)
}

func TestCheckStorageDirWritable(t *testing.T) {
	t.Run("missing directory is judged by its ancestor and not created", func(t *testing.T) {
		cfg := testConfig(t)
		root := t.TempDir()
		cfg.Storage.DSN = filepath.Join(root, "new", "sub", "mtclaw.db")
		res := checkStorageDirWritable(context.Background(), cfg)
		assert.Equal(t, StatusOK, res.Status, res.Message)
		assert.Contains(t, res.Message, "does not exist yet")
		_, err := os.Stat(filepath.Join(root, "new"))
		assert.True(t, os.IsNotExist(err), "doctor must not create the storage directory")
	})

	t.Run("does not touch the home state dir", func(t *testing.T) {
		home := useFakeHome(t)
		cfg := testConfig(t)
		res := checkStorageDirWritable(context.Background(), cfg)
		assert.Equal(t, StatusOK, res.Status, res.Message)
		_, err := os.Stat(filepath.Join(home, ".mtclaw"))
		assert.True(t, os.IsNotExist(err), "the check must not create ~/.mtclaw")
	})

	t.Run("unwritable storage directory fails", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("directory mode bits do not block writes on Windows or for root")
		}
		cfg := testConfig(t)
		dir := t.TempDir()
		cfg.Storage.DSN = filepath.Join(dir, "mtclaw.db")
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		res := checkStorageDirWritable(context.Background(), cfg)
		assert.Equal(t, StatusFail, res.Status)
	})

	t.Run("log directory is checked when log.file is set", func(t *testing.T) {
		cfg := testConfig(t)
		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, nil, 0o600))
		cfg.Log.File = filepath.Join(blocker, "mtclaw.log")
		res := checkStorageDirWritable(context.Background(), cfg)
		assert.Equal(t, StatusFail, res.Status)
		assert.Contains(t, res.Message, "log directory")
	})
}

func TestCheckDatabase(t *testing.T) {
	cfg := testConfig(t)

	// Fresh path: no file yet, must open read-write, create, and migrate.
	res := checkDatabase(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	// Existing, already-migrated file: reopen read-only.
	res = checkDatabase(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	// A schema newer than this binary understands must FAIL, not silently
	// truncate or corrupt. The ledger cutover (see internal/store/
	// migrate.go) means "newer than this binary" is now recorded in
	// schema_migrations, not PRAGMA user_version, so simulating it means
	// inserting an out-of-range ledger row rather than poking the pragma.
	// A bare database/sql.Open under the driver name "sqlite" (registered
	// by the pure-Go SQLite driver's own init, already loaded
	// transitively via this package's blank import) is enough for that
	// one INSERT - no need to go through store.Open (which would re-run
	// Migrate, a no-op here) or directly import the sqlite backend
	// package.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(cfg.Storage.EffectiveDSN()))
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), "INSERT INTO schema_migrations (version, name, applied_at) VALUES (999999, 'future_migration', 0)")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	res = checkDatabase(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
}

// TestCheckDatabase_BehindSchemaIsReportedNotSilentlyMigrated proves a
// database left behind a pending migration (an older binary's schema) is
// never migrated by doctor's own read-only-then-write fallback - only a
// database that does not exist at all gets the write-mode retry. Simulated
// here by deleting the ledger's highest-version row, so the read-only
// open's own "behind the latest migration" check fires.
func TestCheckDatabase_BehindSchemaIsReportedNotSilentlyMigrated(t *testing.T) {
	cfg := testConfig(t)

	db, _, _, err := sqlite.Open(context.Background(), cfg.Storage.EffectiveDSN(), false)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), "DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	res := checkDatabase(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "restart the gateway")

	// The important invariant: the ledger must be untouched by doctor's
	// check - a second, independent read-only open confirms nothing
	// migrated it.
	db2, _, _, err := sqlite.Open(context.Background(), cfg.Storage.EffectiveDSN(), true)
	require.Error(t, err, "a behind-schema database still refuses even a second read-only open")
	if db2 != nil {
		db2.Close()
	}
}

func TestCheckInstanceLock(t *testing.T) {
	cfg := testConfig(t)

	res := checkInstanceLock(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	// Holding the kernel lock from this process on a separate descriptor
	// is exactly what "another instance holds it" looks like to Held,
	// without spawning a real second gateway. gateway.LockPath derives the
	// path from cfg.Storage.Path, so Acquire must target that same path.
	release, err := gateway.Acquire(gateway.LockPath(*cfg))
	require.NoError(t, err)
	res = checkInstanceLock(context.Background(), cfg)
	assert.Equal(t, StatusInfo, res.Status)
	require.NoError(t, release())

	res = checkInstanceLock(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)
}

func TestCheckOpenAIKeyResolves(t *testing.T) {
	cfg := testConfig(t)
	res := checkOpenAIKeyResolves(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "OPENAI_API_KEY")

	loaded, err := config.Load(minimalYAML(t, cfg), t.TempDir(), map[string]string{"OPENAI_API_KEY": "sk-test"})
	require.NoError(t, err)
	res = checkOpenAIKeyResolves(context.Background(), loaded)
	assert.Equal(t, StatusOK, res.Status)
	assert.Contains(t, res.Message, "env:OPENAI_API_KEY")
}

func TestCheckOpenAIReachable_NoKeyDegradesToFail(t *testing.T) {
	cfg := testConfig(t)
	res := checkOpenAIReachable(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "no API key resolved")
}

func TestCheckModelExists_NoKeyDegradesToFail(t *testing.T) {
	cfg := testConfig(t)
	res := checkModelExists(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "no OpenAI API key resolved")
}

func TestCheckTelegramTokenResolves(t *testing.T) {
	cfg := testConfig(t)
	res := checkTelegramTokenResolves(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "disabled channel must skip, not fail")

	cfg.Channels.Telegram.Enabled = true
	res = checkTelegramTokenResolves(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "TELEGRAM_BOT_TOKEN")
}

func TestCheckTelegramGetMe_NoTokenDegradesToFail(t *testing.T) {
	cfg := testConfig(t)
	res := checkTelegramGetMe(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "disabled channel must skip, not fail")

	cfg.Channels.Telegram.Enabled = true
	res = checkTelegramGetMe(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "no Telegram bot token resolved")
}

func TestCheckSystemPromptFiles(t *testing.T) {
	cfg := testConfig(t)
	res := checkSystemPromptFiles(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "empty list has nothing to check")

	existing := filepath.Join(cfg.Agent.Workspace, "AGENTS.md")
	require.NoError(t, os.WriteFile(existing, []byte("hi"), 0o644))
	cfg.Agent.SystemPromptFiles = []string{existing}
	res = checkSystemPromptFiles(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	cfg.Agent.SystemPromptFiles = append(cfg.Agent.SystemPromptFiles, filepath.Join(cfg.Agent.Workspace, "missing.md"))
	res = checkSystemPromptFiles(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "missing.md")
}

func TestCheckWorkspace(t *testing.T) {
	cfg := testConfig(t)
	res := checkWorkspace(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	cfg.Agent.Workspace = filepath.Join(cfg.Agent.Workspace, "does-not-exist")
	res = checkWorkspace(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
}

func TestCheckFilesystemRoots(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tools.Filesystem.Enabled = false
	res := checkFilesystemRoots(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "disabled tool must skip, not fail")

	cfg.Tools.Filesystem.Enabled = true
	res = checkFilesystemRoots(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	cfg.Tools.Filesystem.Roots = []string{filepath.Join(t.TempDir(), "missing")}
	res = checkFilesystemRoots(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
}

func TestCheckExecCWD(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tools.Exec.Enabled = false
	res := checkExecCWD(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "disabled tool must skip, not fail")

	cfg.Tools.Exec.Enabled = true
	res = checkExecCWD(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	cfg.Tools.Exec.CWD = filepath.Join(t.TempDir(), "missing")
	res = checkExecCWD(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
}

func TestCheckShellExists(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tools.Exec.Enabled = false
	res := checkShellExists(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "disabled tool must skip, not fail")

	cfg.Tools.Exec.Enabled = true
	goBin, err := osexec.LookPath("go")
	require.NoError(t, err, "the go toolchain running this test must itself be on PATH")
	cfg.Tools.Exec.Shell = []string{goBin}
	res = checkShellExists(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	cfg.Tools.Exec.Shell = []string{"mtclaw-doctor-definitely-not-a-real-binary-xyz"}
	res = checkShellExists(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
}

func TestCheckDenyListSanity(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tools.Exec.Enabled = false
	res := checkDenyListSanity(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "disabled tool must skip, not fail")

	cfg.Tools.Exec.Enabled = true
	res = checkDenyListSanity(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	cfg.Tools.Exec.Deny = nil
	res = checkDenyListSanity(context.Background(), cfg)
	assert.Equal(t, StatusWarn, res.Status)
	assert.Contains(t, res.Message, "EMPTY")
}

func TestCheckExecModeAuto(t *testing.T) {
	cfg := testConfig(t)
	res := checkExecModeAuto(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)

	cfg.Tools.Exec.Mode = "auto"
	res = checkExecModeAuto(context.Background(), cfg)
	assert.Equal(t, StatusWarn, res.Status)
	assert.Contains(t, res.Message, "not a security control")
}

func TestCheckCron(t *testing.T) {
	cfg := testConfig(t)
	res := checkCron(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status, "cron disabled must skip, not fail")

	cfg.Cron.Enabled = true
	cfg.Cron.Jobs = []config.CronJob{{Name: "daily", Schedule: "0 9 * * *", Enabled: true}}
	res = checkCron(context.Background(), cfg)
	assert.Equal(t, StatusOK, res.Status)
	assert.Contains(t, res.Message, "daily: next")

	cfg.Cron.Timezone = "Not/AZone"
	res = checkCron(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)

	cfg.Cron.Timezone = "UTC"
	cfg.Cron.Jobs[0].Schedule = "not a schedule"
	res = checkCron(context.Background(), cfg)
	assert.Equal(t, StatusFail, res.Status)
}

// minimalYAML renders cfg's effective storage DSN (the only field the
// OpenAI-key-resolution test below needs to satisfy config.Load's parent
// directory check) into a minimal, otherwise-defaulted document with
// telegram disabled so it also satisfies config.Validate. It deliberately
// writes the deprecated storage.path key, not storage.dsn, so this test
// doubles as coverage of the alias actually loading through the real
// config.Load pipeline - TestRunDoctor_FullRunOnLoadableButBrokenConfig
// below exercises the canonical storage.dsn key instead.
func minimalYAML(t *testing.T, cfg *config.Config) []byte {
	t.Helper()
	doc := "version: 1\nagent:\n  model: gpt-4o-mini\nchannels:\n  telegram:\n    enabled: false\nstorage:\n  path: \"" + filepath.ToSlash(cfg.Storage.EffectiveDSN()) + "\"\n"
	return []byte(doc)
}

func TestRunDoctor_MissingConfigIsSoleFailRow(t *testing.T) {
	rows := runDoctor(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Len(t, rows, 1)
	assert.Equal(t, "Config found and valid", rows[0].Check)
	assert.Equal(t, string(StatusFail), rows[0].Status)
	assert.True(t, anyFailed(rows))
}

func TestRunDoctor_FullRunOnLoadableButBrokenConfig_ExitsNonZero(t *testing.T) {
	useFakeHome(t)
	root := t.TempDir()
	missing := filepath.Join(root, "does-not-exist")
	configPath := filepath.Join(root, "config.yaml")

	doc := "" +
		"version: 1\n" +
		"agent:\n" +
		"  model: gpt-4o-mini\n" +
		"  workspace: \"" + filepath.ToSlash(missing) + "\"\n" +
		"channels:\n" +
		"  telegram:\n" +
		"    enabled: false\n" +
		"tools:\n" +
		"  filesystem:\n" +
		"    roots: [\"" + filepath.ToSlash(missing) + "\"]\n" +
		"  exec:\n" +
		"    cwd: \"" + filepath.ToSlash(missing) + "\"\n" +
		"storage:\n" +
		"  dsn: \"" + filepath.ToSlash(filepath.Join(root, "mtclaw.db")) + "\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(doc), 0o600))

	rows := runDoctor(context.Background(), configPath)
	require.True(t, anyFailed(rows), "a config whose workspace/roots/cwd do not exist on disk must fail doctor even though it loads")

	failCount := 0
	for _, r := range rows {
		if r.Status == string(StatusFail) {
			failCount++
		}
	}
	assert.GreaterOrEqual(t, failCount, 3, "expected workspace, filesystem.roots, and exec.cwd to each fail")

	var jsonBuf bytes.Buffer
	require.NoError(t, printDoctorReport(&jsonBuf, rows, true))
	var decoded []Row
	require.NoError(t, json.Unmarshal(jsonBuf.Bytes(), &decoded))
	assert.Equal(t, rows, decoded)

	var tableBuf bytes.Buffer
	require.NoError(t, printDoctorReport(&tableBuf, rows, false))
	table := tableBuf.String()
	assert.Contains(t, table, "STATUS")
	assert.Contains(t, table, "CHECK")
	assert.Contains(t, table, "MESSAGE")
	assert.Contains(t, table, "FAIL")
}
