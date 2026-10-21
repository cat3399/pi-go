package selfupdate

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func replaceExecutable(target, next, backup string) error {
	// Windows allows a running image to be renamed, but not overwritten.
	// Backup names are unique, so another process may still be running any
	// previous image. Roll back a failed second move before releasing the lock.
	if err := moveExecutable(target, backup); err != nil {
		return fmt.Errorf("back up running executable: %w", err)
	}
	if err := moveExecutable(next, target); err != nil {
		if restoreErr := moveExecutable(backup, target); restoreErr != nil {
			return fmt.Errorf("replace failed; restore executable from %s: %w", backup, errors.Join(err, restoreErr))
		}
		return fmt.Errorf("replace failed; original executable restored: %w", err)
	}
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
