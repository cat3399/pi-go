//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package installation

import (
	"errors"
	"os"
)

func tryLock(*os.File) (bool, error) {
	return false, errors.New("installation locking is unavailable on this platform")
}
func unlock(*os.File) {}
func syncDirectory(string) error {
	return errors.New("installation directory sync is unavailable on this platform")
}
