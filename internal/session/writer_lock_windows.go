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
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
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
			return nil, nil
		}
		return nil, fmt.Errorf("CreateFileW session identity: %w", err)
	}
	return claimWindowsWriterHandle(handle, func() error { return syscall.CloseHandle(handle) })
}

func claimWindowsWriterFile(file *os.File) (func(), error) {
	return claimWindowsWriterHandle(syscall.Handle(file.Fd()), file.Close)
}

func claimWindowsWriterHandle(handle syscall.Handle, closeHandle func() error) (func(), error) {
	overlapped := syscall.Overlapped{}
	ok, _, callErr := lockFileEx.Call(
		uintptr(handle), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
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
		_, _, _ = unlockFileEx.Call(uintptr(handle), 0, 1, 0, uintptr(unsafePointer(&overlapped)))
		_ = closeHandle()
	}, nil
}
