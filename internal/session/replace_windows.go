//go:build windows

package session

import (
	"errors"
	"fmt"
	"syscall"
)

// The temporary is created beside the target, so MoveFileExW is a same-volume
// atomic name replacement. WRITE_THROUGH is explicit, and COPY_ALLOWED is
// deliberately absent so this can never degrade into copy+delete.
func replaceTemporary(temporaryPath, targetPath string) (bool, error) {
	from, err := syscall.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return false, err
	}
	to, err := syscall.UTF16PtrFromString(targetPath)
	if err != nil {
		return false, err
	}
	ok, _, callErr := moveFileExW.Call(
		uintptr(unsafePointer(from)),
		uintptr(unsafePointer(to)),
		uintptr(sessionWindowsReplacementFlags()),
	)
	if ok == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			return false, errors.New("MoveFileExW replacement failed")
		}
		return false, fmt.Errorf("MoveFileExW replacement: %w", callErr)
	}
	return true, nil
}
