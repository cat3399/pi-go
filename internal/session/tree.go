package session

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// Entry returns an immutable snapshot selected by durable entry ID.
func (s *Session) Entry(id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index, ok := s.byID[id]
	if !ok {
		return Entry{}, false
	}
	return s.entries[index].clone(), true
}

// LeafEntry returns the entry selected for subsequent append. Selection is
// process-local: reopening a file selects its physically last record.
func (s *Session) LeafEntry() (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.leaf < 0 {
		return Entry{}, false
	}
	return s.entries[s.leaf].clone(), true
}

// SelectLeaf makes an existing entry the append parent. It does not rewrite
// history or persist a pointer, so it is atomic with Append within this process.
func (s *Session) SelectLeaf(id string) error {
	s.acquireAppendUncancelable()
	defer s.releaseAppend()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	index, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}
	s.leaf = index
	s.generation++
	return nil
}

// ResetLeaf selects the virtual position before all entries. The next Append
// creates another root in the v3 forest. Reopen selects that new physical tail.
func (s *Session) ResetLeaf() error {
	s.acquireAppendUncancelable()
	defer s.releaseAppend()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.leaf = -1
	s.generation++
	return nil
}

// PathTo returns root-to-entry order. A v3 session is an append-only forest;
// every selected path therefore has exactly one root.
func (s *Session) PathTo(id string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}
	return s.pathLocked(index), nil
}

// BranchPath returns the root-to-selected-leaf path, or nil for the reset/empty
// position. Entries off the branch never contribute to Context.
func (s *Session) BranchPath() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.leaf < 0 {
		return nil
	}
	return s.pathLocked(s.leaf)
}

func (s *Session) pathLocked(index int) []Entry {
	path := make([]Entry, 0, len(s.entries))
	for index >= 0 {
		entry := s.entries[index]
		path = append(path, entry.clone())
		if !entry.hasParent {
			break
		}
		index = s.byID[entry.parentID]
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path
}

// Tree returns an immutable forest in JSONL append order. Decoder validation
// rejects duplicate, forward, broken, and cyclic parents before a Session exists.
func (s *Session) Tree() []TreeNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nodes := make([]TreeNode, len(s.entries))
	children := make([][]int, len(s.entries))
	roots := make([]int, 0)
	labels := make(map[string]string)
	labelTimestamps := make(map[string]time.Time)
	for _, entry := range s.entries {
		payload, ok := entry.payload.(LabelPayload)
		if !ok {
			continue
		}
		if payload.Label == nil || *payload.Label == "" {
			delete(labels, payload.TargetID)
			delete(labelTimestamps, payload.TargetID)
			continue
		}
		labels[payload.TargetID] = *payload.Label
		labelTimestamps[payload.TargetID] = entry.timestamp
	}
	for index, entry := range s.entries {
		nodes[index].Entry = entry.clone()
		if label, ok := labels[entry.id]; ok {
			labelCopy := label
			timestamp := labelTimestamps[entry.id]
			nodes[index].Label = &labelCopy
			nodes[index].LabelTimestamp = &timestamp
		}
		if !entry.hasParent {
			roots = append(roots, index)
			continue
		}
		children[s.byID[entry.parentID]] = append(children[s.byID[entry.parentID]], index)
	}
	// Build bottom-up to avoid recursive traversal and remain safe for deep trees.
	for index := len(nodes) - 1; index >= 0; index-- {
		if len(children[index]) == 0 {
			continue
		}
		nodes[index].Children = make([]TreeNode, len(children[index]))
		for childIndex, child := range children[index] {
			nodes[index].Children[childIndex] = nodes[child]
		}
		// Match SessionManager.getTree(): siblings are timestamp ordered while
		// stable ties retain their JSONL order.
		sort.SliceStable(nodes[index].Children, func(left, right int) bool {
			return nodes[index].Children[left].Entry.timestamp.Before(nodes[index].Children[right].Entry.timestamp)
		})
	}
	forest := make([]TreeNode, len(roots))
	for index, root := range roots {
		forest[index] = nodes[root]
	}
	return forest
}

// ExtractBranch atomically creates TargetPath with a fresh v3 header and only
// the root-to-leaf path. Source records are copied byte-for-byte and the source
// session is never rewritten, even if target creation fails or is cancelled.
func (s *Session) ExtractBranch(ctx context.Context, leafID string, options ExtractOptions) (*Session, error) {
	snapshot, err := s.snapshotForExport(ctx, &leafID)
	if err != nil {
		return nil, err
	}
	return extractWithStorage(ctx, snapshot.storage, snapshot.sourcePath, snapshot.entries, options)
}

