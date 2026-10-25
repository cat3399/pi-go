package session

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

var ErrReadSourceChanged = errors.New("session source changed while reading")

// ReadIndex is an immutable, rebuildable JSONL projection. It owns neither a
// writer claim, an open file nor a Runtime. Only read methods expose its data.
// Legacy files require the existing writer-owned migration before indexing so
// generated entry IDs cannot change between browsing and a later command.
type ReadIndex struct {
	path    string
	manager *SessionManager
	info    os.FileInfo
	digest  [sha256.Size]byte
	bytes   int64
	// New file timestamps require one successful digest validation after the
	// filesystem clock has advanced, even if no query arrived in that window.
	validationMu sync.Mutex
	settled      bool
	bodies       readBodyCache
}

// ReadIndexIdentity is small validation metadata retained when an index's
// payload is evicted. Rebuilding the same verified file can then resume a page
// cursor even when that file is larger than the payload cache budget.
type ReadIndexIdentity struct {
	info       os.FileInfo
	digest     [sha256.Size]byte
	generation string
}

func OpenReadIndex(path string) (*ReadIndex, error) {
	return OpenReadIndexWithIdentity(path, ReadIndexIdentity{})
}

func OpenReadIndexWithIdentity(path string, previous ReadIndexIdentity) (*ReadIndex, error) {
	return openReadIndex(path, previous, true)
}

func openReadIndex(path string, previous ReadIndexIdentity, bounded bool) (*ReadIndex, error) {
	path, err := resolveSessionPath(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: session source is not a regular file", ErrInvalidSession)
	}
	index := &ReadIndex{path: path, info: before}
	// Remember uncertainty at open, even when decoding a large file outlives
	// the timestamp settling window. A same-tick rewrite during that decode
	// must still be checked against the completed file's content digest.
	needsDigestAtOpen := readSourceNeedsDigest(before)
	if bounded {
		index.bodies.limit = min(defaultReadBodyCacheBytes, before.Size()*8)
	}
	var summary sessionInfoSummary
	var project func(Entry, int64) Entry
	if bounded {
		project = func(entry Entry, offset int64) Entry {
			summary.appendEntry(entry)
			return index.projectEntry(entry, offset)
		}
	}
	digest := sha256.New()
	header, entries, byID, separator, diagnostics, err := decodeCompatibleSessionReaderProjected(path, io.TeeReader(file, digest), project)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !sameReadFile(before, after) || !sameReadFile(after, current) {
		return nil, ErrReadSourceChanged
	}
	index.info = current
	index.settled = !needsDigestAtOpen && !readSourceNeedsDigest(current)
	copy(index.digest[:], digest.Sum(nil))
	if needsDigestAtOpen || readSourceNeedsDigest(current) {
		// Filesystems without change time, and timestamps still within their
		// possible clock granularity, need content validation after decoding.
		if valid, err := index.validateDigest(path); err != nil || !valid {
			if err != nil {
				return nil, err
			}
			return nil, ErrReadSourceChanged
		}
		index.settled = !readSourceNeedsDigest(current)
	}
	store := &Session{
		header: header, entries: entries, byID: byID, leaf: len(entries) - 1,
		needsSeparator: separator, loadDiagnostics: diagnostics,
	}
	index.manager = &SessionManager{
		cwd: header.WorkingDir(), sessionDir: filepath.Dir(path), sessionFile: path,
		persist: true, flushed: true, store: store,
	}
	if bounded {
		index.manager.summary = managerSummary{store: store, entries: len(entries), value: summary}
	}
	if previous.generation != "" && previous.digest == index.digest && sameReadFile(previous.info, index.info) {
		index.manager.readOnce.Do(func() { index.manager.readEpoch = previous.generation })
	}
	if bounded {
		index.bytes = readIndexBytes(header, entries, summary, diagnostics) + index.bodies.limit
	} else {
		// Only explicit full-context/tree reads use this transient fallback.
		index.bytes = before.Size()*4 + int64(len(entries))*1024
	}
	return index, nil
}

func (r *ReadIndex) EstimatedBytes() int64 { return r.bytes }

// Path is immutable source identity, not a fresh projection or file validation.
func (r *ReadIndex) Path() string { return r.path }

func (r *ReadIndex) Identity() ReadIndexIdentity {
	r.manager.mu.RLock()
	defer r.manager.mu.RUnlock()
	r.manager.store.mu.RLock()
	defer r.manager.store.mu.RUnlock()
	return ReadIndexIdentity{info: r.info, digest: r.digest, generation: r.manager.readVersionLocked().Generation}
}

