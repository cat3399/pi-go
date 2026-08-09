package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// volatileSessionStorage is the non-persistent Store used by an in-memory
// SessionManager and by a persistent manager before pi's first-assistant
// durability boundary. Session still owns encoding and validation; this store
// deliberately owns no session semantics.
type volatileSessionStorage struct{}

func (volatileSessionStorage) validateReplace(string) error { return nil }
func (volatileSessionStorage) read(string) ([]byte, error)  { return nil, nil }
func (volatileSessionStorage) create(string, []byte) (bool, error) {
	return true, nil
}
func (volatileSessionStorage) append(ctx context.Context, _ string, _ []byte) (bool, error) {
	if cause := context.Cause(ctx); cause != nil {
		return false, cause
	}
	return true, nil
}
func (volatileSessionStorage) replace(string, []byte) (bool, error) { return true, nil }

func newVolatileSession(header Header, entries []Entry, runtime runtimeConfig) *Session {
	byID := make(map[string]int, len(entries))
	cloned := make([]Entry, len(entries))
	for index, entry := range entries {
		cloned[index] = entry.clone()
		byID[entry.id] = index
	}
	leaf := len(cloned) - 1
	return &Session{
		appendGate: make(chan struct{}, 1),
		storage:    volatileSessionStorage{},
		header:     header.clone(),
		entries:    cloned,
		byID:       byID,
		leaf:       leaf,
		runtime:    runtime,
	}
}

// loadVolatileSessionSnapshot mirrors SessionManager._setSessionFile() for an
// in-memory manager: legacy migration is applied to the in-memory bytes, but
// the source file is never rewritten and no durable writer is claimed.
func loadVolatileSessionSnapshot(path string, runtime runtimeConfig) (*Session, error) {
	header, entries, _, _, diagnostics, err := decodeCompatibleFromStorage(osSessionStorage{}, path)
	if errors.Is(err, ErrUnsupportedVersion) {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrStorage, path, readErr)
		}
		version, versionErr := sessionVersion(data)
		if versionErr != nil {
			return nil, versionErr
		}
		data, err = migrateLegacySession(path, data, version, runtime.newEntryID)
		if err != nil {
			return nil, err
		}
		header, entries, _, _, err = decodeStructurallyCleanCompatibleSessionFile(path, bytes.Clone(data))
		diagnostics = nil
	}
	if err != nil {
		return nil, err
	}
	result := newVolatileSession(header, entries, runtime)
	result.loadDiagnostics = append([]LoadDiagnostic(nil), diagnostics...)
	return result, nil
}

func newManagerHeader(cwd string, options NewSessionOptions, runtime runtimeConfig) (Header, error) {
	timestamp := canonicalTime(runtime.now())
	if timestamp.IsZero() || validateISOTime(timestamp) != nil {
		return Header{}, fmt.Errorf("%w: invalid creation timestamp", ErrInvalidSession)
	}
	id := options.ID
	var err error
	if id == "" {
		id, err = newSessionID(timestamp)
		if err != nil {
			return Header{}, fmt.Errorf("%w: %w", ErrIDGeneration, err)
		}
	}
	if err := ValidateSessionID(id); err != nil {
		return Header{}, err
	}
	wire := newHeaderWire{Type: "session", Version: 3, ID: id, Timestamp: formatISOTime(timestamp), WorkingDir: cwd, ParentSession: options.ParentSession}
	raw, err := json.Marshal(wire)
	if err != nil {
		return Header{}, fmt.Errorf("%w: encode header: %w", ErrInvalidSession, err)
	}
	return Header{id: id, workingDir: cwd, parentSession: options.ParentSession, hasParentSession: options.ParentSession != "", timestamp: timestamp, raw: raw}, nil
}

// createSessionSnapshot is the Store boundary used when SessionManager's
// delayed session becomes durable and when a selected branch is materialized.
// Entry bytes are already validated and are copied without re-encoding.
func createSessionSnapshot(path string, header Header, entries []Entry, runtime runtimeConfig) (*Session, error) {
	resolvedPath, err := resolveSessionPath(path)
	if err != nil {
		return nil, err
	}
	data := append(append([]byte(nil), header.raw...), '\n')
	for _, entry := range entries {
		data = append(data, entry.raw...)
		data = append(data, '\n')
	}
	// Validate the complete candidate before claiming or publishing its target.
	// In particular, a compatible source may carry a diagnosed compaction whose
	// firstKeptEntryId is not on the selected branch. Re-chaining that branch must
	// fail with zero target-file side effects rather than leave a published file
	// that this process refuses to own.
	decodedHeader, decodedEntries, byID, _, err := decodeStructurallyCleanCompatibleSessionFile(resolvedPath, data)
	if err != nil {
		return nil, fmt.Errorf("%w: validate session snapshot: %w", ErrStorage, err)
	}
	claim, err := claimSessionWriter(resolvedPath)
	if err != nil {
		return nil, err
	}
	claimed := true
	defer func() {
		if claimed {
			releaseSessionWriter(claim)
		}
	}()
	storage := osSessionStorage{}
	created, err := storage.create(resolvedPath, data)
	if err != nil {
		if created {
			return nil, fmt.Errorf("%w: %w", ErrDurabilityUnknown, err)
		}
		return nil, fmt.Errorf("%w: create %s: %w", ErrStorage, resolvedPath, err)
	}
	if err := refreshSessionWriter(claim, resolvedPath); err != nil {
		return nil, err
	}
	result := &Session{appendGate: make(chan struct{}, 1), storage: storage, path: resolvedPath, header: decodedHeader, entries: decodedEntries, byID: byID, leaf: len(decodedEntries) - 1, runtime: runtime, writerClaim: claim}
	claimed = false
	return result, nil
}

// initializeEmptySession is the explicit-open exception to append-only
// creation. It replaces only a verified empty target with one valid header;
// non-empty invalid evidence is never overwritten.
func initializeEmptySession(path string, header Header, runtime runtimeConfig) (*Session, error) {
	resolvedPath, err := resolveSessionPath(path)
	if err != nil {
		return nil, err
	}
	claim, err := claimSessionWriter(resolvedPath)
	if err != nil {
		return nil, err
	}
	claimed := true
	defer func() {
		if claimed {
			releaseSessionWriter(claim)
		}
	}()
	storage := osSessionStorage{}
	data, err := storage.read(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read empty session %s: %v", ErrStorage, resolvedPath, err)
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("%w: explicit session is no longer empty", ErrInvalidSession)
	}
	if err := storage.validateReplace(resolvedPath); err != nil {
		return nil, err
	}
	candidate := append(append([]byte(nil), header.raw...), '\n')
	replaced, err := storage.replace(resolvedPath, candidate)
	if err != nil {
		if replaced {
			return nil, fmt.Errorf("%w: initialize empty session: %v", ErrDurabilityUnknown, err)
		}
		return nil, fmt.Errorf("%w: initialize empty session: %v", ErrStorage, err)
	}
	if err := refreshSessionWriterAfterRewrite(claim, resolvedPath); err != nil {
		return nil, fmt.Errorf("%w: adopt initialized session identity: %v", ErrDurabilityUnknown, err)
	}
	decodedHeader, entries, byID, _, err := decodeSessionFile(resolvedPath, candidate)
	if err != nil {
		return nil, err
	}
	result := &Session{appendGate: make(chan struct{}, 1), storage: storage, path: resolvedPath, header: decodedHeader, entries: entries, byID: byID, leaf: -1, runtime: runtime, writerClaim: claim}
	claimed = false
	return result, nil
}
