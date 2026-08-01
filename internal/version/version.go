// Package version holds build-stamped identity for the mtclaw binary. The
// three vars are overwritten at build time via -ldflags (see Makefile); the
// zero values below are what a plain `go build` or `go run` produces.
package version

import "fmt"

var (
	// Version is the release tag or "dev" for an unstamped build.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// Date is the build timestamp (RFC3339 or "unknown").
	Date = "unknown"
)

// String renders the version info the `mtclaw version` command prints.
func String() string {
	return fmt.Sprintf("mtclaw %s (commit %s, built %s)", Version, Commit, Date)
}