// Fork atomically snapshots the complete forest of an active Session and
// creates an independent target. Use this method when the source aggregate is
// already open; unlike path-based ForkFrom it does not contend with its writer
// claim. The append gate makes the snapshot linearize before or after Append.
func (s *Session) Fork(ctx context.Context, options ExtractOptions) (*Session, error) {
	snapshot, err := s.snapshotForExport(ctx, nil)
	if err != nil {
		return nil, err
	}
	return extractWithStorage(ctx, snapshot.storage, snapshot.sourcePath, snapshot.entries, options)
}

// ForkFrom copies the complete durable source forest into a new session. It is
// intentionally separate from branch extraction: all historical branches are
// retained, while ExtractBranch retains exactly one selected conversation.
func ForkFrom(ctx context.Context, sourcePath string, options ExtractOptions) (*Session, error) {
	source, err := Open(sourcePath, OpenOptions{})
	if err != nil {
		return nil, err
	}
	defer source.Close()
	return source.Fork(ctx, options)
}

type exportSnapshot struct {
	storage    sessionStorage
	sourcePath string
	entries    []Entry
}

// snapshotForExport holds the same gate used by Append only while copying
// immutable records. Target filesystem I/O happens after releasing the gate,
// so a slow destination cannot stall subsequent source appends.
func (s *Session) snapshotForExport(ctx context.Context, leafID *string) (exportSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.acquireAppend(ctx); err != nil {
		return exportSnapshot{}, err
	}
	defer s.releaseAppend()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return exportSnapshot{}, ErrClosed
	}
	// A commit-unknown append may be present on disk even though entries and
	// leaf intentionally did not advance. Exporting this in-memory view would
	// silently omit an uncertain durable tail, so the writer remains fully
	// quarantined until the caller closes and explicitly reopens/reconciles it.
	if s.poisoned {
		return exportSnapshot{}, ErrPoisoned
	}
	var entries []Entry
	if leafID != nil {
		index, ok := s.byID[*leafID]
		if !ok {
			return exportSnapshot{}, fmt.Errorf("%w: %s", ErrEntryNotFound, *leafID)
		}
		entries = s.pathLocked(index)
	} else {
		entries = make([]Entry, len(s.entries))
		for index := range s.entries {
			entries[index] = s.entries[index].clone()
		}
	}
	return exportSnapshot{storage: s.storage, sourcePath: s.path, entries: entries}, nil
}

func extractWithStorage(ctx context.Context, storage sessionStorage, sourcePath string, entries []Entry, options ExtractOptions) (*Session, error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, fmt.Errorf("%w: %w", ErrAppendCanceled, cause)
	}
	targetPath, err := resolveSessionPath(options.TargetPath)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(targetPath) == filepath.Clean(sourcePath) {
		return nil, ErrSourceEqualsTarget
	}
	claim, err := claimSessionWriter(targetPath)
	if err != nil {
		return nil, err
	}
	claimed := true
	defer func() {
		if claimed {
			releaseSessionWriter(claim)
		}
	}()
	workingDir, err := resolveWorkingDir(options.WorkingDir)
	if err != nil {
		return nil, err
	}
	runtime := normalizeRuntime(options.Now, options.NewEntryID)
	timestamp := canonicalTime(runtime.now())
	if timestamp.IsZero() || validateISOTime(timestamp) != nil {
		return nil, fmt.Errorf("%w: extraction timestamp", ErrInvalidSession)
	}
	sessionID := options.ID
	if sessionID == "" {
		sessionID, err = newSessionID(timestamp)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrIDGeneration, err)
		}
	}
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, err
	}
	headerRaw, err := json.Marshal(newHeaderWire{Type: "session", Version: 3, ID: sessionID, Timestamp: formatISOTime(timestamp), WorkingDir: workingDir, ParentSession: sourcePath})
	if err != nil {
		return nil, fmt.Errorf("%w: encode extraction header: %w", ErrInvalidSession, err)
	}
	data := append(append([]byte(nil), headerRaw...), '\n')
	for _, entry := range entries {
		data = append(data, entry.raw...)
		data = append(data, '\n')
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, fmt.Errorf("%w: %w", ErrAppendCanceled, cause)
	}
	created, err := storage.create(targetPath, data)
	if err != nil {
		if created {
			return nil, fmt.Errorf("%w: %w", ErrDurabilityUnknown, err)
		}
		return nil, fmt.Errorf("%w: extract %s: %w", ErrStorage, targetPath, err)
	}
	header, decoded, byID, _, err := decodeSessionFile(targetPath, data)
	if err != nil {
		return nil, fmt.Errorf("%w: validate extracted session: %w", ErrStorage, err)
	}
	if err := refreshSessionWriter(claim, targetPath); err != nil {
		return nil, err
	}
	session := &Session{appendGate: make(chan struct{}, 1), storage: storage, path: targetPath, header: header, entries: decoded, byID: byID, leaf: len(decoded) - 1, runtime: runtime, writerClaim: claim}
	claimed = false
	return session, nil
}
