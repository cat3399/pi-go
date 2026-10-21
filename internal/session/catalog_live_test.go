package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogUsesLiveSummariesWithoutTrustingThemAsDurableFingerprints(t *testing.T) {
	root := t.TempDir()
	cwd, agentDir := filepath.Join(root, "workspace"), filepath.Join(root, "agent")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	path := createCatalogTestSession(t, cwd, dir, "live-summary", "first", 1)
	manager, err := OpenSessionManager(path, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	var loads atomic.Int64
	cachePath := filepath.Join(root, "cache", "catalog.json")
	catalog, err := newCatalogAt(agentDir, cachePath, func(path string) (SessionInfo, bool) {
		loads.Add(1)
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	// A warmed disk record must remain identifiable as stale while live data is
	// overlaid; it must not acquire a new fingerprint with the old message count.
	if _, err := catalog.ListAll(); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 1 {
		t.Fatal("fixture was not discovered")
	}
	for i := 0; i < 3; i++ {
		if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "appended", time.UnixMilli(int64(10+i)))); err != nil {
			t.Fatal(err)
		}
		info := manager.Summary()
		missing := info
		missing.ID, missing.Path = "not-persisted", filepath.Join(dir, "missing.jsonl")
		values, err := catalog.ListAllWithSummaries([]SessionInfo{info, missing})
		if err != nil {
			t.Fatal(err)
		}
		if len(values) != 1 || !reflect.DeepEqual(values[0], info) {
			t.Fatalf("live summary = %#v, want %#v", values, info)
		}
		if loads.Load() != 1 {
			t.Fatal("active file was reparsed after append")
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	values, err := catalog.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "live-summary", 5, "first")
	if loads.Load() != 2 {
		t.Fatalf("released owner did not reconcile disk: loads=%d", loads.Load())
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newCatalogAt(agentDir, cachePath, func(string) (SessionInfo, bool) { t.Error("valid snapshot reparsed file"); return SessionInfo{}, false })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	values, err = reopened.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogSession(t, values, "live-summary", 5, "first")
}

func TestCatalogColdLiveSummaryAvoidsLoadingHistory(t *testing.T) {
	root := t.TempDir()
	dir, err := SessionDirForAgentDir(root, root)
	if err != nil {
		t.Fatal(err)
	}
	path := createCatalogTestSession(t, root, dir, "cold-live", "first", 1)
	manager, err := OpenSessionManager(path, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	catalog, err := newCatalogAt(root, ":memory:", func(string) (SessionInfo, bool) { t.Error("live file was loaded"); return SessionInfo{}, false })
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	info := manager.Summary()
	values, err := catalog.ListAllWithSummaries([]SessionInfo{info})
	if err != nil || len(values) != 1 || !reflect.DeepEqual(values[0], info) {
		t.Fatalf("cold live catalog = %#v, %v", values, err)
	}
}

func TestCatalogDirectoriesRefreshHeadersWithoutWaitingForHistory(t *testing.T) {
	root := t.TempDir()
	dir, err := SessionDirForAgentDir(root, root)
	if err != nil {
		t.Fatal(err)
	}
	path := createCatalogTestSession(t, root, dir, "directory-session", "first", 1)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	catalog, err := newCatalogAt(root, ":memory:", func(path string) (SessionInfo, bool) {
		close(started)
		<-release
		return buildSessionCatalogInfo(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(release) }) }
	defer resume()
	go func() { _, err := catalog.ListAll(); finished <- err }()
	<-started
	locations := make(chan []SessionLocation, 1)
	go func() {
		value, err := catalog.ListDirectories()
		if err != nil {
			t.Error(err)
		}
		locations <- value
	}()
	select {
	case value := <-locations:
		if len(value) != 1 || value[0] != (SessionLocation{Path: path, ID: "directory-session", Cwd: root}) {
			t.Fatalf("directory projection = %#v", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("directory lookup waited for history decoding")
	}
	resume()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	manager, err := OpenSessionManager(path, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), "new title"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	value, err := catalog.ListDirectories()
	if err != nil || len(value) != 1 || value[0].Cwd != root {
		t.Fatalf("directory after append = %#v, %v", value, err)
	}
	// Replace the header with an invalid first parsed value; cached access must
	// disappear. The original test fixture is retained rather than removed.
	if err := os.Rename(path, filepath.Join(root, "preserved-session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"type\":\"not-a-session\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	value, err = catalog.ListDirectories()
	if err != nil || len(value) != 0 {
		t.Fatalf("invalid header retained access: %#v, %v", value, err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ListDirectories(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed directories = %v", err)
	}
}

func TestCatalogDirectoriesAcceptTheFullCatalogHeaderFormat(t *testing.T) {
	root := t.TempDir()
	dir, err := SessionDirForAgentDir(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Unknown header metadata is preserved by pi; a directory query must not
	// inherit the recent-session picker's bounded header probe and lose access.
	header, err := json.Marshal(map[string]any{
		"type": "session", "version": 3, "id": "large-header", "cwd": root,
		"timestamp": "2026-08-01T00:00:00.000Z", "metadata": strings.Repeat("x", (1<<20)+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "large-header.jsonl")
	if err := os.WriteFile(path, append(append([]byte("malformed record\n"), header...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := newCatalogAt(root, ":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	full, err := catalog.ListAll()
	if err != nil || len(full) != 1 {
		t.Fatalf("full header discovery = %#v, %v", full, err)
	}
	directories, err := catalog.ListDirectories()
	if err != nil || len(directories) != 1 || directories[0].Cwd != full[0].Cwd || directories[0].ID != full[0].ID {
		t.Fatalf("header-only discovery = %#v, %v", directories, err)
	}
}
