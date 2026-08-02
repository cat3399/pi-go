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

func claimProcessWriter(path string) (func(), error) {
	file, err := os.OpenFile(path+".pi-go.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		}
		return nil, err
	}
	overlapped := syscall.Overlapped{}
	ok, _, callErr := lockFileEx.Call(
		file.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
		uintptr(unsafePointer(&overlapped)),
	)
	if ok == 0 {
		_ = file.Close()
		if callErr == nil || callErr == syscall.Errno(0) {
			return nil, errors.New("LockFileEx failed")
		}
		return nil, fmt.Errorf("LockFileEx: %w", callErr)
	}
	return func() {
		_, _, _ = unlockFileEx.Call(file.Fd(), 0, 1, 0, uintptr(unsafePointer(&overlapped)))
		_ = file.Close()
	}, nil
}
