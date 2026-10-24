package session

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

var ErrReadSourceChanged = errors.New("session source changed while reading")

// ReadIndex is an immutable, rebuildable JSONL projection. It owns neither a
// writer claim, an open file nor a Runtime. Only read methods expose its data.
// Legacy files require the existing writer-owned migration before indexing so
// generated entry IDs cannot change between browsing and a later command.
type ReadIndex struct {
	manager *SessionManager
	info    os.FileInfo
	digest  [sha256.Size]byte
	bytes   int64
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
	digest := sha256.New()
	header, entries, byID, separator, diagnostics, err := decodeCompatibleSessionReader(path, io.TeeReader(file, digest))
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
	index := &ReadIndex{info: current}
	copy(index.digest[:], digest.Sum(nil))
	if _, supported := fileChangeTime(current); !supported {
		// Filesystems without an observable change time use content validation,
		// including after decoding, rather than trusting size + mtime forever.
		if valid, err := index.validateDigest(path); err != nil || !valid {
			if err != nil {
				return nil, err
			}
			return nil, ErrReadSourceChanged
		}
	}
	store := &Session{
		header: header, entries: entries, byID: byID, leaf: len(entries) - 1,
		needsSeparator: separator, loadDiagnostics: diagnostics,
	}
	index.manager = &SessionManager{
		cwd: header.WorkingDir(), sessionDir: filepath.Dir(path), sessionFile: path,
		persist: true, flushed: true, store: store,
	}
	if previous.generation != "" && previous.digest == index.digest && sameReadFile(previous.info, index.info) {
		index.manager.readOnce.Do(func() { index.manager.readEpoch = previous.generation })
	}
	// This is an accounting estimate for eviction, not a heap measurement.
	// Include decoded payloads/indices as well as retained original JSON bytes.
	index.bytes = before.Size()*4 + int64(len(entries))*1024
	return index, nil
}

func (r *ReadIndex) EstimatedBytes() int64 { return r.bytes }

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
	if _, supported := fileChangeTime(current); !supported {
		return r.validateDigest(path)
	}
	return true, nil
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
	return r.manager.ReadSnapshot(options)
}

func (r *ReadIndex) ReadHistory(query HistoryQuery) (HistoryPage, error) {
	return r.manager.ReadHistory(query)
}

func (r *ReadIndex) ReadRuns(query RunsQuery) (RunsPage, error) {
	return r.manager.ReadRuns(query)
}

func (r *ReadIndex) ReadNodes(query NodeQuery) (NodePage, error) {
	return r.manager.ReadNodes(query)
}

func (r *ReadIndex) ReadEntry(id, generation string) (EntrySnapshot, error) {
	return r.manager.ReadEntry(id, generation)
}
