package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
)

// SessionLocation is the identity and working directory of a session. Reading
// it never projects its messages, context, or tree.
type SessionLocation struct {
	Path string
	ID   string
	Cwd  string
}

func (m *SessionManager) Location() SessionLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.store.mu.RLock()
	defer m.store.mu.RUnlock()
	return SessionLocation{Path: m.sessionFile, ID: m.store.header.id, Cwd: m.cwd}
}

type catalogDirectory struct {
	fingerprint catalogFingerprint
	location    SessionLocation
	valid       bool
}

// ListDirectories reconciles the same discoverable session files as ListAll,
// but needs only their headers. Appending messages costs at most another header
// read, independently of the file's history size. Directory state is derived
// and is kept separate from the full metadata catalog's refresh lock.
func (c *Catalog) ListDirectories() ([]SessionLocation, error) {
	if c == nil {
		return nil, errors.New("session catalog is unavailable")
	}
	c.directoryMu.Lock()
	defer c.directoryMu.Unlock()
	if c.directoriesDone {
		return nil, ErrClosed
	}
	files, err := catalogFilesInAgentDir(c.agentDir)
	if err != nil {
		return nil, err
	}
	if c.directories == nil {
		c.directories = make(map[string]catalogDirectory)
	}
	present := make(map[string]struct{}, len(files))
	result := make([]SessionLocation, 0, len(files))
	for _, file := range files {
		present[file.path] = struct{}{}
		record, exists := c.directories[file.path]
		if !exists || record.fingerprint != file.fingerprint {
			location, valid := discoverSessionLocation(file.path)
			record = catalogDirectory{
				fingerprint: file.fingerprint, valid: valid,
				location: location,
			}
			c.directories[file.path] = record
		}
		if record.valid {
			result = append(result, record.location)
		}
	}
	for path := range c.directories {
		if _, exists := present[path]; !exists {
			delete(c.directories, path)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func discoverSessionLocation(path string) (SessionLocation, bool) {
	file, err := os.Open(path)
	if err != nil {
		return SessionLocation{}, false
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) != 0 && json.Valid(line) {
			// Like full catalog discovery, the first parsed value must be a
			// compatible header. No history-size or header-size limit is added.
			header, decodeErr := decodeDiscoverableHeader(line)
			if decodeErr != nil {
				return SessionLocation{}, false
			}
			return SessionLocation{Path: path, ID: header.ID(), Cwd: header.WorkingDir()}, true
		}
		if readErr != nil {
			return SessionLocation{}, false
		}
	}
}
