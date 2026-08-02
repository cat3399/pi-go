package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const defaultLockPoll = 20 * time.Millisecond

// acquireFileLock uses mkdir because directory creation is atomic on the
// supported local filesystems and works on Windows without a syscall-specific
// advisory-lock API. A crash can leave a lock directory behind; it is never
// reclaimed automatically because a stale heuristic could corrupt credentials
// on a slow or suspended process. Operators get a typed lock error instead.
func (s *Store) acquireFileLock(ctx context.Context) (func(), error) {
	lockPath := s.path + ".lock"
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, failure(KindIO, "acquire lock", "", err)
		}
		if cause := context.Cause(ctx); cause != nil {
			return nil, failure(KindCancelled, "acquire lock", "", cause)
		}
		if s.lockPoll != nil {
			if err := s.lockPoll(ctx); err != nil {
				if cause := context.Cause(ctx); cause != nil {
					return nil, failure(KindCancelled, "acquire lock", "", cause)
				}
				return nil, failure(KindLock, "acquire lock", "", err)
			}
			continue
		}
		timer := time.NewTimer(defaultLockPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, failure(KindCancelled, "acquire lock", "", context.Cause(ctx))
		case <-timer.C:
		}
	}
}

func lockPathParent(path string) string { return filepath.Dir(path) }
