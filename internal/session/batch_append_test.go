package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func controlPayloads() []EntryPayload {
	return []EntryPayload{
		ModelChangePayload{Provider: "provider", ModelID: "model"},
		ThinkingLevelChangePayload{ThinkingLevel: "high"},
	}
}

func TestAppendPayloadsPersistsOneLinearBatchAndReopens(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "batch.jsonl")
	value, err := Create(path, CreateOptions{
		ID: "batch-session", WorkingDir: directory,
		Now:        sequenceClock(time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)),
		NewEntryID: sequenceIDs("model-entry", "thinking-entry"),
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := value.AppendPayloads(context.Background(), controlPayloads())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID() != "model-entry" || entries[1].ID() != "thinking-entry" {
		t.Fatalf("batch entries = %#v", entries)
	}
	if parent, ok := entries[1].ParentID(); !ok || parent != entries[0].ID() {
		t.Fatalf("second parent = %q, %t", parent, ok)
	}
	if err := value.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.Entries()
	if len(got) != 2 || got[0].Type() != "model_change" || got[1].Type() != "thinking_level_change" {
		t.Fatalf("reopened batch = %#v", got)
	}
	if parent, ok := got[1].ParentID(); !ok || parent != got[0].ID() {
		t.Fatalf("reopened parent = %q, %t", parent, ok)
	}
}

func TestAppendPayloadsDefiniteAndUnknownStorageFailures(t *testing.T) {
	failure := errors.New("append failed")
	t.Run("definite", func(t *testing.T) {
		storage := &fakeStorage{createCreated: true, appendErr: failure, appendStarted: false}
		value := newFakeSession(t, storage, sequenceIDs("model-entry", "thinking-entry"))
		_, err := value.AppendPayloads(context.Background(), controlPayloads())
		if !errors.Is(err, ErrStorage) || errors.Is(err, ErrCommitUnknown) {
			t.Fatalf("batch error = %v", err)
		}
		if len(storage.appendCalls) != 1 || len(value.Entries()) != 0 || value.Poisoned() {
			t.Fatalf("definite failure side effects: calls=%d entries=%d poisoned=%t", len(storage.appendCalls), len(value.Entries()), value.Poisoned())
		}
	})
	t.Run("commit unknown", func(t *testing.T) {
		storage := &fakeStorage{createCreated: true, appendErr: failure, appendStarted: true}
		value := newFakeSession(t, storage, sequenceIDs("model-entry", "thinking-entry"))
		_, err := value.AppendPayloads(context.Background(), controlPayloads())
		if !errors.Is(err, ErrCommitUnknown) || !errors.Is(err, failure) {
			t.Fatalf("batch error = %v", err)
		}
		if len(value.Entries()) != 0 || !value.Poisoned() {
			t.Fatalf("unknown failure state: entries=%d poisoned=%t", len(value.Entries()), value.Poisoned())
		}
	})
}

func TestAppendPayloadsRetriesDuplicateIDsAndExhausts(t *testing.T) {
	t.Run("retry", func(t *testing.T) {
		storage := &fakeStorage{createCreated: true, appendStarted: true}
		value := newFakeSession(t, storage, sequenceIDs("duplicate", "duplicate", "second"))
		entries, err := value.AppendPayloads(context.Background(), controlPayloads())
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].ID() != "duplicate" || entries[1].ID() != "second" || len(storage.appendCalls) != 1 {
			t.Fatalf("retried batch = %#v calls=%d", entries, len(storage.appendCalls))
		}
	})
	t.Run("exhaust", func(t *testing.T) {
		storage := &fakeStorage{createCreated: true, appendStarted: true}
		value := newFakeSession(t, storage, func() (string, error) { return "duplicate", nil })
		_, err := value.AppendPayloads(context.Background(), controlPayloads())
		if !errors.Is(err, ErrEntryIDExhausted) {
			t.Fatalf("batch error = %v", err)
		}
		if len(storage.appendCalls) != 0 || len(value.Entries()) != 0 {
			t.Fatalf("exhausted batch side effects: calls=%d entries=%d", len(storage.appendCalls), len(value.Entries()))
		}
	})
}

func TestAppendPayloadsCancellationDoesNotReachStorage(t *testing.T) {
	t.Run("before admission", func(t *testing.T) {
		storage := &fakeStorage{createCreated: true, appendStarted: true}
		value := newFakeSession(t, storage, sequenceIDs("model-entry", "thinking-entry"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := value.AppendPayloads(ctx, controlPayloads())
		if !errors.Is(err, ErrAppendCanceled) || len(storage.appendCalls) != 0 || len(value.Entries()) != 0 {
			t.Fatalf("canceled batch = %v calls=%d entries=%d", err, len(storage.appendCalls), len(value.Entries()))
		}
	})
	t.Run("during storage", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		storage := &fakeStorage{createCreated: true}
		storage.appendFunc = func(context.Context, []byte) (bool, error) {
			cancel()
			return false, context.Canceled
		}
		value := newFakeSession(t, storage, sequenceIDs("model-entry", "thinking-entry"))
		_, err := value.AppendPayloads(ctx, controlPayloads())
		if !errors.Is(err, ErrAppendCanceled) || len(storage.appendCalls) != 1 || len(value.Entries()) != 0 || value.Poisoned() {
			t.Fatalf("mid-storage cancellation = %v calls=%d entries=%d poisoned=%t", err, len(storage.appendCalls), len(value.Entries()), value.Poisoned())
		}
	})
}
