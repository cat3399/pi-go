//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func publishTemporary(temporaryPath, targetPath string) (bool, error) {
	// A same-directory hard link atomically publishes the already-synced inode
	// without the overwrite behavior of os.Rename.
	if err := os.Link(temporaryPath, targetPath); err != nil {
		return false, err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return true, fmt.Errorf("remove published session temporary: %w", err)
	}
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return true, err
	}
	return true, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open session directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync session directory: %w", err), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close session directory after sync: %w", err)
	}
	return nil
}
