package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func replaceExecutable(target, next string) error {
	// Windows allows a running image to be renamed, but not overwritten.
	// Reuse one old-file slot, removing the previous image before replacing
	// anything. If it is still running, fail instead of accumulating copies.
	backup := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".old")
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove previous executable at %s; close processes using it and retry: %w", backup, err)
	}
	if err := moveExecutable(target, backup); err != nil {
		return fmt.Errorf("back up running executable: %w", err)
	}
	if err := moveExecutable(next, target); err != nil {
		if restoreErr := moveExecutable(backup, target); restoreErr != nil {
			return fmt.Errorf("replace failed; restore executable from %s: %w", backup, errors.Join(err, restoreErr))
		}
		return fmt.Errorf("replace failed; original executable restored: %w", err)
	}
	// A running old image cannot be deleted yet. It stays in the fixed slot
	// until the next update; an image that is no longer running is removed now.
	_ = os.Remove(backup)
	return nil
}

func moveExecutable(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_WRITE_THROUGH)
}
