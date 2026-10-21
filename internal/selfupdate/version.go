package selfupdate

import (
	"fmt"
	"strings"
)

// Releases use exactly vMAJOR.MINOR.PATCH, matching scripts/latest-version.sh.
// Comparing decimal strings avoids integer overflow for valid SemVer components.
func stableVersion(value string) (string, bool) {
	value = strings.TrimPrefix(value, "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return "", false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", false
			}
		}
	}
	return value, true
}

func compareStable(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := range left {
		if len(left[i]) < len(right[i]) {
			return -1
		}
		if len(left[i]) > len(right[i]) {
			return 1
		}
		if cmp := strings.Compare(left[i], right[i]); cmp != 0 {
			return cmp
		}
	}
	return 0
}

// compareCurrent compares a stable release with the installed stable/dev build.
func compareCurrent(target, current string) (int, bool) {
	current, _, _ = strings.Cut(strings.TrimPrefix(current, "v"), "+")
	base, _, prerelease := strings.Cut(current, "-")
	base, valid := stableVersion(base)
	if !valid {
		return 0, false
	}
	cmp := compareStable(target, base)
	if cmp == 0 && prerelease {
		cmp = 1
	}
	return cmp, true
}

// ValidateOptions runs before any network or filesystem operation.
func ValidateOptions(options Options) error {
	if options.Version != "" {
		if _, ok := stableVersion(options.Version); !ok {
			return fmt.Errorf("version must be MAJOR.MINOR.PATCH (optionally prefixed with v)")
		}
	}
	return nil
}
