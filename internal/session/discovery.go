package session

import (
	"bufio"
	"bytes"
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
	maxSessionHeaderScanBytes     = 1024 * 1024
	maxConcurrentSessionInfoLoads = 10
)

// SessionInfo is the discovery projection used by pi's session picker and
// continue-recent lifecycle. Modified is conversation activity time, not a
// label/custom-entry append time.
type SessionInfo struct {
	Path              string
	ID                string
	Cwd               string
	Name              string
	HasName           bool
	ParentSessionPath string
	HasParentSession  bool
	Created           time.Time
	Modified          time.Time
	MessageCount      int
	FirstMessage      string
	AllMessagesText   string
}

type SessionListProgress func(loaded, total int)

// FindMostRecentSession returns an empty path when no compatible session is
// discoverable. Corrupt and non-session JSONL files do not hide valid peers.
func FindMostRecentSession(sessionDir, cwd string) (string, error) {
	resolvedDir, err := filepath.Abs(sessionDir)
	if err != nil {
		return "", err
	}
	resolvedCwd := ""
	if cwd != "" {
		resolvedCwd, err = resolveWorkingDir(cwd)
		if err != nil {
			return "", err
		}
	}
	directory, err := os.ReadDir(resolvedDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", nil
	}
	type candidate struct {
		path  string
		mtime time.Time
	}
	values := make([]candidate, 0)
	for _, item := range directory {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(resolvedDir, item.Name())
		header, ok := discoverSessionHeader(path)
		if !ok {
			continue
		}
		if resolvedCwd != "" {
			headerCwd, resolveErr := resolveWorkingDir(header.Cwd)
			if resolveErr != nil || headerCwd != resolvedCwd {
				continue
			}
		}
		info, statErr := item.Info()
		if statErr == nil {
			values = append(values, candidate{path: path, mtime: info.ModTime()})
		}
	}
	sort.SliceStable(values, func(i, j int) bool { return values[i].mtime.After(values[j].mtime) })
	if len(values) == 0 {
		return "", nil
	}
	return values[0].path, nil
}

type discoveryHeader struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession"`
}

func discoverSessionHeader(path string) (discoveryHeader, bool) {
	file, err := os.Open(path)
	if err != nil {
		return discoveryHeader{}, false
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, maxSessionHeaderScanBytes+1))
	read := 0
	for read <= maxSessionHeaderScanBytes {
		line, readErr := reader.ReadBytes('\n')
		read += len(line)
		if read > maxSessionHeaderScanBytes {
			return discoveryHeader{}, false
		}
		line = bytes.TrimSpace(line)
		if len(line) != 0 {
			var header discoveryHeader
			if json.Unmarshal(line, &header) == nil {
				if header.Type != "session" || header.ID == "" {
					return discoveryHeader{}, false
				}
				return header, true
			}
		}
		if readErr != nil {
			return discoveryHeader{}, false
		}
	}
	return discoveryHeader{}, false
}

// ListSessions lists one project's sessions. A custom flat directory is
// filtered by header cwd, while the default cwd-encoded directory is not.
func ListSessions(cwd, sessionDir string, progress SessionListProgress) ([]SessionInfo, error) {
	resolvedCwd, err := resolveWorkingDir(cwd)
	if err != nil {
		return nil, err
	}
	dir, err := resolveManagerSessionDir(resolvedCwd, sessionDir, false)
	if err != nil {
		return nil, err
	}
	filter := ""
	if sessionDir != "" && dir != defaultSessionDirPath(resolvedCwd, "") {
		filter = resolvedCwd
	}
	values := listSessionsFromDir(dir, progress, 0, 0)
	if filter != "" {
		values = filterSessionInfosByCwd(values, filter)
	}
	sortSessionInfos(values)
	return values, nil
}

// ListAllSessions lists a custom flat session directory when provided. With an
// empty argument it scans every cwd directory below ~/.pi/agent/sessions.
func ListAllSessions(sessionDir string, progress SessionListProgress) ([]SessionInfo, error) {
	if sessionDir != "" {
		dir, err := filepath.Abs(sessionDir)
		if err != nil {
			return nil, err
		}
		values := listSessionsFromDir(filepath.Clean(dir), progress, 0, 0)
		sortSessionInfos(values)
		return values, nil
	}
	root := filepath.Join(defaultAgentDir(), "sessions")
	directories, err := os.ReadDir(root)
	if err != nil {
		return []SessionInfo{}, nil
	}
	filesByDir := make([][]string, 0, len(directories))
	total := 0
	for _, directory := range directories {
		if !directory.IsDir() {
			continue
		}
		items, readErr := os.ReadDir(filepath.Join(root, directory.Name()))
		if readErr != nil {
			continue
		}
		files := make([]string, 0)
		for _, item := range items {
			if !item.IsDir() && strings.HasSuffix(item.Name(), ".jsonl") {
				files = append(files, filepath.Join(root, directory.Name(), item.Name()))
			}
		}
		filesByDir = append(filesByDir, files)
		total += len(files)
	}
	allFiles := make([]string, 0, total)
	for _, files := range filesByDir {
		allFiles = append(allFiles, files...)
	}
	values := buildSessionInfos(allFiles, progress, 0, total)
	sortSessionInfos(values)
	return values, nil
}

