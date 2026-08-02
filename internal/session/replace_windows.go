//go:build windows

package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
)

var setFileInformationByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

// replaceTemporary renames an already-synced sibling temporary over target
// while target's identity-lock handle remains open. FileRenameInfoEx with POSIX
// semantics is the only accepted Windows replacement primitive: traditional
// MoveFileExW cannot replace this open destination, and no copy fallback is
// atomic. The source handle's WRITE_THROUGH flag flushes rename metadata; a
// subsequent FlushFileBuffers makes any post-publication durability failure
// explicit to the caller.
func replaceTemporary(temporaryPath, targetPath string) (replaced bool, returnErr error) {
	temporaryPath, err := filepath.Abs(temporaryPath)
	if err != nil {
		return false, fmt.Errorf("resolve Windows replacement temporary: %w", err)
	}
	targetPath, err = filepath.Abs(targetPath)
	if err != nil {
		return false, fmt.Errorf("resolve Windows replacement target: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(filepath.Dir(temporaryPath)), filepath.Clean(filepath.Dir(targetPath))) {
		return false, fmt.Errorf("Windows atomic replacement requires a same-directory temporary")
	}
	if err := setFileInformationByHandle.Find(); err != nil {
		return false, fmt.Errorf("%w: SetFileInformationByHandle unavailable: %v", ErrAtomicReplaceUnsupported, err)
	}

	temporaryPointer, err := syscall.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return false, fmt.Errorf("encode Windows replacement temporary: %w", err)
	}
	spec := sessionWindowsReplacementHandleSpec()
	handle, err := syscall.CreateFile(
		temporaryPointer,
		spec.DesiredAccess,
		spec.ShareMode,
		nil,
		spec.CreationDisposition,
		spec.FlagsAndAttributes,
		0,
	)
	if err != nil {
		return false, fmt.Errorf("CreateFileW replacement temporary: %w", err)
	}
	handleOpen := true
	defer func() {
		if !handleOpen {
			return
		}
		if closeErr := syscall.CloseHandle(handle); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close Windows replacement temporary: %w", closeErr))
		}
	}()

	targetUTF16, err := syscall.UTF16FromString(targetPath)
	if err != nil {
		return false, fmt.Errorf("encode Windows replacement target: %w", err)
	}
	targetUTF16 = targetUTF16[:len(targetUTF16)-1]
	renameInfo, err := buildWindowsRenameInfoEx(targetUTF16, windowsNativePointerSize())
	if err != nil {
		return false, err
	}
	ok, _, callErr := setFileInformationByHandle.Call(
		uintptr(handle),
		uintptr(windowsFileRenameInfoEx),
		uintptr(unsafePointer(&renameInfo[0])),
		uintptr(uint32(len(renameInfo))),
	)
	if ok == 0 {
		callErr = windowsCallError("SetFileInformationByHandle(FileRenameInfoEx)", callErr)
		var errno syscall.Errno
		if errors.As(callErr, &errno) && isWindowsAtomicRenameUnsupportedCode(uint32(errno)) {
			return false, fmt.Errorf("%w: %v", ErrAtomicReplaceUnsupported, callErr)
		}
		return false, callErr
	}
	replaced = true

	if err := syscall.FlushFileBuffers(handle); err != nil {
		return true, fmt.Errorf("FlushFileBuffers after Windows replacement: %w", err)
	}
	if err := syscall.CloseHandle(handle); err != nil {
		handleOpen = false
		return true, fmt.Errorf("close Windows replacement temporary: %w", err)
	}
	handleOpen = false
	return true, nil
}

func windowsCallError(operation string, callErr error) error {
	if callErr == nil || callErr == syscall.Errno(0) {
		return fmt.Errorf("%s failed", operation)
	}
	return fmt.Errorf("%s: %w", operation, callErr)
}
