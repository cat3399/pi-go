//go:build darwin || linux

package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func replaceExecutable(target, next, backup string) error {
	if err := backupExecutable(target, backup); err != nil {
		return fmt.Errorf("back up executable: %w", err)
	}
	if err := syncDirectory(filepath.Dir(backup)); err != nil {
		return err
	}
	if err := os.Rename(next, target); err != nil {
		return fmt.Errorf("replace executable (original retained, backup at %s): %w", backup, err)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		// Preserve the new artifact as well as restoring the original.
		failed := filepath.Join(filepath.Dir(backup), "uncommitted")
		if saveErr := backupExecutable(target, failed); saveErr != nil {
			return fmt.Errorf("new executable installed but directory sync failed; original at %s: %w", backup, errors.Join(err, saveErr))
		}
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return fmt.Errorf("restore executable from %s: %w", backup, errors.Join(err, restoreErr))
		}
		return fmt.Errorf("directory sync failed; original executable restored: %w", errors.Join(err, syncDirectory(filepath.Dir(target))))
	}
	return nil
}

func backupExecutable(target, backup string) error {
	if err := os.Link(target, backup); err == nil {
		return nil
	}
	// Some installation filesystems cannot create hard links. A synced copy
	// still leaves the current executable in place until the atomic rename.
	input, err := os.Open(target)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer output.Close()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	return output.Close()
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
