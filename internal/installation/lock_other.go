//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package installation

import "errors"

func syncDirectory(string) error {
	return errors.New("installation directory sync is unavailable on this platform")
}
