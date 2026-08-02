//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package session

import (
	"os"
	"path/filepath"
)

func replaceTemporary(temporaryPath, targetPath string) (bool, error) {
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return false, err
	}
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return true, err
	}
	return true, nil
}
