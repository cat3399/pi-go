package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogProjectionMatchesCompatibleDiscoveryMetadata(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	parentPath := createCatalogTestSession(t, cwd, sessionDir, "catalog-parent", "parent message", 1)
	zone := time.FixedZone("catalog-offset", 8*60*60)
	created := time.Date(2026, 8, 17, 12, 34, 56, 789000000, zone)
	manager, err := CreateSessionManagerWithOptions(cwd, sessionDir, ManagerOptions{
		NewSession: NewSessionOptions{ID: "catalog-rich", ParentSession: parentPath},
		Now:        sequenceClock(created),
		NewEntryID: sequenceIDs("catalog-user", "catalog-assistant", "catalog-name"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "rich first", created.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "rich reply", created.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), "  Catalog Name  "); err != nil {
		t.Fatal(err)
	}
	path, ok := manager.SessionFile()
	if !ok {
		t.Fatal("rich session is not persistent")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	compatible, compatibleOK := buildSessionInfo(path)
	projection, projectionOK := buildSessionCatalogInfo(path)
	if !compatibleOK || !projectionOK {
		t.Fatalf("projection validity = compatible:%v catalog:%v", compatibleOK, projectionOK)
	}
	compatible.AllMessagesText = ""
	if !reflect.DeepEqual(projection, compatible) {
		t.Fatalf("catalog projection differs:\n got  %#v\n want %#v", projection, compatible)
	}

	catalog, err := newCatalogAt(agentDir, filepath.Join(root, "cache", "catalog.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ListAll(); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newCatalogAt(agentDir, filepath.Join(root, "cache", "catalog.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	values, err := reopened.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	stored, found := catalogSessionByID(values, "catalog-rich")
	if !found {
		t.Fatalf("stored sessions = %#v", values)
	}
	if !reflect.DeepEqual(stored, projection) {
		t.Fatalf("snapshot round trip differs:\n got  %#v\n want %#v", stored, projection)
	}
}

func TestCatalogPersistsAndRefreshesOnlyChangedSessionFiles(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := createCatalogTestSession(t, cwd, sessionDir, "catalog-first", "first message", 1)
	if direct, ok := buildSessionCatalogInfo(firstPath); !ok {
		t.Fatal("direct catalog projection rejected a valid session")
	} else if direct.ID != "catalog-first" {
		t.Fatalf("direct catalog projection = %#v", direct)
	}
	snapshotPath := filepath.Join(root, "cache", "session-catalog.json")

	var initialLoads atomic.Int64
	catalog, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		initialLoads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.persistent {
		t.Fatal("catalog unexpectedly fell back to memory-only storage")
	}
	values, err := catalog.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-first", 2, "first message")
	if values[0].AllMessagesText != "" {
		t.Fatalf("catalog retained aggregate message text %q", values[0].AllMessagesText)
	}
	if got := initialLoads.Load(); got != 1 {
		t.Fatalf("initial parse count = %d", got)
	}
	if _, err := catalog.ListAll(); err != nil {
		t.Fatal(err)
	}
	if got := initialLoads.Load(); got != 1 {
		t.Fatalf("unchanged file was reparsed, count = %d", got)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}

	var reopenedLoads atomic.Int64
	reopened, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		reopenedLoads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	values, err = reopened.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-first", 2, "first message")
	if got := reopenedLoads.Load(); got != 0 {
		t.Fatalf("persistent catalog reparsed unchanged files after reopen, count = %d", got)
	}

	manager, err := OpenSessionManager(firstPath, sessionDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "second message", time.UnixMilli(2))); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	values, err = reopened.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-first", 3, "first message")
	if got := reopenedLoads.Load(); got != 1 {
		t.Fatalf("changed file parse count = %d", got)
	}

	secondPath := createCatalogTestSession(t, cwd, sessionDir, "catalog-second", "new session", 3)
	values, err = reopened.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 {
		t.Fatalf("sessions after create = %#v", values)
	}
	if got := reopenedLoads.Load(); got != 2 {
		t.Fatalf("new file parse count = %d", got)
	}

	archiveDir := filepath.Join(root, "archived")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(firstPath, filepath.Join(archiveDir, filepath.Base(firstPath))); err != nil {
		t.Fatal(err)
	}
	values, err = reopened.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-second", 2, "new session")
	if got := reopenedLoads.Load(); got != 2 {
		t.Fatalf("removing a file reparsed an unchanged peer, count = %d", got)
	}
	if values[0].Path != secondPath {
		t.Fatalf("remaining session path = %q, want %q", values[0].Path, secondPath)
	}
}

func TestCatalogCoalescesConcurrentRefreshes(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	createCatalogTestSession(t, cwd, sessionDir, "catalog-concurrent", "shared refresh", 1)

	var loads atomic.Int64
	catalog, err := newCatalog(agentDir, func(path string) (SessionInfo, bool) {
		loads.Add(1)
		time.Sleep(10 * time.Millisecond)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	initialCatalog := catalog
	t.Cleanup(func() { _ = initialCatalog.Close() })

	const callers = 24
	start := make(chan struct{})
	results := make([][]SessionInfo, callers)
	errorsByCaller := make([]error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer group.Done()
			<-start
			results[index], errorsByCaller[index] = catalog.ListAll()
		}()
	}
	close(start)
	group.Wait()
	for index := range callers {
		if errorsByCaller[index] != nil {
			t.Fatalf("caller %d: %v", index, errorsByCaller[index])
		}
		assertCatalogSession(t, results[index], "catalog-concurrent", 2, "shared refresh")
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("concurrent parse count = %d", got)
	}
}

func TestCatalogLargeCorpusRefreshIsIncremental(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	const sessionCount = 160
	paths := make([]string, sessionCount)
	for index := range sessionCount {
		id := fmt.Sprintf("catalog-large-%03d", index)
		paths[index] = createCatalogTestSession(t, cwd, sessionDir, id, "message "+id, int64(index*2+1))
	}

	var loads atomic.Int64
	snapshotPath := filepath.Join(root, "cache", "large.json")
	catalog, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		loads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	coldStarted := time.Now()
	values, err := catalog.ListAll()
	coldElapsed := time.Since(coldStarted)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != sessionCount || loads.Load() != sessionCount {
		t.Fatalf("cold catalog: sessions=%d parses=%d", len(values), loads.Load())
	}
	warmStarted := time.Now()
	if _, err := catalog.ListAll(); err != nil {
		t.Fatal(err)
	}
	warmElapsed := time.Since(warmStarted)
	if got := loads.Load(); got != sessionCount {
		t.Fatalf("warm catalog reparsed files, count = %d", got)
	}

	const changedIndex = 73
	manager, err := OpenSessionManager(paths[changedIndex], sessionDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "only changed session", time.UnixMilli(1000))); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	incrementalStarted := time.Now()
	values, err = catalog.ListAll()
	incrementalElapsed := time.Since(incrementalStarted)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != sessionCount {
		t.Fatalf("incremental catalog sessions = %d", len(values))
	}
	if got := loads.Load(); got != sessionCount+1 {
		t.Fatalf("one file change caused %d total parses, want %d", got, sessionCount+1)
	}
	changed, found := catalogSessionByID(values, fmt.Sprintf("catalog-large-%03d", changedIndex))
	if !found || changed.MessageCount != 3 {
		t.Fatalf("changed session = %#v, found=%v", changed, found)
	}
	t.Logf("160-session catalog timings: cold=%s warm=%s one-changed=%s", coldElapsed, warmElapsed, incrementalElapsed)
}

func TestCatalogRealCorpusOptIn(t *testing.T) {
	agentDirValue := os.Getenv("PI_GO_REAL_SESSION_CORPUS")
	if agentDirValue == "" {
		t.Skip("set PI_GO_REAL_SESSION_CORPUS to a copied agent directory")
	}
	agentDir, err := filepath.Abs(agentDirValue)
	if err != nil {
		t.Fatal(err)
	}
	var bytesPerSecond int64
	if value := os.Getenv("PI_GO_REAL_SESSION_BYTES_PER_SECOND"); value != "" {
		bytesPerSecond, err = strconv.ParseInt(value, 10, 64)
		if err != nil || bytesPerSecond <= 0 {
			t.Fatalf("invalid PI_GO_REAL_SESSION_BYTES_PER_SECOND %q", value)
		}
	}
	var seekDelay time.Duration
	if value := os.Getenv("PI_GO_REAL_SESSION_SEEK_DELAY"); value != "" {
		seekDelay, err = time.ParseDuration(value)
		if err != nil || seekDelay < 0 {
			t.Fatalf("invalid PI_GO_REAL_SESSION_SEEK_DELAY %q", value)
		}
	}

	var loads atomic.Int64
	var loadedBytes atomic.Int64
	var slowIO sync.Mutex
	load := func(path string) (SessionInfo, bool) {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return SessionInfo{}, false
		}
		loads.Add(1)
		loadedBytes.Add(info.Size())
		delay := seekDelay
		if bytesPerSecond != 0 {
			delay += time.Duration(info.Size()) * time.Second / time.Duration(bytesPerSecond)
		}
		if delay == 0 {
			return buildSessionCatalogInfo(path)
		}
		// Serialize the artificial delay and read to model one shared slow disk,
		// rather than incorrectly granting each parser its own bandwidth.
		slowIO.Lock()
		defer slowIO.Unlock()
		time.Sleep(delay)
		return buildSessionCatalogInfo(path)
	}

	snapshotPath := filepath.Join(t.TempDir(), "real-corpus.json")
	var snapshotStore catalogSnapshotStore = osCatalogSnapshotStore{}
	if bytesPerSecond != 0 || seekDelay != 0 {
		snapshotStore = simulatedSlowCatalogSnapshotStore{
			delegate: osCatalogSnapshotStore{}, bytesPerSecond: bytesPerSecond, latency: seekDelay,
		}
	}
	coldStarted := time.Now()
	catalog, err := newCatalogAtWithStore(agentDir, snapshotPath, load, snapshotStore)
	if err != nil {
		t.Fatal(err)
	}
	cold, err := catalog.ListAll()
	coldElapsed := time.Since(coldStarted)
	if err != nil {
		t.Fatal(err)
	}
	if len(cold) == 0 || loads.Load() == 0 || loadedBytes.Load() == 0 {
		t.Fatalf("real corpus did not contain usable sessions: sessions=%d loads=%d bytes=%d", len(cold), loads.Load(), loadedBytes.Load())
	}
	coldLoads := loads.Load()
	coldBytes := loadedBytes.Load()
	warmStarted := time.Now()
	warm, err := catalog.ListAll()
	warmElapsed := time.Since(warmStarted)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(warm, cold) || loads.Load() != coldLoads {
		t.Fatalf("warm real corpus changed: sessions=%d loads=%d, want sessions=%d loads=%d", len(warm), loads.Load(), len(cold), coldLoads)
	}
	stableLoads := coldLoads
	var incrementalElapsed time.Duration
	var changedBytes int64
	if os.Getenv("PI_GO_REAL_SESSION_TOUCH_LARGEST") == "1" {
		largestPath := ""
		var largestInfo os.FileInfo
		for _, value := range cold {
			info, statErr := os.Stat(value.Path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if largestInfo == nil || info.Size() > largestInfo.Size() {
				largestPath, largestInfo = value.Path, info
			}
		}
		changedBytes = largestInfo.Size()
		changedTime := largestInfo.ModTime().Add(time.Second)
		if err := os.Chtimes(largestPath, changedTime, changedTime); err != nil {
			t.Fatal(err)
		}
		incrementalStarted := time.Now()
		incremental, listErr := catalog.ListAll()
		incrementalElapsed = time.Since(incrementalStarted)
		if listErr != nil {
			t.Fatal(listErr)
		}
		stableLoads++
		if !reflect.DeepEqual(incremental, cold) || loads.Load() != stableLoads {
			t.Fatalf("single-file refresh changed: sessions=%d loads=%d, want sessions=%d loads=%d", len(incremental), loads.Load(), len(cold), stableLoads)
		}
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}

	reopenStarted := time.Now()
	reopened, err := newCatalogAtWithStore(agentDir, snapshotPath, load, snapshotStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopenedValues, err := reopened.ListAll()
	reopenElapsed := time.Since(reopenStarted)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopenedValues, cold) || loads.Load() != stableLoads {
		t.Fatalf("reopened real corpus changed: sessions=%d loads=%d, want sessions=%d loads=%d", len(reopenedValues), loads.Load(), len(cold), stableLoads)
	}
	snapshotInfo, err := os.Stat(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"real corpus: sessions=%d files=%d source_bytes=%d snapshot_bytes=%d cold=%s warm=%s one_changed=%s changed_bytes=%d reopen=%s simulated_bps=%d seek=%s",
		len(cold), coldLoads, coldBytes, snapshotInfo.Size(), coldElapsed, warmElapsed, incrementalElapsed, changedBytes, reopenElapsed, bytesPerSecond, seekDelay,
	)
}

func TestCatalogCachesInvalidFilesAndRecoversAfterReplacement(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	invalidDir := filepath.Join(agentDir, "sessions", "invalid-bucket")
	if err := os.MkdirAll(invalidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invalidPath := filepath.Join(invalidDir, "broken.jsonl")
	if err := os.WriteFile(invalidPath, []byte("not a session\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var loads atomic.Int64
	snapshotPath := filepath.Join(root, "cache", "invalid.json")
	catalog, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		loads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	initialInvalidCatalog := catalog
	t.Cleanup(func() { _ = initialInvalidCatalog.Close() })
	for attempt := 0; attempt < 2; attempt++ {
		values, listErr := catalog.ListAll()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(values) != 0 {
			t.Fatalf("invalid catalog values = %#v", values)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("invalid unchanged file parse count = %d", got)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	catalog, err = newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		loads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	values, err := catalog.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 || loads.Load() != 1 {
		t.Fatalf("persisted invalid file: values=%#v loads=%d", values, loads.Load())
	}

	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	validPath := createCatalogTestSession(t, cwd, sessionDir, "catalog-recovered", "recovered", 1)
	archiveDir := filepath.Join(root, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(invalidPath, filepath.Join(archiveDir, "broken.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(validPath, invalidPath); err != nil {
		t.Fatal(err)
	}
	values, err = catalog.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-recovered", 2, "recovered")
	if got := loads.Load(); got != 2 {
		t.Fatalf("replacement parse count = %d", got)
	}
}

func TestCatalogTemporaryAgentDirectoryStaysMemoryOnly(t *testing.T) {
	agentDir := filepath.Join(t.TempDir(), "not-created-agent")
	catalog, err := NewCatalog(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	if catalog.persistent {
		t.Fatal("temporary catalog unexpectedly persisted")
	}
	values, err := catalog.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("sessions = %#v", values)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("catalog created the temporary agent directory: %v", err)
	}
}

func TestCatalogCoordinatesIndependentSnapshotWriters(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	path := createCatalogTestSession(t, cwd, sessionDir, "catalog-shared-db", "first", 1)
	snapshotPath := filepath.Join(root, "cache", "shared.json")
	first, err := newCatalogAt(agentDir, snapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := newCatalogAt(agentDir, snapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := first.ListAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ListAll(); err != nil {
		t.Fatal(err)
	}

	manager, err := OpenSessionManager(path, sessionDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "changed", time.UnixMilli(3))); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make([][]SessionInfo, 2)
	errorsByCatalog := make([]error, 2)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		results[0], errorsByCatalog[0] = first.ListAll()
	}()
	go func() {
		defer group.Done()
		<-start
		results[1], errorsByCatalog[1] = second.ListAll()
	}()
	close(start)
	group.Wait()
	for index := range results {
		if errorsByCatalog[index] != nil {
			t.Fatalf("catalog %d: %v", index, errorsByCatalog[index])
		}
		assertCatalogSession(t, results[index], "catalog-shared-db", 3, "first")
	}

	var reopenedLoads atomic.Int64
	reopened, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		reopenedLoads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	values, err := reopened.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-shared-db", 3, "first")
	if got := reopenedLoads.Load(); got != 0 {
		t.Fatalf("valid concurrently-written snapshot reparsed %d files", got)
	}
}

func TestCatalogRebuildsInvalidSnapshots(t *testing.T) {
	encode := func(t *testing.T, snapshot catalogSnapshot) []byte {
		t.Helper()
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	cases := []struct {
		name string
		data func(*testing.T, string) []byte
	}{
		{name: "malformed", data: func(_ *testing.T, _ string) []byte { return []byte("{") }},
		{name: "unsupported version", data: func(t *testing.T, agentDir string) []byte {
			return encode(t, catalogSnapshot{Version: sessionCatalogSnapshotVersion + 1, AgentDir: agentDir})
		}},
		{name: "different agent directory", data: func(t *testing.T, agentDir string) []byte {
			return encode(t, catalogSnapshot{Version: sessionCatalogSnapshotVersion, AgentDir: agentDir + "-other"})
		}},
		{name: "outside session root", data: func(t *testing.T, agentDir string) []byte {
			return encode(t, catalogSnapshot{
				Version: sessionCatalogSnapshotVersion, AgentDir: agentDir,
				Entries: []catalogSnapshotEntry{{
					Path: filepath.Join(filepath.Dir(agentDir), "outside.jsonl"), FileSize: 1,
				}},
			})
		}},
		{name: "trailing value", data: func(t *testing.T, agentDir string) []byte {
			valid := encode(t, catalogSnapshot{Version: sessionCatalogSnapshotVersion, AgentDir: agentDir})
			return append(valid, []byte("{}")...)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := filepath.Join(root, "workspace")
			agentDir := filepath.Join(root, "agent")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
			if err != nil {
				t.Fatal(err)
			}
			createCatalogTestSession(t, cwd, sessionDir, "catalog-rebuilt", "rebuilt", 1)
			snapshotPath := filepath.Join(root, "cache", "catalog.json")
			if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(snapshotPath, testCase.data(t, agentDir), 0o600); err != nil {
				t.Fatal(err)
			}

			var loads atomic.Int64
			catalog, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
				loads.Add(1)
				return buildSessionCatalogInfo(path)
			})
			if err != nil {
				t.Fatal(err)
			}
			values, err := catalog.ListAll()
			if err != nil {
				t.Fatal(err)
			}
			assertCatalogSession(t, values, "catalog-rebuilt", 2, "rebuilt")
			if got := loads.Load(); got != 1 {
				t.Fatalf("rebuild parse count = %d", got)
			}
			if err := catalog.Close(); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(snapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			records, err := decodeCatalogSnapshot(agentDir, data)
			if err != nil || len(records) != 1 {
				t.Fatalf("rebuilt snapshot: records=%d err=%v", len(records), err)
			}
			var reopenedLoads atomic.Int64
			reopened, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
				reopenedLoads.Add(1)
				return buildSessionCatalogInfo(path)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if _, err := reopened.ListAll(); err != nil {
				t.Fatal(err)
			}
			if got := reopenedLoads.Load(); got != 0 {
				t.Fatalf("repaired snapshot reparsed %d files", got)
			}
		})
	}
}

func TestCatalogRepairsInvalidSnapshotWithoutSessionChanges(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	snapshotPath := filepath.Join(root, "cache", "catalog.json")
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := newCatalogAt(agentDir, snapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if values, err := catalog.ListAll(); err != nil || len(values) != 0 {
		t.Fatalf("empty repair: values=%#v err=%v", values, err)
	}
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	records, err := decodeCatalogSnapshot(agentDir, data)
	if err != nil || len(records) != 0 {
		t.Fatalf("empty repaired snapshot: records=%d err=%v", len(records), err)
	}
}

func TestCatalogSnapshotWriteFailureIsBestEffortAndRetried(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	createCatalogTestSession(t, cwd, sessionDir, "catalog-best-effort", "available", 1)
	store := &memoryCatalogSnapshotStore{writeErrors: []error{errors.New("disk full")}}
	snapshotPath := filepath.Join(root, "cache", "catalog.json")
	var loads atomic.Int64
	catalog, err := newCatalogAtWithStore(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		loads.Add(1)
		return buildSessionCatalogInfo(path)
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	values, err := catalog.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-best-effort", 2, "available")
	if !catalog.rewriteSnapshot || store.writeCount() != 1 {
		t.Fatalf("failed write state: retry=%v writes=%d", catalog.rewriteSnapshot, store.writeCount())
	}
	values, err = catalog.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "catalog-best-effort", 2, "available")
	if catalog.rewriteSnapshot || store.writeCount() != 2 || loads.Load() != 1 {
		t.Fatalf("retry state: retry=%v writes=%d loads=%d", catalog.rewriteSnapshot, store.writeCount(), loads.Load())
	}
	if _, err := catalog.ListAll(); err != nil {
		t.Fatal(err)
	}
	if store.writeCount() != 2 {
		t.Fatalf("unchanged warm read wrote snapshot %d times", store.writeCount())
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}

	var reopenedLoads atomic.Int64
	reopened, err := newCatalogAtWithStore(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		reopenedLoads.Add(1)
		return buildSessionCatalogInfo(path)
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.ListAll(); err != nil {
		t.Fatal(err)
	}
	if reopenedLoads.Load() != 0 {
		t.Fatalf("successful retry was not reusable, parses=%d", reopenedLoads.Load())
	}
}

func TestCatalogRepairsSnapshotAfterStaleConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	createCatalogTestSession(t, cwd, sessionDir, "catalog-old", "old", 1)
	snapshotPath := filepath.Join(root, "cache", "catalog.json")
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	first, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		close(entered)
		<-release
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	var firstValues []SessionInfo
	var firstErr error
	firstDone := make(chan struct{})
	go func() {
		firstValues, firstErr = first.ListAll()
		close(firstDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stale writer did not enter loader")
	}

	createCatalogTestSession(t, cwd, sessionDir, "catalog-new", "new", 3)
	second, err := newCatalogAt(agentDir, snapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	secondValues, err := second.ListAll()
	if err != nil || len(secondValues) != 2 {
		t.Fatalf("newer writer: sessions=%d err=%v", len(secondValues), err)
	}
	close(release)
	released = true
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stale writer did not finish")
	}
	if firstErr != nil || len(firstValues) != 1 {
		t.Fatalf("stale writer result: sessions=%d err=%v", len(firstValues), firstErr)
	}

	var repairLoads atomic.Int64
	repair, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		repairLoads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := repair.ListAll()
	if err != nil || len(repaired) != 2 {
		t.Fatalf("repair result: sessions=%d err=%v", len(repaired), err)
	}
	if got := repairLoads.Load(); got != 1 {
		t.Fatalf("repair parsed %d files, want only the missing entry", got)
	}
	if err := repair.Close(); err != nil {
		t.Fatal(err)
	}

	var finalLoads atomic.Int64
	finalCatalog, err := newCatalogAt(agentDir, snapshotPath, func(path string) (SessionInfo, bool) {
		finalLoads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = finalCatalog.Close() })
	finalValues, err := finalCatalog.ListAll()
	if err != nil || len(finalValues) != 2 || finalLoads.Load() != 0 {
		t.Fatalf("final snapshot: sessions=%d parses=%d err=%v", len(finalValues), finalLoads.Load(), err)
	}
}

func TestCatalogCloseIsIdempotentAndRejectsReads(t *testing.T) {
	catalog, err := newCatalogAt(filepath.Join(t.TempDir(), "agent"), ":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ListAll(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListAll after Close error = %v", err)
	}
}

func TestCatalogSnapshotEncodingIsDeterministic(t *testing.T) {
	agentDir := filepath.Join(t.TempDir(), "agent")
	root := filepath.Join(agentDir, "sessions", "bucket")
	firstPath := filepath.Join(root, "a.jsonl")
	secondPath := filepath.Join(root, "b.jsonl")
	modified := time.Date(2026, 8, 17, 20, 0, 0, 123, time.UTC)
	records := map[string]catalogRecord{
		secondPath: {
			fingerprint: catalogFingerprint{size: 2, modifiedNS: 20},
			valid:       true,
			info: SessionInfo{
				Path: secondPath, ID: "second", Cwd: "/workspace", Modified: modified,
			},
		},
		firstPath: {
			fingerprint: catalogFingerprint{size: 1, modifiedNS: 10},
			valid:       false,
		},
	}
	first, err := encodeCatalogSnapshot(agentDir, records)
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeCatalogSnapshot(agentDir, records)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("snapshot encoding changed without a state change:\n%s\n%s", first, second)
	}
	var snapshot catalogSnapshot
	if err := json.Unmarshal(first, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Entries) != 2 || snapshot.Entries[0].Path != firstPath || snapshot.Entries[1].Path != secondPath {
		t.Fatalf("snapshot entry order = %#v", snapshot.Entries)
	}
	decoded, err := decodeCatalogSnapshot(agentDir, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(records) || decoded[secondPath].info.AllMessagesText != "" {
		t.Fatalf("decoded snapshot = %#v", decoded)
	}
}

type memoryCatalogSnapshotStore struct {
	mu          sync.Mutex
	data        []byte
	writes      int
	writeErrors []error
}

type simulatedSlowCatalogSnapshotStore struct {
	delegate       catalogSnapshotStore
	bytesPerSecond int64
	latency        time.Duration
}

func (store simulatedSlowCatalogSnapshotStore) Read(path string) ([]byte, error) {
	data, err := store.delegate.Read(path)
	store.wait(int64(len(data)))
	return data, err
}

func (store simulatedSlowCatalogSnapshotStore) Write(path string, data []byte) error {
	store.wait(int64(len(data)))
	return store.delegate.Write(path, data)
}

func (store simulatedSlowCatalogSnapshotStore) wait(size int64) {
	delay := store.latency
	if store.bytesPerSecond != 0 {
		delay += time.Duration(size) * time.Second / time.Duration(store.bytesPerSecond)
	}
	if delay != 0 {
		time.Sleep(delay)
	}
}

func (store *memoryCatalogSnapshotStore) Read(string) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.data == nil {
		return nil, os.ErrNotExist
	}
	return bytes.Clone(store.data), nil
}

func (store *memoryCatalogSnapshotStore) Write(_ string, data []byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.writes++
	if len(store.writeErrors) != 0 {
		err := store.writeErrors[0]
		store.writeErrors = store.writeErrors[1:]
		if err != nil {
			return err
		}
	}
	store.data = bytes.Clone(data)
	return nil
}

func (store *memoryCatalogSnapshotStore) writeCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.writes
}

func createCatalogTestSession(t *testing.T, cwd, sessionDir, id, text string, timestamp int64) string {
	t.Helper()
	manager, err := CreateSessionManager(cwd, sessionDir, NewSessionOptions{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, text, time.UnixMilli(timestamp))); err != nil {
		_ = manager.Close()
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "assistant response", time.UnixMilli(timestamp+1))); err != nil {
		_ = manager.Close()
		t.Fatal(err)
	}
	path, ok := manager.SessionFile()
	if !ok {
		_ = manager.Close()
		t.Fatal("session is not persistent")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCatalogSession(t *testing.T, values []SessionInfo, id string, messageCount int, firstMessage string) {
	t.Helper()
	if len(values) != 1 {
		t.Fatalf("sessions = %#v", values)
	}
	if values[0].ID != id || values[0].MessageCount != messageCount || values[0].FirstMessage != firstMessage {
		t.Fatalf("session = %#v", values[0])
	}
}

func catalogSessionByID(values []SessionInfo, id string) (SessionInfo, bool) {
	for _, value := range values {
		if value.ID == id {
			return value, true
		}
	}
	return SessionInfo{}, false
}
