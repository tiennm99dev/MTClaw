package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrPathEmpty is returned by Resolve for an empty user-supplied path.
var ErrPathEmpty = errors.New("tools: path must not be empty")

// Resolve confines userPath to one of roots, resolving symlinks rather than
// trusting string prefixes. This is the one primitive every filesystem tool
// (read_file, write_file, list_dir) must route through before touching
// disk.
//
// Algorithm:
//  1. reject an empty path or one containing a NUL byte outright.
//  2. expand a leading "~" against the user's home directory.
//  3. make the path absolute, joining against roots[0] when it is relative.
//  4. resolve symlinks on the deepest existing ancestor (filepath.EvalSymlinks
//     cannot run on a path that does not exist yet - a new file write_file is
//     about to create), then rejoin the non-existent tail verbatim.
//  5. require the resolved path to be root itself or a descendant of it,
//     checked with filepath.Rel rather than strings.HasPrefix so that
//     "/data-evil" is never accepted as inside "/data".
//
// Comparison is case-insensitive on Windows and macOS, matching those
// filesystems' own case-insensitive-by-default semantics.
func Resolve(roots []string, userPath string) (string, error) {
	if userPath == "" {
		return "", ErrPathEmpty
	}
	if strings.ContainsRune(userPath, 0) {
		return "", fmt.Errorf("tools: path %q contains a NUL byte", userPath)
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("tools: no filesystem roots configured")
	}

	expanded, err := expandTilde(userPath)
	if err != nil {
		return "", err
	}

	abs := expanded
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(roots[0], abs)
	}
	abs = filepath.Clean(abs)

	resolved, err := resolveDeepestSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("tools: resolve %q: %w", userPath, err)
	}

	for _, root := range roots {
		realRoot := resolveRootBestEffort(root)
		if withinRoot(resolved, realRoot) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("tools: path %q escapes the configured filesystem roots", userPath)
}

// expandTilde replaces a leading "~" or "~/..." with the user's home
// directory. A path that does not start with "~" is returned unchanged.
func expandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("tools: expand ~: %w", err)
	}
	rest := strings.TrimPrefix(p, "~")
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.TrimPrefix(rest, `\`)
	return filepath.Join(home, rest), nil
}

// resolveDeepestSymlinks resolves symlinks on the deepest existing ancestor
// of abs and rejoins whatever tail does not exist yet, so a write_file call
// targeting a brand-new file still resolves through any symlinked parent
// directory instead of failing outright.
func resolveDeepestSymlinks(abs string) (string, error) {
	existing := abs
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			// Reached the filesystem root without finding an existing
			// ancestor; resolve whatever is left as-is.
			break
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		existing = parent
	}

	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	if len(tail) > 0 {
		real = filepath.Join(append([]string{real}, tail...)...)
	}
	return real, nil
}

// resolveRootBestEffort resolves root's own symlinks when possible, falling
// back to the cleaned path unresolved when root does not exist (a config
// error surfaced elsewhere, not this function's job to fail on).
func resolveRootBestEffort(root string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		return real
	}
	return filepath.Clean(root)
}

// withinRoot reports whether path is root itself or a descendant of it,
// using filepath.Rel so a lookalike sibling directory (root "/data",
// candidate "/data-evil") is correctly rejected instead of accepted by a
// naive string-prefix check.
func withinRoot(path, root string) bool {
	p, r := path, root
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p, r = strings.ToLower(p), strings.ToLower(r)
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
