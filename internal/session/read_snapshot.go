package session

import (
	"fmt"
)

// ReadVersion identifies a manager's immutable append log and current branch
// view. It is process-local validation metadata, never part of durable JSONL.
// Reopening/replacing a source creates another Generation even when its session
// ID and entry IDs are reused. Log advances only for accepted entries; View also
// advances for leaf selection. Neither version depends on streaming tokens.
type ReadVersion struct {
	Generation string
	Log        uint64
	View       uint64
}

// SnapshotOptions selects projections from one SessionManager read boundary.
// LeafID changes the requested context projection, not the selected append leaf.
// Leaving both projections disabled reads metadata/version without copying
// historical context or tree projections.
type SnapshotOptions struct {
	LeafID         string
	IncludeContext bool
	IncludeTree    bool
}

// ReadSnapshot contains owned immutable projections from one manager state.
// Version describes that state only: consumers must also match their query
// (requested leaf, projection and, for pages, range) before reusing a result.
// Entries is the compaction-aware context projection, not the complete log.
type ReadSnapshot struct {
	Version ReadVersion
	Info    SessionInfo
	LeafID  *string
	Entries []Entry
	Context Context
	Tree    []TreeNode
}

func (m *SessionManager) ReadSnapshot(options SnapshotOptions) (ReadSnapshot, error) {
	var result ReadSnapshot
	err := m.readView(func(store *Session, metadata ReadSnapshot) error {
		leaf := store.leaf
		if options.LeafID != "" {
			var exists bool
			leaf, exists = store.byID[options.LeafID]
			if !exists {
				return fmt.Errorf("%w: %s", ErrEntryNotFound, options.LeafID)
			}
		}
		result = metadata
		if options.IncludeContext {
			if leaf >= 0 {
				result.Entries = managerContextEntries(store.pathLocked(leaf))
			}
			result.Context = store.buildContextAtLocked(leaf)
		}
		if options.IncludeTree {
			result.Tree = store.treeLocked()
		}
		return nil
	})
	return result, err
}

// readView holds one owner boundary while a read-only projection runs. Only
// projections internal to this package are passed here, never user callbacks.
func (m *SessionManager) readView(project func(*Session, ReadSnapshot) error) error {
	if m == nil {
		return fmt.Errorf("%w: nil session manager", ErrInvalidSession)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	store := m.store
	if store == nil {
		return ErrClosed
	}
	m.summaryMu.Lock()
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		m.summaryMu.Unlock()
		return ErrClosed
	}
	if store.poisoned {
		m.summaryMu.Unlock()
		// The durable tail is uncertain. Never validate a cached read against a
		// version that could conceal an accepted but unreconciled disk write.
		return ErrPoisoned
	}
	info := m.summaryLocked(store)
	m.summaryMu.Unlock()
	result := ReadSnapshot{
		Version: m.readVersionLocked(),
		Info:    info,
	}
	if store.leaf >= 0 {
		id := store.entries[store.leaf].id
		result.LeafID = &id
	}
	return project(store, result)
}
