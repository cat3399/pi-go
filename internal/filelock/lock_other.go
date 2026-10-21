//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package filelock

import (
	"errors"
	"os"
)

func tryLock(*os.File) (bool, error) {
	return false, errors.New("installation locking is unavailable on this platform")
}
func unlock(*os.File) {}
