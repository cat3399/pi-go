package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

func TestDirectoryPagesKeepOrderingThroughActivityAndRenames(t *testing.T) {
	corpus := makeDataBenchmarkCorpus(t, dataBenchmarkShape{name: "directory", sessions: 12, messages: 4, textBytes: 64})
	api, managers := openDataBenchmarkService(t, corpus)
	original, err := api.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	first, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: corpus.cwd, Limit: 3})
	if err != nil || first.Total != 12 || len(first.Sessions) != 3 || first.Next == nil {
		t.Fatalf("first = %#v, %v", first, err)
	}
	// Update a row that has not been loaded yet. Its current ordering changes,
	// but the cursor must continue through the original immutable directory.
	changed := original[len(original)-1].ID
	if err := api.RenameSession(context.Background(), changed, "renamed old session"); err != nil {
		t.Fatal(err)
	}
	renamed, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: corpus.cwd, Search: "RENAMED OLD"})
	if err != nil || renamed.Total != 1 || !renamed.Sessions[0].Modified.Equal(original[len(original)-1].Modified) {
		t.Fatal("rename changed message activity time")
	}
	path := original[len(original)-1].Path
	writer, err := session.OpenSessionManager(path, filepath.Dir(path), "")
	if err != nil {
		t.Fatal(err)
	}
	message, _ := llm.NewUserTextMessage("new activity", time.UnixMilli(3_000_000_000_000))
	if _, err := writer.AppendLLMMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	values := append([]application.SessionInfo{}, first.Sessions...)
	cursor := first.Next
	for cursor != nil {
		page, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: corpus.cwd, Limit: 3, Cursor: cursor})
		if err != nil || page.Version != first.Version || page.Start != len(values) {
			t.Fatalf("next = %#v, %v", page, err)
		}
		values = append(values, page.Sessions...)
		cursor = page.Next
	}
	if len(values) != len(original) {
		t.Fatalf("lost rows: %d", len(values))
	}
	for i := range original {
		if values[i] != original[i] {
			t.Fatalf("row %d moved or changed", i)
		}
	}
	latest, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: corpus.cwd, Limit: 3})
	if err != nil || latest.Version == first.Version || latest.Sessions[0].ID != changed {
		t.Fatalf("latest = %#v, %v", latest, err)
	}
	matching, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: corpus.cwd, Search: "RENAMED OLD"})
	if err != nil || matching.Total != 1 || matching.Sessions[0].ID != changed {
		t.Fatalf("search = %#v, %v", matching, err)
	}
	if _, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Search: "other", Cursor: first.Next}); !errors.Is(err, session.ErrInvalidReadQuery) {
		t.Fatalf("cross-query cursor: %v", err)
	}
	first.Sessions[0].Name = "caller changed copy"
	copy, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: corpus.cwd, Cursor: &application.SessionDirectoryCursor{Index: first.Version, Offset: 0}})
	if err != nil || copy.Sessions[0].Name == first.Sessions[0].Name {
		t.Fatal("caller mutated immutable index")
	}
	if len(managers) != 0 {
		t.Fatal("directory admitted a Runtime")
	}
}

func TestOverviewTokenExcludesEventAnchorAndDirectoryIsBounded(t *testing.T) {
	corpus := makeDataBenchmarkCorpus(t, dataBenchmarkShape{name: "overview", sessions: 8, messages: 4, textBytes: 64})
	api, _ := openDataBenchmarkService(t, corpus)
	before, err := surfacewire.ReadApplicationResource(api, "overview", surfacewire.ApplicationReadRequest{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var overview surfacewire.Overview
	if err := json.Unmarshal(before.Data, &overview); err != nil {
		t.Fatal(err)
	}
	if overview.SessionCount != 8 || len(overview.Projects) != 1 || before.Cursor == "" {
		t.Fatalf("overview = %s", before.Data)
	}
	if _, err := api.Dispatch(context.Background(), corpus.firstID, application.GetToolsCommand{}); err != nil {
		t.Fatal(err)
	}
	after, err := surfacewire.ReadApplicationResource(api, "overview", surfacewire.ApplicationReadRequest{}, before.Token)
	if err != nil || !after.Unchanged || after.Cursor == before.Cursor {
		t.Fatalf("unrelated event invalidated overview: %#v %v", after, err)
	}
	page, err := surfacewire.ReadApplicationResource(api, "directory", surfacewire.ApplicationReadRequest{Limit: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	var directory surfacewire.SessionDirectoryPage
	if err := json.Unmarshal(page.Data, &directory); err != nil {
		t.Fatal(err)
	}
	if len(directory.Sessions) != 2 || directory.Total != 8 || directory.Next == "" {
		t.Fatalf("page = %s", page.Data)
	}
	validated, err := surfacewire.ReadApplicationResource(api, "directory", surfacewire.ApplicationReadRequest{Limit: 2}, page.Token)
	if err != nil || !validated.Unchanged {
		t.Fatalf("unchanged = %#v %v", validated, err)
	}
}
