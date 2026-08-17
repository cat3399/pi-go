package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sessionCatalogSnapshotVersion  = 1
	maxSessionCatalogSnapshotBytes = 64 << 20
)

// Catalog is a rebuildable, concurrency-safe projection of the JSONL session
// directory. Session files remain authoritative: every read reconciles file
// fingerprints and only reparses files that are new or changed.
type Catalog struct {
	agentDir        string
	snapshotPath    string
	loadInfo        sessionInfoLoader
	records         map[string]catalogRecord
	store           catalogSnapshotStore
	persistent      bool
	rewriteSnapshot bool
	closed          bool

	mu sync.Mutex
}

type catalogFingerprint struct {
	size       int64
	modifiedNS int64
}

type catalogFile struct {
	path        string
	fingerprint catalogFingerprint
}

type catalogRecord struct {
	fingerprint catalogFingerprint
	valid       bool
	info        SessionInfo
}

type catalogSnapshot struct {
	Version  int                    `json:"version"`
	AgentDir string                 `json:"agentDir"`
	Entries  []catalogSnapshotEntry `json:"entries"`
}

type catalogSnapshotEntry struct {
	Path           string               `json:"path"`
	FileSize       int64                `json:"fileSize"`
	FileModifiedNS int64                `json:"fileModifiedNs"`
	Valid          bool                 `json:"valid"`
	Info           *catalogSnapshotInfo `json:"info,omitempty"`
}

type catalogSnapshotInfo struct {
	ID                string    `json:"id"`
	CWD               string    `json:"cwd"`
	Name              string    `json:"name,omitempty"`
	HasName           bool      `json:"hasName,omitempty"`
	ParentSessionPath string    `json:"parentSessionPath,omitempty"`
	HasParentSession  bool      `json:"hasParentSession,omitempty"`
	Created           time.Time `json:"created"`
	Modified          time.Time `json:"modified"`
	MessageCount      int       `json:"messageCount"`
	FirstMessage      string    `json:"firstMessage"`
}

type catalogSnapshotStore interface {
	Read(string) ([]byte, error)
	Write(string, []byte) error
}

type osCatalogSnapshotStore struct{}

func (osCatalogSnapshotStore) Read(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(statErr, file.Close())
	}
	if info.Size() > maxSessionCatalogSnapshotBytes {
		return nil, errors.Join(
			fmt.Errorf("session catalog snapshot is too large: %d bytes", info.Size()),
			file.Close(),
		)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSessionCatalogSnapshotBytes+1))
	closeErr := file.Close()
	if len(data) > maxSessionCatalogSnapshotBytes {
		readErr = errors.Join(readErr, fmt.Errorf("session catalog snapshot exceeds size limit"))
	}
	return data, errors.Join(readErr, closeErr)
}

func (osCatalogSnapshotStore) Write(path string, data []byte) error {
	if len(data) > maxSessionCatalogSnapshotBytes {
		return fmt.Errorf("session catalog snapshot exceeds size limit: %d bytes", len(data))
	}
	_, err := replaceSessionFile(osSessionRewriteOps{}, path, data)
	return err
}

// NewCatalog opens a rebuildable catalog backed by a versioned snapshot in the
// user's cache directory. Temporary agent directories and cache failures stay
// memory-only so session discovery never depends on derived state.
func NewCatalog(agentDir string) (*Catalog, error) {
	return newCatalog(agentDir, buildSessionCatalogInfo)
}

func newCatalog(agentDir string, load sessionInfoLoader) (*Catalog, error) {
	resolved, load, err := resolveCatalogInputs(agentDir, load)
	if err != nil {
		return nil, err
	}
	snapshotPath, persistent := defaultCatalogSnapshotPath(resolved)
	if !persistent {
		return openCatalogSnapshot(resolved, "", load, nil, false), nil
	}
	cacheDir := filepath.Dir(snapshotPath)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return openCatalogSnapshot(resolved, "", load, nil, false), nil
	}
	_ = os.Chmod(cacheDir, 0o700)
	return openCatalogSnapshot(resolved, snapshotPath, load, osCatalogSnapshotStore{}, true), nil
}

func newCatalogAt(agentDir, snapshotPath string, load sessionInfoLoader) (*Catalog, error) {
	return newCatalogAtWithStore(agentDir, snapshotPath, load, osCatalogSnapshotStore{})
}

func newCatalogAtWithStore(
	agentDir, snapshotPath string,
	load sessionInfoLoader,
	store catalogSnapshotStore,
) (*Catalog, error) {
	resolved, load, err := resolveCatalogInputs(agentDir, load)
	if err != nil {
		return nil, err
	}
	if snapshotPath == ":memory:" {
		return openCatalogSnapshot(resolved, "", load, nil, false), nil
	}
	if store == nil {
		return nil, errors.New("session catalog snapshot store is unavailable")
	}
	snapshotPath, err = filepath.Abs(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("resolve session catalog path: %w", err)
	}
	snapshotPath = filepath.Clean(snapshotPath)
	cacheDir := filepath.Dir(snapshotPath)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create session catalog directory: %w", err)
	}
	_ = os.Chmod(cacheDir, 0o700)
	return openCatalogSnapshot(resolved, snapshotPath, load, store, true), nil
}

