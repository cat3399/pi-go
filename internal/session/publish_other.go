//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func publishTemporary(temporaryPath, targetPath string) (bool, error) {
	if err := os.Link(temporaryPath, targetPath); err != nil {
		return false, fmt.Errorf("atomic no-replace session publication is unsupported: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return true, err
	}
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return true, err
	}
	return true, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
