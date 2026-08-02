//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
)

var (
	lockFileEx   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	unlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

func claimProcessPathWriter(path string) (func(), error) {
	file, err := os.OpenFile(path+".pi-go.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		}
		return nil, err
	}
	return claimWindowsWriterFile(file)
}

func claimProcessIdentityWriter(path string) (func(), error) {
	unlock, _, err := claimProcessIdentityWriterWithInfo(path)
	return unlock, err
}

func claimProcessIdentityWriterWithInfo(path string) (func(), os.FileInfo, error) {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	spec := sessionWindowsIdentityHandleSpec()
	handle, err := syscall.CreateFile(
		pathPointer,
		spec.DesiredAccess,
		spec.ShareMode,
		nil,
		spec.CreationDisposition,
		spec.FlagsAndAttributes,
		0,
	)
	if err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("CreateFileW session identity: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = syscall.CloseHandle(handle)
		return nil, nil, errors.New("wrap Windows session identity handle")
	}
	unlock, err := claimWindowsWriterFile(file)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		unlock()
		return nil, nil, fmt.Errorf("stat Windows session identity handle: %w", err)
	}
	return unlock, info, nil
}

func claimWindowsWriterFile(file *os.File) (func(), error) {
	return claimWindowsWriterHandle(syscall.Handle(file.Fd()), file.Close)
}

func claimWindowsWriterHandle(handle syscall.Handle, closeHandle func() error) (func(), error) {
	lockRange := sessionWindowsIdentityLockRange()
	overlapped := syscall.Overlapped{
		Offset:     lockRange.OffsetLow,
		OffsetHigh: lockRange.OffsetHigh,
	}
	ok, _, callErr := lockFileEx.Call(
		uintptr(handle), lockfileExclusiveLock|lockfileFailImmediately, 0,
		uintptr(lockRange.LengthLow), uintptr(lockRange.LengthHigh),
		uintptr(unsafePointer(&overlapped)),
	)
	if ok == 0 {
		closeErr := closeHandle()
		if callErr == nil || callErr == syscall.Errno(0) {
			return nil, errors.Join(errors.New("LockFileEx failed"), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("LockFileEx: %w", callErr), closeErr)
	}
	return func() {
		_, _, _ = unlockFileEx.Call(
			uintptr(handle), 0,
			uintptr(lockRange.LengthLow), uintptr(lockRange.LengthHigh),
			uintptr(unsafePointer(&overlapped)),
		)
		_ = closeHandle()
	}, nil
}