func listSessionsFromDir(dir string, progress SessionListProgress, offset, total int) []SessionInfo {
	items, err := os.ReadDir(dir)
	if err != nil {
		return []SessionInfo{}
	}
	files := make([]string, 0)
	for _, item := range items {
		if !item.IsDir() && strings.HasSuffix(item.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, item.Name()))
		}
	}
	if total == 0 {
		total = len(files)
	}
	return buildSessionInfos(files, progress, offset, total)
}

// buildSessionInfos mirrors pi's bounded concurrent discovery. Results retain
// directory order while progress follows completion order and is serialized
// so callers never need to make their callback concurrency-safe.
func buildSessionInfos(files []string, progress SessionListProgress, offset, total int) []SessionInfo {
	if len(files) == 0 {
		return []SessionInfo{}
	}
	if total == 0 {
		total = len(files)
	}
	results := make([]SessionInfo, len(files))
	valid := make([]bool, len(files))
	jobs := make(chan int)
	workerCount := min(len(files), maxConcurrentSessionInfoLoads)
	var workers sync.WaitGroup
	var progressMu sync.Mutex
	loaded := 0
	workers.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index], valid[index] = buildSessionInfo(files[index])
				progressMu.Lock()
				loaded++
				if progress != nil {
					progress(offset+loaded, total)
				}
				progressMu.Unlock()
			}
		}()
	}
	for index := range files {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	values := make([]SessionInfo, 0, len(files))
	for index := range results {
		if valid[index] {
			values = append(values, results[index])
		}
	}
	return values
}

func buildSessionInfo(path string) (SessionInfo, bool) {
	file, err := os.Open(path)
	if err != nil {
		return SessionInfo{}, false
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return SessionInfo{}, false
	}
	result := SessionInfo{Path: path, FirstMessage: "(no messages)"}
	allMessages := make([]string, 0)
	var lastActivity time.Time
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, maxSessionLine)
	headerFound := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(line, &object) != nil {
			continue
		}
		var typeName string
		_ = json.Unmarshal(object["type"], &typeName)
		if !headerFound {
			if typeName != "session" {
				return SessionInfo{}, false
			}
			headerFound = true
			_ = json.Unmarshal(object["id"], &result.ID)
			_ = json.Unmarshal(object["cwd"], &result.Cwd)
			_ = json.Unmarshal(object["parentSession"], &result.ParentSessionPath)
			result.HasParentSession = result.ParentSessionPath != ""
			var timestamp string
			_ = json.Unmarshal(object["timestamp"], &timestamp)
			result.Created, _ = time.Parse(time.RFC3339, timestamp)
			continue
		}
		if typeName == "session_info" {
			var name string
			if json.Unmarshal(object["name"], &name) == nil {
				name = strings.TrimSpace(name)
				result.Name, result.HasName = name, name != ""
			} else {
				result.Name, result.HasName = "", false
			}
			continue
		}
		if typeName != "message" {
			continue
		}
		result.MessageCount++
		text, role, activity, hasContent := sessionInfoMessage(object)
		if role != "user" && role != "assistant" {
			continue
		}
		if hasContent && activity.After(time.UnixMilli(0)) && activity.After(lastActivity) {
			lastActivity = activity
		}
		if text != "" {
			allMessages = append(allMessages, text)
			if result.FirstMessage == "(no messages)" && role == "user" {
				result.FirstMessage = text
			}
		}
	}
	if !headerFound || result.ID == "" || scanner.Err() != nil {
		return SessionInfo{}, false
	}
	result.AllMessagesText = strings.Join(allMessages, " ")
	if !lastActivity.IsZero() {
		result.Modified = lastActivity
	} else if !result.Created.IsZero() {
		result.Modified = result.Created
	} else {
		result.Modified = stat.ModTime()
	}
	return result, true
}

func sessionInfoMessage(entry map[string]json.RawMessage) (text, role string, activity time.Time, hasContent bool) {
	var message map[string]json.RawMessage
	if json.Unmarshal(entry["message"], &message) != nil {
		return "", "", time.Time{}, false
	}
	_ = json.Unmarshal(message["role"], &role)
	_, hasContent = message["content"]
	var milliseconds int64
	if json.Unmarshal(message["timestamp"], &milliseconds) == nil {
		activity = time.UnixMilli(milliseconds)
	} else {
		var timestamp string
		_ = json.Unmarshal(entry["timestamp"], &timestamp)
		activity, _ = time.Parse(time.RFC3339, timestamp)
	}
	var stringContent string
	if json.Unmarshal(message["content"], &stringContent) == nil {
		return stringContent, role, activity, hasContent
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil {
		return "", role, activity, hasContent
	}
	parts := make([]string, 0)
	for _, block := range blocks {
		var kind, value string
		_ = json.Unmarshal(block["type"], &kind)
		_ = json.Unmarshal(block["text"], &value)
		if kind == "text" && value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " "), role, activity, hasContent
}

func filterSessionInfosByCwd(values []SessionInfo, cwd string) []SessionInfo {
	result := make([]SessionInfo, 0, len(values))
	for _, value := range values {
		resolved, err := resolveWorkingDir(value.Cwd)
		if err == nil && resolved == cwd {
			result = append(result, value)
		}
	}
	return result
}

func sortSessionInfos(values []SessionInfo) {
	sort.SliceStable(values, func(i, j int) bool { return values[i].Modified.After(values[j].Modified) })
}

func (s SessionInfo) String() string {
	return fmt.Sprintf("%s (%s)", s.ID, s.Path)
}
