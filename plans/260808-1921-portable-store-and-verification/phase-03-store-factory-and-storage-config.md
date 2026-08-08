# Phase 3: Store Factory and Storage Config

**Status:** Completed · **Effort:** 4h · **Blocks:** phase 4 · **Blocked by:** phase 2

Add `store.Open` dispatching on `storage.driver`, introduce `storage.dsn` with
`storage.path` as a working deprecated alias, and rewire the three production
call sites so no package outside `internal/store` names a concrete backend.

## Context

- Phase 2 left `store.New(db, dialect)` plus `sqlite.Open(ctx, dsn, readOnly)`
  as the wiring path. This phase replaces both at the call sites with one
  factory entry point.
- The three production call sites, verified:
  - `internal/cli/root.go:53-66` — `openStore`, `sqlite.Open` at `:60`, `sqlite.New` at `:64`.
  - `internal/cli/doctor_checks.go:119-129` — `checkDatabase`, read-only then read-write retry at `:120-123`.
  - `internal/gateway/gateway.go:67-72` — `sqlite.Open` + `sqlite.New`.
- Current config surface: `StorageConfig{Path}` at `internal/config/types.go:215-218`;
  default `~/.mtclaw/mtclaw.db` at `internal/config/defaults.go:66-68`;
  path expansion at `internal/config/load.go:180-182`; parent-dir check at
  `internal/config/validate.go:287-292`.
- Other `Storage.Path` readers: `internal/cli/gateway_cmd.go:54` (a `db_path`
  log attribute).
- Tests that set `Storage.Path` directly:
  `internal/config/validate_test.go:27,291,301` and its error-text assertion at
  `:305`; `internal/cli/doctor_test.go:36,287`.
- Configs that write `storage.path` in YAML and must keep loading:
  `internal/provider/openai/e2e_test.go:40-42`, `internal/cli/doctor_test.go:287`,
  and phase 1's e2e helper.
- Docs coverage is mechanical: `internal/config/docs_coverage_test.go:65-82`
  fails if any new key is missing from `docs/configuration.md`.
- `onboard` serializes the whole `Config` struct with `yaml.Marshal`
  (`internal/cli/onboard_cmd.go:419`), so new keys appear in written configs
  automatically — and `Path` must be `omitempty` or every fresh config gets a
  stray deprecated key.

## Requirements

1. `store.Open(ctx, cfg config.StorageConfig, readOnly bool) (store.Store, error)`
   dispatching on `cfg.Driver`, with `sqlite` the only registered driver.
2. `storage.driver` (default `sqlite`) and `storage.dsn` (default
   `~/.mtclaw/mtclaw.db`) exist, are validated, and are documented.
3. `storage.path` keeps working, warns once, and errors only when combined
   with `storage.dsn`.
4. No package outside `internal/store` imports `internal/store/sqlite` except
   as a blank import.
5. `mtclaw doctor`'s DB check behaves as before (read-only, falling back to
   read-write) and names the DSN in its messages.

## Files

**Create**

| Path | Contents |
|---|---|
| `internal/store/factory.go` | `Open`, `Register`, the driver registry, and `ErrUnknownDriver`. |

**Modify**

| Path | Change |
|---|---|
| `internal/store/sqlite/dialect.go` | Add `func init() { store.Register("sqlite", open) }` where `open` adapts phase 2's exported `Open`. Keep `Open` exported for tests needing a raw `*sql.DB`. |
| `internal/config/types.go:215-218` | `StorageConfig{ Driver string \`yaml:"driver"\`; DSN string \`yaml:"dsn"\`; Path string \`yaml:"path,omitempty"\` }` + `func (s StorageConfig) EffectiveDSN() string`. |
| `internal/config/defaults.go:66-68` | `Driver: "sqlite"`, `DSN: "~/.mtclaw/mtclaw.db"`, `Path: ""`. |
| `internal/config/load.go:180-182` | Expand `DSN` and `Path` **only when `Driver == "sqlite"`**; then normalize `Path` into `DSN` and clear `Path`, emitting the deprecation warning once. |
| `internal/config/validate.go:287-292` | `validateStorage`: driver whitelist; both-keys-set error; parent-dir-creatable only for `sqlite`; error keys become `storage.dsn` / `storage.driver` / `storage.path`. |
| `internal/cli/root.go:53-66` | `openStore` calls `store.Open(ctx, s.cfg.Storage, readOnly)`; drop the `sqlite` import; add the blank import. |
| `internal/cli/doctor_checks.go:119-129` | Same, keeping the read-only→read-write retry and updating message text to say DSN. |
| `internal/gateway/gateway.go:67-72` | Same; blank import here too. |
| `internal/cli/gateway_cmd.go:54` | `"db_path"` → `"storage_dsn"`, value `s.cfg.Storage.EffectiveDSN()`. |
| `internal/config/validate_test.go:27,291,301,305` | Set `DSN` instead of `Path`; expected error prefix becomes `storage.dsn`. Add the new cases (see step 6). |
| `internal/cli/doctor_test.go:36,85,287` | `Storage.DSN`; `minimalYAML` keeps emitting `storage: path:` in **one** case on purpose, to prove the alias, and `dsn:` elsewhere. |
| `docs/configuration.md:127-131` | Rewrite the `storage` section: three rows (`driver`, `dsn`, `path` marked deprecated) with the both-set rule and the alias semantics. |

