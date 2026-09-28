//go:build !linux

package tools

// DisableEnvironRead is a no-op outside Linux: PR_SET_DUMPABLE is a
// Linux-specific prctl option with no equivalent this package implements on
// other platforms - see exec_linux.go.
func DisableEnvironRead() error { return nil }
