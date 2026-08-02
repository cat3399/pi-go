//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package session

import (
	"fmt"
	"os"
	"path/filepath"
)

func replaceTemporary(temporaryPath, targetPath string) (bool, error) {
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return false, err
	}
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return true, fmt.Errorf("sync rewritten session directory: %w", err)
	}
	return true, nil
}
