package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// A writer claim uses both a canonical path and kernel locks on every file
// identity it has owned. The path lock survives atomic replacement; identity
// locks make hard-link aliases conflict even when their path sidecars differ.
type writerClaim struct {
	paths   map[string]struct{}
	info    os.FileInfo
	unlocks []func()
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
	key, lockPath, info, err := sessionWriterDescriptor(path)
	if err != nil {
		return nil, fmt.Errorf("%w: identify writer path %s: %v", ErrStorage, path, err)
	}

	activeSessionWriters.Lock()
	defer activeSessionWriters.Unlock()
	if conflict := conflictingWriterLocked(nil, key, info); conflict != nil {
		return nil, fmt.Errorf("%w: %s", ErrWriterActive, path)
	}
	pathUnlock, err := claimProcessPathWriter(lockPath)
	if err != nil {
		return nil, fmt.Errorf("%w: acquire path lock for %s: %v", ErrWriterActive, path, err)
	}
	identityUnlock, err := claimProcessIdentityWriter(lockPath)
	if err != nil {
		pathUnlock()
		return nil, fmt.Errorf("%w: acquire file identity lock for %s: %v", ErrWriterActive, path, err)
	}
	claim := &writerClaim{paths: map[string]struct{}{key: {}}, info: info, unlocks: []func(){pathUnlock}}
	if identityUnlock != nil {
		claim.unlocks = append(claim.unlocks, identityUnlock)
	}
	activeSessionWriters.paths[key] = claim
	activeSessionWriters.claims[claim] = struct{}{}
	return claim, nil
}

func refreshSessionWriter(claim *writerClaim, path string) error {
	key, lockPath, info, err := sessionWriterDescriptor(path)
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
	if claim.info == nil && info != nil {
		identityUnlock, lockErr := claimProcessIdentityWriter(lockPath)
		if lockErr != nil {
			return fmt.Errorf("%w: acquire created file identity lock for %s: %v", ErrWriterActive, path, lockErr)
		}
		if identityUnlock != nil {
			claim.unlocks = append(claim.unlocks, identityUnlock)
		}
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
	key, lockPath, info, err := sessionWriterDescriptor(path)
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
	// The stable path lock remains held across replacement. Retain the old
	// identity lock as well, and add a lock for the newly published inode so a
	// hard-link alias to either generation cannot become a concurrent writer.
	identityUnlock, lockErr := claimProcessIdentityWriter(lockPath)
	if lockErr != nil {
		return fmt.Errorf("%w: acquire rewritten file identity lock for %s: %v", ErrWriterActive, path, lockErr)
	}
	if identityUnlock != nil {
		claim.unlocks = append(claim.unlocks, identityUnlock)
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
		for index := len(claim.unlocks) - 1; index >= 0; index-- {
			claim.unlocks[index]()
		}
		claim.unlocks = nil
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

func sessionWriterDescriptor(path string) (string, string, os.FileInfo, error) {
	canonical := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonical = resolved
	} else if resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(path)); parentErr == nil {
		canonical = filepath.Join(resolvedParent, filepath.Base(path))
	}
	canonical = filepath.Clean(canonical)
	keyPath := canonical
	if runtime.GOOS == "windows" {
		keyPath = strings.ToLower(keyPath)
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "path:" + keyPath, canonical, nil, nil
		}
		return "", "", nil, err
	}
	return "path:" + keyPath, canonical, info, nil
}
