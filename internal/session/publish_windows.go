//go:build windows

package session

import (
	"errors"
	"fmt"
	"syscall"
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func publishTemporary(temporaryPath, targetPath string) (bool, error) {
	from, err := syscall.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return false, err
	}
	to, err := syscall.UTF16PtrFromString(targetPath)
	if err != nil {
		return false, err
	}
	// Omitting MOVEFILE_REPLACE_EXISTING makes publication no-clobber.
	// WRITE_THROUGH waits for the same-volume move to reach disk before return.
	ok, _, callErr := moveFileExW.Call(
		uintptr(unsafePointer(from)),
		uintptr(unsafePointer(to)),
		uintptr(sessionWindowsCreatePublishFlags()),
	)
	if ok == 0 {
		if callErr == nil {
			return false, errors.New("MoveFileExW failed")
		}
		if errno, ok := callErr.(syscall.Errno); ok && errno == 0 {
			return false, errors.New("MoveFileExW failed")
		}
		return false, fmt.Errorf("MoveFileExW: %w", callErr)
	}
	return true, nil
}
