//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package session

import (
	"errors"
	"os"
	"syscall"
)

// A flock is released by the kernel on process exit, unlike a mkdir sentinel.
// The stable sidecar contains no user session data and is intentionally kept so
// reopening never needs a racy delete/recreate sequence.
func claimProcessPathWriter(path string) (func(), error) {
	file, err := os.OpenFile(path+".pi-go.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// Create owns the more useful parent-directory diagnostic. There can be
		// no competing session writer beneath a directory that does not exist.
		if errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		}
		return nil, err
	}
	return claimUnixWriterFile(file)
}

func claimProcessIdentityWriter(path string) (func(), error) {
	unlock, _, err := claimProcessIdentityWriterWithInfo(path)
	return unlock, err
}

func claimProcessIdentityWriterWithInfo(path string) (func(), os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	unlock, err := claimUnixWriterFile(file)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return unlock, info, nil
}

func claimUnixWriterFile(file *os.File) (func(), error) {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
