// Package version holds build-stamped identity for the mtclaw binary.
// Version is overwritten at build time via -ldflags (see Makefile) for a
// release build; Commit and Date are filled in automatically from
// runtime/debug.ReadBuildInfo's VCS stamping whenever the binary was built
// with `go build` run directly inside a git checkout (the release workflow
// qualifies), so only the tag name itself needs an explicit -X flag there.
// `go run` and `go install .../MTClaw@vX` do not qualify: `go run` reports
// no VCS settings at all (verified: it prints "commit none, committed
// unknown"), and a module-cache install has no `.git` directory for
// ReadBuildInfo to read VCS settings from in the first place - see
// README.md for the accurate version of this list.
package version

import (
	"fmt"
	"runtime/debug"
	"sync"
)

var (
	// Version is the release tag, resolved in this order: an -ldflags -X
	// override, then runtime/debug.ReadBuildInfo's own module version (set
	// by `go install .../MTClaw@vX`), then "dev".
	Version = "dev"
	// Commit is the short git SHA the binary was built from, resolved from
	// ReadBuildInfo's vcs.revision when not overridden by -ldflags.
	Commit = "none"
	// Date is the commit timestamp (RFC3339), resolved from ReadBuildInfo's
	// vcs.time when not overridden by -ldflags. Using the commit time
	// rather than build wall-clock time means two builds of the same commit
	// report the same Date, which is what makes the release workflow's
	// SHA256SUMS reproducible across a rebuild.
	Date = "unknown"
)

// commitDisplayLen bounds how much of a full VCS revision String renders,
// matching the short-SHA length `git rev-parse --short` produces.
const commitDisplayLen = 12

var resolveOnce sync.Once

// resolve fills in whichever of Version/Commit/Date -ldflags left at its
// default, from runtime/debug.ReadBuildInfo. It is a no-op (and returns
// immediately on every call after the first) when built without VCS
// information available at all - a tarball source build with no .git
// directory, for instance - in which case the -ldflags value (or the
// package's own "dev"/"none"/"unknown" default) stands as printed.
func resolve() {
	resolveOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}

		if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			Version = info.Main.Version
		}

		var revision, commitTime string
		var modified bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				commitTime = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}

		if Commit == "none" && revision != "" {
			commit := revision
			if len(commit) > commitDisplayLen {
				commit = commit[:commitDisplayLen]
			}
			if modified {
				commit += "-dirty"
			}
			Commit = commit
		}
		if Date == "unknown" && commitTime != "" {
			Date = commitTime
		}
	})
}

// String renders the version info the `mtclaw version` command prints.
func String() string {
	resolve()
	return fmt.Sprintf("mtclaw %s (commit %s, committed %s)", Version, Commit, Date)
}
