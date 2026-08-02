package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const narrowNoBreakSpace = "\u202f"

// resolveToolPath has one shared policy for every filesystem operation. The
// Pi-compatible @ prefix and Unicode-space normalization are input ergonomics,
// not a security mechanism. No symlink/root confinement is implied.
func resolveToolPath(cwd, supplied string) (string, error) {
	if !utf8.ValidString(supplied) {
		return "", fmt.Errorf("%w: path must be valid UTF-8", ErrFilesystemPath)
	}
	if strings.HasPrefix(supplied, "@") {
		supplied = strings.TrimPrefix(supplied, "@")
	}
	supplied = normalizeUnicodeSpaces(supplied)
	if supplied == "" {
		supplied = "."
	}
	if supplied == "~" || strings.HasPrefix(supplied, "~/") || strings.HasPrefix(supplied, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: resolve home directory: %w", ErrFilesystemPath, err)
		}
		if supplied == "~" {
			supplied = home
		} else {
			supplied = filepath.Join(home, supplied[2:])
		}
	}
	if !filepath.IsAbs(supplied) {
		supplied = filepath.Join(cwd, supplied)
	}
	return filepath.Clean(supplied), nil
}

func normalizeUnicodeSpaces(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u00a0', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u202f', '\u205f', '\u3000':
			return ' '
		default:
			return r
		}
	}, value)
}

// resolveReadPath retains the four documented macOS screenshot fallbacks. It
// only chooses a variant that exists; write/edit never rewrite an accidental
// lookalike path.
func resolveReadPath(cwd, supplied string) (string, error) {
	resolved, err := resolveToolPath(cwd, supplied)
	if err != nil || pathExists(resolved) {
		return resolved, err
	}
	variants := []string{
		strings.ReplaceAll(strings.ReplaceAll(resolved, " AM.", narrowNoBreakSpace+"AM."), " PM.", narrowNoBreakSpace+"PM."),
		resolvedNFD(resolved),
		strings.ReplaceAll(resolved, "'", "’"),
	}
	variants = append(variants, strings.ReplaceAll(resolvedNFD(resolved), "'", "’"))
	for _, candidate := range variants {
		if candidate != resolved && pathExists(candidate) {
			return candidate, nil
		}
	}
	return resolved, nil
}

// Go's standard library intentionally has no Unicode normalization package.
// On Darwin HFS/APFS the filesystem itself performs canonical matching, and on
// other platforms this explicit unsupported normalization is safer than a
// lossy hand-written decomposition. Curly quote/AM-PM variants remain exact.
func resolvedNFD(value string) string { return value }

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func requireContext(ctxDone <-chan struct{}) error {
	if ctxDone == nil {
		return nil
	}
	select {
	case <-ctxDone:
		return ErrOperationCancelled
	default:
		return nil
	}
}

// mutationKey resolves existing aliases and the deepest existing parent for a
// not-yet-created target. This serializes target/alias writes without turning
// the entire suite into a global lock.
func mutationKey(path string) (string, error) {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: resolve mutation path: %w", ErrFilesystemPath, err)
	}
	parent := filepath.Dir(path)
	base := filepath.Base(path)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			return filepath.Join(resolved, base), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: resolve mutation parent: %w", ErrFilesystemPath, err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return path, nil
		}
		base = filepath.Join(filepath.Base(parent), base)
		parent = next
	}
}