func resolveCatalogInputs(agentDir string, load sessionInfoLoader) (string, sessionInfoLoader, error) {
	if strings.TrimSpace(agentDir) == "" {
		return "", nil, fmt.Errorf("%w: agent directory must be non-empty", ErrInvalidSession)
	}
	resolved, err := filepath.Abs(agentDir)
	if err != nil {
		return "", nil, fmt.Errorf("%w: resolve agent directory: %v", ErrInvalidSession, err)
	}
	if load == nil {
		load = buildSessionCatalogInfo
	}
	return filepath.Clean(resolved), load, nil
}

func defaultCatalogSnapshotPath(agentDir string) (string, bool) {
	if temporaryRoot, err := filepath.Abs(os.TempDir()); err == nil && catalogPathWithin(temporaryRoot, agentDir) {
		return "", false
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(cacheRoot) == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(agentDir))
	name := fmt.Sprintf("session-catalog-v%d-%x.json", sessionCatalogSnapshotVersion, digest[:16])
	return filepath.Join(cacheRoot, "pi-go", "session-catalog", name), true
}

func catalogPathWithin(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func openCatalogSnapshot(
	agentDir, snapshotPath string,
	load sessionInfoLoader,
	store catalogSnapshotStore,
	persistent bool,
) *Catalog {
	catalog := &Catalog{
		agentDir: agentDir, snapshotPath: snapshotPath, loadInfo: load,
		records: make(map[string]catalogRecord), store: store, persistent: persistent,
	}
	if !persistent {
		return catalog
	}
	data, err := store.Read(snapshotPath)
	if errors.Is(err, os.ErrNotExist) {
		return catalog
	}
	if err != nil {
		catalog.rewriteSnapshot = true
		return catalog
	}
	records, err := decodeCatalogSnapshot(agentDir, data)
	if err != nil {
		catalog.rewriteSnapshot = true
		return catalog
	}
	catalog.records = records
	return catalog
}

func decodeCatalogSnapshot(agentDir string, data []byte) (map[string]catalogRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot catalogSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode session catalog snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode session catalog trailing data: %w", err)
	}
	if snapshot.Version != sessionCatalogSnapshotVersion {
		return nil, fmt.Errorf("unsupported session catalog snapshot version %d", snapshot.Version)
	}
	if filepath.Clean(snapshot.AgentDir) != agentDir {
		return nil, fmt.Errorf("session catalog snapshot belongs to a different agent directory")
	}

	root := filepath.Join(agentDir, "sessions")
	records := make(map[string]catalogRecord, len(snapshot.Entries))
	for index, entry := range snapshot.Entries {
		path := filepath.Clean(entry.Path)
		if !filepath.IsAbs(path) || !catalogPathWithin(root, path) || !strings.HasSuffix(path, ".jsonl") {
			return nil, fmt.Errorf("session catalog snapshot entry %d has invalid path %q", index, entry.Path)
		}
		if entry.FileSize < 0 {
			return nil, fmt.Errorf("session catalog snapshot entry %d has negative size", index)
		}
		if _, exists := records[path]; exists {
			return nil, fmt.Errorf("session catalog snapshot contains duplicate path %q", path)
		}
		record := catalogRecord{fingerprint: catalogFingerprint{
			size: entry.FileSize, modifiedNS: entry.FileModifiedNS,
		}}
		if entry.Valid {
			if entry.Info == nil || strings.TrimSpace(entry.Info.ID) == "" {
				return nil, fmt.Errorf("session catalog snapshot entry %d has no session identity", index)
			}
			record.valid = true
			record.info = entry.Info.sessionInfo(path)
		} else if entry.Info != nil {
			return nil, fmt.Errorf("invalid session catalog snapshot entry %d contains metadata", index)
		}
		records[path] = record
	}
	return records, nil
}

func (info catalogSnapshotInfo) sessionInfo(path string) SessionInfo {
	return SessionInfo{
		Path: path, ID: info.ID, Cwd: info.CWD, Name: info.Name, HasName: info.HasName,
		ParentSessionPath: info.ParentSessionPath, HasParentSession: info.HasParentSession,
		Created: info.Created, Modified: info.Modified, MessageCount: info.MessageCount,
		FirstMessage: info.FirstMessage,
	}
}

