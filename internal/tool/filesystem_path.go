package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const narrowNoBreakSpace = "\u202f"

var screenshotAMPM = regexp.MustCompile(` (?i:(am|pm))\.`)

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
		screenshotAMPM.ReplaceAllString(resolved, narrowNoBreakSpace+"${1}."),
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

func resolvedNFD(value string) string { return norm.NFD.String(value) }

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// mutationKey resolves existing aliases and the deepest existing parent for a
// not-yet-created target. This serializes target/alias writes without turning
// the entire suite into a global lock.
func mutationKey(path string) (string, error) {
	return resolveMutationDestination(filepath.Clean(path))
}
