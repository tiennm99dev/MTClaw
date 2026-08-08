package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tiennm99/MTClaw/internal/config"
)

// Opener is what a backend package registers via Register: given a DSN
// (backend-specific shape - a filesystem path for sqlite) and a read-only
// flag, it opens the database, applies whatever pool/pragma setup that
// backend needs, and runs (write mode) or verifies (read-only mode)
// migrations via Migrate - exactly what internal/store/sqlite.Open already
// does. The returned bool reports which mode actually won (a backend may
// transparently fall back to read-write, as sqlite's readonly-recovery
// handling does), mirroring Open's own second return value.
type Opener func(ctx context.Context, dsn string, readOnly bool) (*sql.DB, Dialect, bool, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Opener{}
)

// ErrUnknownDriver is wrapped by Open's error when cfg.Driver names no
// registered backend. The most common cause is not a bad config value but a
// forgotten blank import: a backend package registers itself from its own
// init() (see internal/store/sqlite/dialect.go), and store cannot import
// that package directly without an import cycle (sqlite already imports
// store) - see plan.md's A6/R8. Open's error message names the supported
// set precisely so this is diagnosable without reading source.
var ErrUnknownDriver = errors.New("store: unknown storage driver")

// Register makes driver available to Open. Backend packages call this from
// their own init(), following database/sql.Register's own pattern -
// including its choice to panic, not return an error, on a duplicate name:
// two backends racing to claim the same driver string is a build-time
// mistake (a copy-pasted package, a typo'd Name()), never a runtime
// condition a caller could sensibly recover from.
func Register(driver string, open Opener) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[driver]; dup {
		panic("store: Register called twice for driver " + driver)
	}
	registry[driver] = open
}

// Open resolves cfg.Driver in the registry, opens cfg.EffectiveDSN()
// through the registered backend's Opener, and wraps the result as a Store
// via New. readOnly is forwarded verbatim to the backend - see Opener's
// doc comment for what that means in practice.
//
// store deliberately takes a config.StorageConfig rather than a bare
// (driver, dsn string) pair: EffectiveDSN() is the one read path every
// consumer must use to see a legacy storage.path-only config the same way
// a freshly-loaded storage.dsn config looks, and duplicating that logic at
// every one of this function's call sites would be exactly the kind of
// drift the seam exists to prevent. This makes store import config - a
// one-way, acyclic dependency (sqlite -> store -> config; config imports
// nothing internal) - see plan.md's Risks table for why the reverse
// (config importing store, to keep the alias logic in one place without
// this import) was rejected instead.
func Open(ctx context.Context, cfg config.StorageConfig, readOnly bool) (Store, error) {
	open, ok, supported := lookup(cfg.Driver)
	if !ok {
		return nil, fmt.Errorf("%w %q (supported: %s)", ErrUnknownDriver, cfg.Driver, supported)
	}

	db, dia, _, err := open(ctx, cfg.EffectiveDSN(), readOnly)
	if err != nil {
		return nil, err
	}
	return New(db, dia), nil
}

// lookup resolves driver under a single lock acquisition, returning the
// sorted, comma-joined set of every registered driver alongside the
// (Opener, found) pair so Open's error path never has to take the lock a
// second time to describe what is available.
func lookup(driver string) (Opener, bool, string) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	open, ok := registry[driver]

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)

	return open, ok, strings.Join(names, ", ")
}