// ListAll returns the current durable session catalog, ordered by conversation
// activity. Calls are serialized so concurrent surfaces share one refresh.
func (c *Catalog) ListAll() ([]SessionInfo, error) {
	if c == nil {
		return nil, errors.New("session catalog is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}

	files, err := catalogFilesInAgentDir(c.agentDir)
	if err != nil {
		return nil, err
	}
	present := make(map[string]struct{}, len(files))
	changed := make([]catalogFile, 0)
	for _, file := range files {
		present[file.path] = struct{}{}
		if record, ok := c.records[file.path]; !ok || record.fingerprint != file.fingerprint {
			changed = append(changed, file)
		}
	}
	dirty := c.rewriteSnapshot
	for path := range c.records {
		if _, ok := present[path]; !ok {
			delete(c.records, path)
			dirty = true
		}
	}
	if len(changed) != 0 {
		c.applyChanges(changed)
		dirty = true
	}
	if dirty && c.persistent {
		// The snapshot is only an accelerator. A write failure keeps the current
		// in-memory projection authoritative for this process and is retried on
		// the next call without failing session discovery.
		_ = c.persistSnapshot()
	}
	return c.queryAll(), nil
}

func catalogFilesInAgentDir(agentDir string) ([]catalogFile, error) {
	root := filepath.Join(agentDir, "sessions")
	directories, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []catalogFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session catalog root: %w", err)
	}
	files := make([]catalogFile, 0)
	for _, directory := range directories {
		if !directory.IsDir() {
			continue
		}
		bucket := filepath.Join(root, directory.Name())
		items, readErr := os.ReadDir(bucket)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read session catalog directory %s: %w", bucket, readErr)
		}
		for _, item := range items {
			if item.IsDir() || !strings.HasSuffix(item.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(bucket, item.Name())
			info, statErr := os.Stat(path)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil {
				return nil, fmt.Errorf("stat session file %s: %w", path, statErr)
			}
			if info.IsDir() {
				continue
			}
			files = append(files, catalogFile{
				path: path,
				fingerprint: catalogFingerprint{
					size: info.Size(), modifiedNS: info.ModTime().UnixNano(),
				},
			})
		}
	}
	return files, nil
}

func (c *Catalog) applyChanges(changed []catalogFile) {
	paths := make([]string, len(changed))
	for index, file := range changed {
		paths[index] = file.path
	}
	infos, valid := buildSessionInfoResults(paths, nil, 0, len(paths), c.loadInfo)
	for index, file := range changed {
		record := catalogRecord{fingerprint: file.fingerprint, valid: valid[index]}
		if valid[index] {
			record.info = infos[index]
			record.info.Path = file.path
			record.info.AllMessagesText = ""
		}
		c.records[file.path] = record
	}
}

func (c *Catalog) persistSnapshot() error {
	data, err := encodeCatalogSnapshot(c.agentDir, c.records)
	if err != nil {
		c.rewriteSnapshot = true
		return err
	}
	if err := c.store.Write(c.snapshotPath, data); err != nil {
		c.rewriteSnapshot = true
		return err
	}
	c.rewriteSnapshot = false
	return nil
}

func encodeCatalogSnapshot(agentDir string, records map[string]catalogRecord) ([]byte, error) {
	paths := make([]string, 0, len(records))
	for path := range records {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	snapshot := catalogSnapshot{
		Version: sessionCatalogSnapshotVersion, AgentDir: agentDir,
		Entries: make([]catalogSnapshotEntry, 0, len(records)),
	}
	for _, path := range paths {
		record := records[path]
		entry := catalogSnapshotEntry{
			Path: path, FileSize: record.fingerprint.size,
			FileModifiedNS: record.fingerprint.modifiedNS, Valid: record.valid,
		}
		if record.valid {
			entry.Info = newCatalogSnapshotInfo(record.info)
		}
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode session catalog snapshot: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxSessionCatalogSnapshotBytes {
		return nil, fmt.Errorf("session catalog snapshot exceeds size limit: %d bytes", len(data))
	}
	return data, nil
}

func newCatalogSnapshotInfo(info SessionInfo) *catalogSnapshotInfo {
	return &catalogSnapshotInfo{
		ID: info.ID, CWD: info.Cwd, Name: info.Name, HasName: info.HasName,
		ParentSessionPath: info.ParentSessionPath, HasParentSession: info.HasParentSession,
		Created: info.Created, Modified: info.Modified, MessageCount: info.MessageCount,
		FirstMessage: info.FirstMessage,
	}
}

func (c *Catalog) queryAll() []SessionInfo {
	result := make([]SessionInfo, 0, len(c.records))
	for _, record := range c.records {
		if record.valid {
			result = append(result, record.info)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if !result[left].Modified.Equal(result[right].Modified) {
			return result[left].Modified.After(result[right].Modified)
		}
		if result[left].ID != result[right].ID {
			return result[left].ID < result[right].ID
		}
		return result[left].Path < result[right].Path
	})
	return result
}

// Close releases the process-local catalog. The snapshot and JSONL session
// files are untouched.
func (c *Catalog) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.records = nil
	return nil
}
