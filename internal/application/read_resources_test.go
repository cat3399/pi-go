package application_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

func TestHistoryQueriesReuseVerifiedIndexWithoutOpeningRuntimeOrWriter(t *testing.T) {
	corpus := makeDataBenchmarkCorpus(t, dataBenchmarkShape{name: "pages", sessions: 1, messages: 240, textBytes: 64, compacted: true})
	api, managers := openDataBenchmarkService(t, corpus)
	const readers = 12
	versions := make(chan session.ReadVersion, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			value, err := api.ReadHistory(corpus.firstID, session.HistoryQuery{Limit: 10})
			if err != nil {
				t.Error(err)
				return
			}
			if value.Page.Total != 241 || len(value.Page.Items) != 10 || value.Page.Source.Tree != nil || value.Page.Source.Entries != nil {
				t.Errorf("bounded history = %#v", value.Page)
			}
			versions <- value.Page.Version
		})
	}
	wg.Wait()
	close(versions)
	var first session.ReadVersion
	for version := range versions {
		if first.Generation == "" {
			first = version
		} else if version != first {
			t.Fatalf("concurrent reads loaded separate index generations: %#v, %#v", first, version)
		}
	}
	if len(managers) != 0 {
		t.Fatal("history read admitted an inactive Runtime")
	}
	metadata, err := api.ReadSessionMetadata(corpus.firstID)
	if err != nil || metadata.Version != first {
		t.Fatalf("metadata = %#v, %v", metadata, err)
	}
	writer, err := session.OpenSessionManager(metadata.FilePath, filepath.Dir(metadata.FilePath), "")
	if err != nil {
		t.Fatalf("cached index held a writer lock: %v", err)
	}
	message, _ := llm.NewUserTextMessage("external append", time.UnixMilli(3_000_000_000_000))
	if _, err := writer.AppendLLMMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := api.ReadHistory(corpus.firstID, session.HistoryQuery{Limit: 10})
	if err != nil || after.Page.Total != 242 || after.Page.Version.Generation == first.Generation || after.Page.Items[len(after.Page.Items)-1].Node.Preview != "external append" {
		t.Fatalf("external append did not invalidate index: %#v, %v", after, err)
	}
}

func TestHistoryCursorSurvivesPayloadEvictionAndRejectsSourceReplacement(t *testing.T) {
	corpus := makeDataBenchmarkCorpus(t, dataBenchmarkShape{name: "eviction", sessions: 1, messages: 240, textBytes: 64})
	api, _ := openDataBenchmarkService(t, corpus, 1)
	first, err := api.ReadHistory(corpus.firstID, session.HistoryQuery{Limit: 10})
	if err != nil || first.Page.Older == nil {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	older, err := api.ReadHistory(corpus.firstID, session.HistoryQuery{Cursor: first.Page.Older, Limit: 10})
	if err != nil || older.Page.Version != first.Page.Version || older.Page.End != first.Page.Start {
		t.Fatalf("payload budget prevented paging: %#v, %v", older, err)
	}
	path := first.Page.Source.Info.Path
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Keep every identity/length/mtime; change actual content in place.
	replaced := bytes.ReplaceAll(raw, []byte("fixture"), []byte("changed"))
	if bytes.Equal(raw, replaced) {
		t.Fatal("fixture did not contain replaceable content")
	}
	if err := os.WriteFile(path, replaced, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ReadHistory(corpus.firstID, session.HistoryQuery{Cursor: first.Page.Older}); !errors.Is(err, session.ErrReadCursorUnavailable) {
		t.Fatalf("source replacement accepted a stale page cursor: %v", err)
	}
	latest, err := api.ReadHistory(corpus.firstID, session.HistoryQuery{Limit: 10})
	if err != nil || latest.Page.Version.Generation == first.Page.Version.Generation {
		t.Fatalf("replacement kept old read generation: %#v, %v", latest, err)
	}
}

func TestActiveHistoryPagesAndLiveSnapshotShareCurrentManagerGeneration(t *testing.T) {
	corpus := makeDataBenchmarkCorpus(t, dataBenchmarkShape{name: "active-pages", sessions: 1, messages: 240, textBytes: 64})
	api, _ := openDataBenchmarkService(t, corpus)
	if _, err := api.Dispatch(context.Background(), corpus.firstID, application.GetStateCommand{}); err != nil {
		t.Fatal(err)
	}
	live, active, err := api.ReadLiveSession(corpus.firstID)
	if err != nil || !active {
		t.Fatalf("live = %#v, %t, %v", live, active, err)
	}
	page, err := api.ReadHistory(corpus.firstID, session.HistoryQuery{At: &live.State.History.Version, LeafID: live.State.History.LeafID, Limit: 10})
	if err != nil || page.Page.Version != live.State.History.Version || page.Page.End != live.State.Agent.MessageCount {
		t.Fatalf("live/history split generations: %#v, %v", page, err)
	}
}
