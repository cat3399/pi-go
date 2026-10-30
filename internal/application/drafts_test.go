package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDraftTextPersistsAndRejectsStaleWriters(t *testing.T) {
	ctx := context.Background()
	cwd, agentDir := t.TempDir(), t.TempDir()
	first := projectTestService(t, cwd, agentDir)
	second := projectTestService(t, cwd, agentDir)
	key := "session:one"
	initial, err := first.ReadDraftText(ctx, key)
	if err != nil || initial.Revision != "" || initial.Text != "" {
		t.Fatalf("initial = %#v, %v", initial, err)
	}
	var wg sync.WaitGroup
	results := make(chan DraftTextWriteResult, 2)
	for i, service := range []*Service{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			revision := []string{"one", "two"}[i]
			result, err := service.WriteDraftText(ctx, key, DraftTextUpdate{Revision: revision, Text: revision})
			if err != nil {
				t.Error(err)
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	saved := 0
	var winner DraftText
	for result := range results {
		if result.Saved {
			saved++
			winner = result.Draft
		}
	}
	if saved != 1 {
		t.Fatalf("saved %d competing writes", saved)
	}
	if winner.UpdatedAt.IsZero() {
		t.Fatal("missing timestamp")
	}
	path, _ := first.draftPath(key)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("draft permissions = %v, %v", info, err)
	}
	retry, err := second.WriteDraftText(ctx, key, DraftTextUpdate{Revision: winner.Revision, Text: winner.Text})
	if err != nil || !retry.Saved || retry.Draft != winner {
		t.Fatalf("idempotent retry = %#v, %v", retry, err)
	}
	_, err = first.WriteDraftText(ctx, key, DraftTextUpdate{Revision: winner.Revision, Text: "different"})
	if !errors.Is(err, ErrInvalidDraft) {
		t.Fatalf("reused revision: %v", err)
	}
	cleared, err := first.WriteDraftText(ctx, key, DraftTextUpdate{BaseRevision: winner.Revision, Revision: "cleared"})
	if err != nil || !cleared.Saved {
		t.Fatalf("clear = %#v, %v", cleared, err)
	}
	reopened := projectTestService(t, cwd, agentDir)
	value, err := reopened.ReadDraftText(ctx, key)
	if err != nil || value.Text != "" || value.Revision != "cleared" {
		t.Fatalf("reopen = %#v, %v", value, err)
	}
	stale, err := second.WriteDraftText(ctx, key, DraftTextUpdate{BaseRevision: winner.Revision, Revision: "stale", Text: "old"})
	if err != nil || stale.Saved || stale.Draft.Revision != "cleared" {
		t.Fatalf("stale = %#v, %v", stale, err)
	}
}

func TestDraftTextKeysAreIsolatedAndCorruptionIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	service := projectTestService(t, t.TempDir(), t.TempDir())
	for _, key := range []string{"session:a", "session:b", "new:/workspace", "../../escape"} {
		result, err := service.WriteDraftText(ctx, key, DraftTextUpdate{Revision: "revision", Text: key})
		if err != nil || !result.Saved {
			t.Fatalf("write %q: %#v, %v", key, result, err)
		}
		path, _ := service.draftPath(key)
		if filepath.Dir(path) != filepath.Join(service.paths.AgentDir, "drafts") {
			t.Fatalf("escaped path %q", path)
		}
		got, err := service.ReadDraftText(ctx, key)
		if err != nil || got.Text != key {
			t.Fatalf("read %q: %#v, %v", key, got, err)
		}
	}
	path, _ := service.draftPath("session:a")
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.WriteDraftText(ctx, "session:a", DraftTextUpdate{Revision: "next"}); err == nil {
		t.Fatal("overwrote corrupt draft")
	}
	if _, err := service.ReadDraftText(ctx, ""); !errors.Is(err, ErrInvalidDraft) {
		t.Fatalf("empty key: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := service.WriteDraftText(cancelled, "session:c", DraftTextUpdate{Revision: "cancelled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
}
