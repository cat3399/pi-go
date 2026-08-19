package tui

import (
	"context"
	"testing"

	"github.com/cat3399/pi-go/internal/application"
)

type fileCompletionTestAPI struct {
	modelTestAPI
	result application.FileIndexResult
	err    error
	cwd    string
	query  string
}

func (a *fileCompletionTestAPI) QueryFileIndex(_ context.Context, cwd, query string) (application.FileIndexResult, error) {
	a.cwd, a.query = cwd, query
	return a.result, a.err
}

func TestAtFileCompletionQueriesProjectIndexAndAppliesSelection(t *testing.T) {
	api := &fileCompletionTestAPI{result: application.FileIndexResult{
		HasQuery: true,
		Matches: []application.FileIndexEntry{
			{Path: "src/main.go"},
			{Path: "src/middleware", IsDir: true},
		},
	}}
	model := newModelWithAPIForTest(t, api)
	model.composer.SetDraft("inspect @src/ma", nil)
	command := model.refreshFileCompletion()
	if command == nil {
		t.Fatal("@ reference did not start file completion")
	}
	message, ok := command().(fileCompletionsLoadedMsg)
	if !ok || message.err != nil || api.cwd != "/workspace" || api.query != "src/ma" {
		t.Fatalf("completion query = %#v / %q / %q", message, api.cwd, api.query)
	}
	model.handleFileCompletionLoaded(message)
	if !model.fileCompletion.Active() || len(model.fileCompletion.items) != 2 {
		t.Fatalf("completion state = %#v", model.fileCompletion)
	}
	if command := model.acceptFileCompletion(); command != nil {
		t.Fatal("file selection unexpectedly requested another completion")
	}
	if got := model.composer.Value(); got != "inspect @src/main.go " {
		t.Fatalf("completed draft = %q", got)
	}
	if model.fileCompletion.Active() {
		t.Fatal("file completion remained active after selecting a file")
	}
}

func TestDirectoryCompletionContinuesAndQuotedPathKeepsCursorInsideQuotes(t *testing.T) {
	model := newModelForTest(t)
	model.composer.SetDraft(`read @"docs/re`, nil)
	target, ok := currentFileCompletionTarget(model.composer.Value(), model.composer.CursorOffset())
	if !ok || !target.quoted || target.query != "docs/re" {
		t.Fatalf("quoted target = %#v, %t", target, ok)
	}
	replacement, cursor := fileCompletionReplacement(application.FileIndexEntry{
		Path: "docs/release notes", IsDir: true,
	}, target.quoted)
	model.composer.ReplaceRuneRange(target.start, target.end, replacement, cursor)
	if got := model.composer.Value(); got != `read @"docs/release notes/"` {
		t.Fatalf("quoted directory draft = %q", got)
	}
	next, ok := currentFileCompletionTarget(model.composer.Value(), model.composer.CursorOffset())
	if !ok || next.query != "docs/release notes/" || !next.quoted {
		t.Fatalf("continued quoted target = %#v, %t", next, ok)
	}
}

func TestFileCompletionIgnoresStaleAsyncResult(t *testing.T) {
	model := newModelForTest(t)
	model.composer.SetDraft("@old", nil)
	target, _ := currentFileCompletionTarget(model.composer.Value(), model.composer.CursorOffset())
	model.fileCompletionGeneration = 2
	model.fileCompletion.SetLoading(target)
	model.composer.SetDraft("@new", nil)
	model.handleFileCompletionLoaded(fileCompletionsLoadedMsg{
		generation: 1,
		target:     target,
		result: application.FileIndexResult{
			HasQuery: true, Matches: []application.FileIndexEntry{{Path: "old.txt"}},
		},
	})
	if len(model.fileCompletion.items) != 0 {
		t.Fatalf("stale completion was applied: %#v", model.fileCompletion.items)
	}
}
