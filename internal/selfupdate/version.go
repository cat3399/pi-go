package selfupdate

import (
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/product"
)

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
	base, valid := product.StableVersion(base)
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
		if _, ok := product.StableVersion(options.Version); !ok {
			return fmt.Errorf("version must be MAJOR.MINOR.PATCH (optionally prefixed with v)")
		}
	}
	return nil
}
