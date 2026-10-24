package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

func TestProjectDirectorySeedsSessionsAndKeepsBothCursorLevelsStable(t *testing.T) {
	corpus := makeDataBenchmarkCorpus(t, dataBenchmarkShape{name: "grouped-directory", sessions: 15 * 12, messages: 4, textBytes: 32})
	paths, err := filepath.Glob(filepath.Join(corpus.agentDir, "sessions", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for index, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.SplitN(data, []byte{'\n'}, 2)
		var header map[string]any
		if err := json.Unmarshal(lines[0], &header); err != nil {
			t.Fatal(err)
		}
		header["cwd"] = filepath.Join(corpus.cwd, fmt.Sprintf("project-%02d", index/12))
		encoded, err := json.Marshal(header)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(append(encoded, '\n'), lines[1]...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	api, managers := openDataBenchmarkService(t, corpus)
	overview, err := api.ReadOverview()
	if err != nil || len(overview.Projects) != 8 || len(overview.Directory.Projects) != 8 || overview.Directory.Total != 15 || overview.Directory.Next == nil {
		t.Fatalf("bounded first screen = %#v, %v", overview, err)
	}
	for _, item := range overview.Directory.Projects {
		if len(item.Sessions.Sessions) != 8 || item.Sessions.Total != 12 || item.Sessions.Next == nil {
			t.Fatalf("project did not include a bounded first session page: %#v", item)
		}
		if item.Project.Path != item.Sessions.Sessions[0].CWD {
			t.Fatal("project/session grouping differs")
		}
	}
	first := overview.Directory.Projects[0]
	more, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Project: first.Project.Path, Cursor: first.Sessions.Next, Limit: 20})
	if err != nil || more.Start != 8 || len(more.Sessions) != 4 || more.Next != nil {
		t.Fatalf("seed cursor cannot continue through ordinary directory reads: %#v, %v", more, err)
	}
	// A new activity moves an unvisited project to the head. Existing project
	// cursors must still finish the earlier ordering without duplicates.
	writer, err := session.OpenSessionManager(paths[0], filepath.Dir(paths[0]), "")
	if err != nil {
		t.Fatal(err)
	}
	message, _ := llm.NewUserTextMessage("new project activity", time.UnixMilli(3_000_000_000_000))
	if _, err := writer.AppendLLMMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	remaining, err := api.ReadProjectDirectory(application.ProjectDirectoryQuery{Cursor: overview.Directory.Next, Limit: 8, SessionLimit: 3})
	if err != nil || len(remaining.Projects) != 7 || remaining.Version != overview.Directory.Version || remaining.Start != 8 || remaining.Next != nil {
		t.Fatalf("project continuation changed snapshot: %#v, %v", remaining, err)
	}
	seen := make(map[string]bool)
	for _, item := range append(overview.Directory.Projects, remaining.Projects...) {
		if seen[item.Project.Path] {
			t.Fatal("repeated project", item.Project.Path)
		}
		seen[item.Project.Path] = true
	}
	latest, err := api.ReadOverview()
	if err != nil || latest.Directory.Version == overview.Directory.Version || filepath.Base(latest.Projects[0].Path) != "project-00" {
		t.Fatalf("fresh head did not include changed project: %#v, %v", latest, err)
	}
	if _, err := api.ReadProjectDirectory(application.ProjectDirectoryQuery{Cursor: &application.ProjectDirectoryCursor{Index: first.Sessions.Version, Offset: 0}}); !errors.Is(err, session.ErrInvalidReadQuery) {
		t.Fatalf("session cursor was accepted as project cursor: %v", err)
	}
	if _, err := api.ReadSessionDirectory(application.SessionDirectoryQuery{Cursor: &application.SessionDirectoryCursor{Index: overview.Directory.Version, Offset: 0}}); !errors.Is(err, session.ErrInvalidReadQuery) {
		t.Fatalf("project cursor was accepted as session cursor: %v", err)
	}
	if len(managers) != 0 {
		t.Fatal("sidebar browse admitted an Agent Runtime")
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