func (r *ReadIndex) ValidateSource() (bool, error) {
	path := r.manager.sessionFile
	current, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameReadFile(r.info, current) {
		return false, nil
	}
	r.validationMu.Lock()
	defer r.validationMu.Unlock()
	needsDigest := readSourceNeedsDigest(current)
	if !r.settled || needsDigest {
		valid, err := r.validateDigest(path)
		if valid && err == nil && !needsDigest {
			r.settled = true
		}
		return valid, err
	}
	return true, nil
}

// Several writes can share a filesystem clock tick. In particular, an in-place
// overwrite followed by restoring mtime can also leave ctime unchanged for a
// newly indexed file. Hash during a conservative settling window; older sources
// retain the cheap stat-only validation path. Platforms without ctime always
// use content validation. Future-dated clocks also fail safe rather than cache.
func readSourceNeedsDigest(info os.FileInfo) bool {
	change, supported := fileChangeTime(info)
	return !supported || time.Since(time.Unix(change[0], change[1])) < time.Second
}

func (r *ReadIndex) validateDigest(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	digest := sha256.New()
	_, readErr := io.Copy(digest, file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return false, err
	}
	var actual [sha256.Size]byte
	copy(actual[:], digest.Sum(nil))
	return actual == r.digest, nil
}

func sameReadFile(left, right os.FileInfo) bool {
	if left == nil || right == nil || !os.SameFile(left, right) || left.Size() != right.Size() || !left.ModTime().Equal(right.ModTime()) {
		return false
	}
	leftChange, leftOK := fileChangeTime(left)
	rightChange, rightOK := fileChangeTime(right)
	return leftOK == rightOK && (!leftOK || leftChange == rightChange)
}

// Go's portable FileInfo lacks ctime. Unix Stat_t exposes it under one of
// these two field names. Reflection keeps the read index portable; platforms
// without either shape use the content-digest path above.
func fileChangeTime(info os.FileInfo) ([2]int64, bool) {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return [2]int64{}, false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := value.FieldByName(name)
		if field.Kind() != reflect.Struct {
			continue
		}
		sec, nsec := field.FieldByName("Sec"), field.FieldByName("Nsec")
		if sec.IsValid() && nsec.IsValid() && sec.CanInt() && nsec.CanInt() {
			return [2]int64{sec.Int(), nsec.Int()}, true
		}
	}
	return [2]int64{}, false
}

func fileSourceSignature(path string, info os.FileInfo) (string, error) {
	change, hasChange := fileChangeTime(info)
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if hasChange && value.Kind() == reflect.Struct {
		device, devOK := readStatInteger(value.FieldByName("Dev"))
		inode, inoOK := readStatInteger(value.FieldByName("Ino"))
		if devOK && inoOK {
			return fmt.Sprintf("%x:%x:%d:%d", device, inode, change[0], change[1]), nil
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, readErr := io.Copy(digest, file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

func readStatInteger(value reflect.Value) (uint64, bool) {
	if value.IsValid() && value.CanUint() {
		return value.Uint(), true
	}
	if value.IsValid() && value.CanInt() {
		return uint64(value.Int()), true
	}
	return 0, false
}

func (r *ReadIndex) ReadSnapshot(options SnapshotOptions) (ReadSnapshot, error) {
	return readIndexed(r, func(manager *SessionManager) (ReadSnapshot, error) {
		if options.IncludeContext || options.IncludeTree {
			// Preserve the full projection contract for explicit consumers; page
			// and metadata reads never materialize the complete log this way.
			full, err := openReadIndex(r.path, r.Identity(), false)
			if err != nil {
				return ReadSnapshot{}, err
			}
			return full.manager.ReadSnapshot(options)
		}
		return manager.ReadSnapshot(options)
	})
}

func (r *ReadIndex) ReadHistory(query HistoryQuery) (HistoryPage, error) {
	return readIndexed(r, func(manager *SessionManager) (HistoryPage, error) { return manager.ReadHistory(query) })
}

func (r *ReadIndex) ReadRuns(query RunsQuery) (RunsPage, error) {
	return readIndexed(r, func(manager *SessionManager) (RunsPage, error) { return manager.ReadRuns(query) })
}

func (r *ReadIndex) ReadNodes(query NodeQuery) (NodePage, error) {
	return readIndexed(r, func(manager *SessionManager) (NodePage, error) { return manager.ReadNodes(query) })
}

func (r *ReadIndex) ReadEntry(id, generation string) (EntrySnapshot, error) {
	return readIndexed(r, func(manager *SessionManager) (EntrySnapshot, error) { return manager.ReadEntry(id, generation) })
}
