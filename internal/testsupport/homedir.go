// Package testsupport holds test-only helpers shared across more than one
// package's test suite (internal/cli, internal/gateway, and this package's
// own fakeapi subpackage). It is imported exclusively from _test.go files;
// nothing in a non-test build ever depends on it.
package testsupport

import "runtime"

// TB is the minimal subset of *testing.T FakeHome needs. It is hand-rolled
// instead of taking *testing.T directly so this package - itself imported
// only from other packages' tests - never imports the standard "testing"
// package: a non-test package importing "testing" pulls testing's own flag
// registration into every binary that imports this one transitively, which
// is exactly the kind of thing a shared test helper must not do.
type TB interface {
	Helper()
	TempDir() string
	Setenv(key, value string)
}

// FakeHome points os.UserHomeDir() (HOME on POSIX, USERPROFILE on Windows)
// at a fresh temp directory for the duration of one test, and returns that
// directory. internal/config.StateDir and every "~/..." default in
// config.Default() resolve through the home directory, and gateway.New
// acquires its instance lock under it (internal/gateway/gateway.go) - so
// without this, any test that builds a real Gateway or runs `onboard` would
// create a real ~/.mtclaw (database, lock file, starter AGENTS.md) on
// whatever machine runs the suite.
//
// This is the single implementation internal/cli/onboard_test.go's
// useFakeHome and internal/gateway's and internal/cli's e2e helpers all
// delegate to.
//
// tb.Setenv forbids t.Parallel() for the remainder of the calling test (see
// testing.T.Setenv's own doc comment) - every caller of FakeHome is
// therefore serialized with any other test that also calls it. This is
// accepted, not overlooked: e2e suites are few and each runs in well under a
// second, so serializing them costs nothing worth working around (see plan
// risk R11 in
// plans/260808-1921-portable-store-and-verification/plan.md).
func FakeHome(tb TB) string {
	tb.Helper()
	home := tb.TempDir()
	if runtime.GOOS == "windows" {
		tb.Setenv("USERPROFILE", home)
	} else {
		tb.Setenv("HOME", home)
	}
	return home
}