**Delete** — none.

## Design

### Factory + registry

`store.Open` cannot import `sqlite` (that package imports `store`), so
dispatch goes through a `database/sql`-style registry:

```go
// internal/store/factory.go
type Opener func(ctx context.Context, dsn string, readOnly bool) (*sql.DB, Dialect, bool, error)

func Register(driver string, open Opener)   // called from sqlite's init()
func Open(ctx context.Context, cfg config.StorageConfig, readOnly bool) (Store, error)
```

`Open` resolves the driver, calls the opener with `cfg.EffectiveDSN()`, runs
migrations (write mode) or verifies them (read mode), and wraps the result via
phase 2's `New`. Unknown driver →
`unknown storage driver %q (supported: sqlite)`.

The dependency direction is `sqlite → store → config`; `config` imports no
internal package. This is why `store` may take a `config.StorageConfig`
directly.

The cost of the registry is a runtime, not compile-time, failure if a wiring
site forgets `_ "github.com/tiennm99/MTClaw/internal/store/sqlite"`. There are
exactly three such sites, all listed above, plus test helpers. Mitigated by an
error message that names the fix and by a test asserting the unknown-driver
path.

### `EffectiveDSN` and normalization

```go
func (s StorageConfig) EffectiveDSN() string {
    if s.DSN != "" { return s.DSN }
    return s.Path
}
```

