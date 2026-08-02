package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// A writer claim uses both a canonical path and the filesystem identity. The
// path catches aliases before a file exists; os.SameFile catches hard links,
// symlinks, and platform-specific aliases once it does.
type writerClaim struct {
	paths  map[string]struct{}
	info   os.FileInfo
	unlock func()
}

var activeSessionWriters = struct {
	sync.Mutex
	paths  map[string]*writerClaim
	claims map[*writerClaim]struct{}
}{
	paths:  make(map[string]*writerClaim),
	claims: make(map[*writerClaim]struct{}),
}

func claimSessionWriter(path string) (*writerClaim, error) {
	key, info, err := sessionWriterDescriptor(path)
	if err != nil {
		return nil, fmt.Errorf("%w: identify writer path %s: %v", ErrStorage, path, err)
	}

	activeSessionWriters.Lock()
	defer activeSessionWriters.Unlock()
	if conflict := conflictingWriterLocked(nil, key, info); conflict != nil {
		return nil, fmt.Errorf("%w: %s", ErrWriterActive, path)
	}
	unlock, err := claimProcessWriter(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrWriterActive, path)
	}
	claim := &writerClaim{paths: map[string]struct{}{key: {}}, info: info, unlock: unlock}
	activeSessionWriters.paths[key] = claim
	activeSessionWriters.claims[claim] = struct{}{}
	return claim, nil
}

func refreshSessionWriter(claim *writerClaim, path string) error {
	key, info, err := sessionWriterDescriptor(path)
	if err != nil {
		return fmt.Errorf("%w: identify writer path %s: %v", ErrStorage, path, err)
	}

	activeSessionWriters.Lock()
	defer activeSessionWriters.Unlock()
	if _, active := activeSessionWriters.claims[claim]; !active {
		return fmt.Errorf("%w: writer claim for %s is inactive", ErrStorage, path)
	}
	if claim.info != nil && (info == nil || !os.SameFile(claim.info, info)) {
		return fmt.Errorf("%w: session file changed while opening %s", ErrStorage, path)
	}
	if conflict := conflictingWriterLocked(claim, key, info); conflict != nil {
		return fmt.Errorf("%w: %s", ErrWriterActive, path)
	}
	claim.paths[key] = struct{}{}
	activeSessionWriters.paths[key] = claim
	if info != nil {
		claim.info = info
	}
	return nil
}

// refreshSessionWriterAfterRewrite adopts the new inode produced by this
// claim's own atomic replacement. It is intentionally narrower than refresh:
// a normal Open must reject a changed file, while a successful migration has
// just made exactly that change under the same process lock.
func refreshSessionWriterAfterRewrite(claim *writerClaim, path string) error {
	key, info, err := sessionWriterDescriptor(path)
	if err != nil {
		return fmt.Errorf("%w: identify rewritten session %s: %v", ErrStorage, path, err)
	}
	if info == nil {
		return fmt.Errorf("%w: rewritten session disappeared: %s", ErrStorage, path)
	}
	activeSessionWriters.Lock()
	defer activeSessionWriters.Unlock()
	if _, active := activeSessionWriters.claims[claim]; !active {
		return fmt.Errorf("%w: writer claim for %s is inactive", ErrStorage, path)
	}
	if conflict := conflictingWriterLocked(claim, key, info); conflict != nil {
		return fmt.Errorf("%w: %s", ErrWriterActive, path)
	}
	claim.paths[key] = struct{}{}
	activeSessionWriters.paths[key] = claim
	claim.info = info
	return nil
}

func releaseSessionWriter(claim *writerClaim) {
	if claim == nil {
		return
	}
	activeSessionWriters.Lock()
	if _, active := activeSessionWriters.claims[claim]; active {
		for key := range claim.paths {
			if activeSessionWriters.paths[key] == claim {
				delete(activeSessionWriters.paths, key)
			}
		}
		delete(activeSessionWriters.claims, claim)
		if claim.unlock != nil {
			claim.unlock()
			claim.unlock = nil
		}
	}
	activeSessionWriters.Unlock()
}

func conflictingWriterLocked(owner *writerClaim, key string, info os.FileInfo) *writerClaim {
	if claim := activeSessionWriters.paths[key]; claim != nil && claim != owner {
		return claim
	}
	if info == nil {
		return nil
	}
	for claim := range activeSessionWriters.claims {
		if claim != owner && claim.info != nil && os.SameFile(info, claim.info) {
			return claim
		}
	}
	return nil
}

func sessionWriterDescriptor(path string) (string, os.FileInfo, error) {
	canonical := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonical = resolved
	} else if resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(path)); parentErr == nil {
		canonical = filepath.Join(resolvedParent, filepath.Base(path))
	}
	canonical = filepath.Clean(canonical)
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "path:" + canonical, nil, nil
		}
		return "", nil, err
	}
	return "path:" + canonical, info, nil
}