One read path for every consumer, so a hand-built `config.Config` in a test
(which never runs `Load`'s normalization) behaves identically to a loaded one.
`Load` additionally rewrites `Path` into `DSN` and clears `Path`, so
`mtclaw config show` renders the canonical key.

Rules, in `Validate`:

| `dsn` | `path` | Result |
|---|---|---|
| set | empty | use `dsn` |
| empty | set | use `path`; one-line stderr deprecation warning at load |
| set | set | **error**: `storage.dsn: set either storage.dsn or the deprecated storage.path, not both` |
| empty | empty | default (`Default()` fills `dsn`, so this only arises for hand-built structs) |

Path expansion (`~`, relative-to-config-dir) and the parent-directory check
apply only when `Driver == "sqlite"`, because a DSN is a file path for SQLite
and a URL for anything else. That two-line `switch` lives in
`internal/config` with a comment naming it as the place a second driver must
edit — deliberately, so `config` never imports `store` (plan R7).

## Implementation steps

1. **`internal/store/factory.go`.** Registry (`map[string]Opener` + `sync.RWMutex`,
   `Register` panicking on a duplicate name, as `sql.Register` does), `Open`,
   `ErrUnknownDriver`.
2. **Register in `sqlite`.** `init()` in `internal/store/sqlite/dialect.go`.
   Keep `sqlite.Open` exported — `migrate_test.go` needs a raw `*sql.DB`.
3. **Config struct + defaults + accessor.** `Driver`, `DSN`,
   `Path` (`omitempty`), `EffectiveDSN`. Doc comment on `Path` states it is
   deprecated, still honoured, and never silently combined with `DSN`.
4. **Load-time expansion and normalization** in `expandConfigPaths`
   (`load.go:150-187`): replace the single `expand(&cfg.Storage.Path)` with the
   driver-conditional expansion of both fields, then normalization. The
   deprecation warning goes to stderr via the same mechanism as
   `warnIfWorldReadable` (`load.go:134-145`) — one line, no logger dependency.
5. **`validateStorage`.** Driver whitelist first (an unknown driver makes the
   remaining checks meaningless), then the both-set rule, then the
   sqlite-only parent-dir check using `EffectiveDSN()`.
6. **Config tests.** Update the four `validate_test.go` sites and add:
   dsn-only loads; path-only loads and `EffectiveDSN()` equals it; both set
   fails naming both keys; `driver: postgres` fails naming the supported set;
   `driver: sqlite` with a non-creatable parent still fails as before.
7. **Rewire the three call sites.** `root.go`, `doctor_checks.go`,
   `gateway.go` — each loses its `sqlite` import and gains the blank import.
   Preserve `doctor`'s read-only-then-read-write retry exactly
   (`doctor_checks.go:120-123`) and its comment explaining why.
8. **Rewire the test helpers** phase 2 touched
   (`agent/loop_test.go`, `cron/scheduler_test.go`,
   `gateway/gateway_test_helpers_test.go`, `tools/exec_test.go`,
   `tools/registry_test.go`, `cli/doctor_test.go`) to
   `store.Open(ctx, config.StorageConfig{Driver: "sqlite", DSN: path}, false)`
   with the blank import. `internal/store/sqlite/*_test.go` keeps using
   `sqlite.Open` — it is testing that package.
9. **Docs.** Rewrite `docs/configuration.md`'s storage section. `storage.path`
   keeps a row (marked **deprecated**) because the docs-coverage test walks
   every struct field, and because users with existing configs need to find it.
10. **Verify the import boundary** with the greps in the success criteria, then
    run the full suite plus phase 1's e2e.

## Tests / validation

```powershell
go test -race ./internal/config/... ./internal/store/...
go test -race ./internal/cli/ ./internal/gateway/     # includes phase 1 e2e
go test -race ./...
$env:CGO_ENABLED=0; go build ./...

# import boundary
rg -n "store/sqlite" internal/ --glob '*.go' | rg -v "^internal/store/"
#   -> only lines of the form: _ "github.com/tiennm99/MTClaw/internal/store/sqlite"

# behavioural spot checks against a real config file
go run . --config .\testcfg.yaml config validate    # dsn form
go run . --config .\testcfg-path.yaml config validate  # path form: warns, passes
go run . --config .\testcfg-both.yaml config validate  # both: fails naming both keys
go run . --config .\testcfg-pg.yaml config validate    # driver: postgres -> fails
go run . --config .\testcfg.yaml doctor              # DB row names the DSN
go run . --config .\testcfg-path.yaml sessions list  # legacy config still opens the same DB
```

## Success criteria

- [ ] `store.Open(ctx, config.StorageConfig{Driver: "sqlite", DSN: tmp}, false)` returns a usable `store.Store`
- [ ] `Driver: "postgres"` returns an error naming the supported drivers
- [ ] `rg -n "store/sqlite" internal/ --glob '*.go'` outside `internal/store/` matches only blank imports
- [ ] `rg -n "modernc.org/sqlite" internal/ --glob '*.go'` matches only `internal/store/sqlite/`
- [ ] A config with only `storage.path` loads, warns once on stderr, and opens the same database file as before
- [ ] A config with both `storage.path` and `storage.dsn` fails validation naming both keys
- [ ] A config with `storage.driver: postgres` fails validation
- [ ] `mtclaw config show` on a `path`-only config renders `dsn` (normalization visible)
- [ ] `mtclaw onboard` writes a config containing `driver`/`dsn` and no `path` key
- [ ] `docs/configuration.md` documents `storage.driver`, `storage.dsn`, `storage.path` (deprecated); `go test ./internal/config/...` green
- [ ] `mtclaw doctor`'s DB check still tries read-only first and reports the DSN
- [ ] Phase 1 e2e suites unchanged and green; `go test -race ./...` green; `go.mod`/`go.sum` unchanged

## Risks and rollback

| Risk | Mitigation |
|---|---|
| Forgotten blank import → runtime "unknown storage driver" (plan R8) | Three sites, all enumerated; error text names the fix; grep check is an acceptance criterion; phase 1's e2e would fail immediately since it constructs a real gateway. |
| A user's existing config silently stops finding its database | `path` is honoured verbatim, expanded exactly as before (`load.go:180`); one manual check runs `sessions list` against a legacy config and expects the same rows. |
| `omitempty` on `Path` changes `config show` output for existing users | Intended and visible: `show` renders `dsn`. Documented in `docs/configuration.md`. |
| Both-set becomes an error for someone who set both harmlessly | Deliberate: silent preference is worse. Error text tells them which line to delete. Only reachable by hand-editing, since `Default()` sets only `dsn`. |
| `store` importing `config` is an unwanted coupling | One-way and acyclic (`sqlite → store → config`); `config` imports no internal package. Alternative (`Open(ctx, driver, dsn string, ...)`) was considered and dropped as noise at three call sites. |

**Rollback:** one commit. Reverting restores `storage.path` as canonical; any
config written by the new binary contains `driver`/`dsn`, which the old binary
rejects as unknown fields (`yaml.DisallowUnknownField`, `load.go:58`) — so a
rollback for a user who already ran the new `onboard` requires renaming
`dsn:` back to `path:` and deleting `driver:`. Note this explicitly in the
release notes; it is the one non-trivial downgrade cost in the plan.
