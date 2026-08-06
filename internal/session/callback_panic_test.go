package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const callbackDeadline = 2 * time.Second

type callbackAppendResult struct {
	entry Entry
	err   error
}

func awaitManagerAppend(t *testing.T, operation func(context.Context) (Entry, error)) (Entry, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callbackDeadline/2)
	defer cancel()
	done := make(chan callbackAppendResult, 1)
	go func() {
		entry, err := operation(ctx)
		done <- callbackAppendResult{entry: entry, err: err}
	}()
	select {
	case result := <-done:
		return result.entry, result.err
	case <-time.After(callbackDeadline):
		t.Fatal("session manager append did not return before deadline")
		return Entry{}, context.DeadlineExceeded
	}
}

func awaitManagerClose(t *testing.T, manager *SessionManager) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- manager.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SessionManager.Close: %v", err)
		}
	case <-time.After(callbackDeadline):
		t.Fatal("session manager close did not return before deadline")
	}
}

func awaitManagerBranch(t *testing.T, manager *SessionManager) []Entry {
	t.Helper()
	type branchResult struct {
		entries []Entry
		err     error
	}
	done := make(chan branchResult, 1)
	go func() {
		entries, err := manager.BranchPath("")
		done <- branchResult{entries: entries, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("SessionManager.BranchPath: %v", result.err)
		}
		return result.entries
	case <-time.After(callbackDeadline):
		t.Fatal("session manager branch read did not return before deadline")
		return nil
	}
}

func TestSessionManagerIDGeneratorPanicDoesNotLeakLocksOrPayload(t *testing.T) {
	cwd := t.TempDir()
	calls := 0
	const sensitivePanicPayload = "credential=must-not-escape"
	manager, err := InMemorySessionManagerWithOptions(cwd, ManagerOptions{
		NewSession: NewSessionOptions{ID: "callback-id-panic"},
		Now:        func() time.Time { return time.Date(2026, time.August, 6, 1, 0, 0, 0, time.UTC) },
		NewEntryID: func() (string, error) {
			calls++
			if calls == 1 {
				panic(sensitivePanicPayload)
			}
			return "entry-after-id-panic", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, firstErr := awaitManagerAppend(t, func(ctx context.Context) (Entry, error) {
		return manager.AppendThinkingLevelChange(ctx, "low")
	})
	if !errors.Is(firstErr, ErrIDGeneration) {
		t.Fatalf("first append error = %v, want ErrIDGeneration", firstErr)
	}
	if strings.Contains(firstErr.Error(), sensitivePanicPayload) {
		t.Fatalf("ID panic payload leaked in error: %q", firstErr)
	}
	entry, err := awaitManagerAppend(t, func(ctx context.Context) (Entry, error) {
		return manager.AppendThinkingLevelChange(ctx, "high")
	})
	if err != nil || entry.ID() != "entry-after-id-panic" {
		t.Fatalf("second append = (%q, %v)", entry.ID(), err)
	}
	entries := manager.Entries()
	if len(entries) != 1 || entries[0].ID() != entry.ID() {
		t.Fatalf("manager entries = %#v", entries)
	}
	branch := awaitManagerBranch(t, manager)
	if len(branch) != 1 || branch[0].ID() != entry.ID() {
		t.Fatalf("manager branch = %#v", branch)
	}
	awaitManagerClose(t, manager)
}

func TestSessionManagerClockPanicDoesNotLeakLocksOrPayload(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(t.TempDir(), "clock-panic.jsonl")
	stored, err := Create(path, CreateOptions{ID: "callback-clock-panic", WorkingDir: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.Close(); err != nil {
		t.Fatal(err)
	}
	clockCalls := 0
	idCalls := 0
	const sensitivePanicPayload = "token=must-not-escape"
	manager, err := OpenSessionManagerWithOptions(path, "", "", ManagerOptions{
		Now: func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				panic(sensitivePanicPayload)
			}
			return time.Date(2026, time.August, 6, 2, 0, clockCalls, 0, time.UTC)
		},
		NewEntryID: func() (string, error) {
			idCalls++
			if idCalls == 1 {
				return "discarded-clock-panic-id", nil
			}
			return "entry-after-clock-panic", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := mustUserMessage(t, "after clock callback", time.Date(2026, time.August, 6, 2, 0, 0, 0, time.UTC))
	_, firstErr := awaitManagerAppend(t, func(ctx context.Context) (Entry, error) {
		return manager.AppendLLMMessage(ctx, message)
	})
	if !errors.Is(firstErr, ErrInvalidEntry) {
		t.Fatalf("first append error = %v, want ErrInvalidEntry", firstErr)
	}
	if strings.Contains(firstErr.Error(), sensitivePanicPayload) {
		t.Fatalf("clock panic payload leaked in error: %q", firstErr)
	}
	entry, err := awaitManagerAppend(t, func(ctx context.Context) (Entry, error) {
		return manager.AppendLLMMessage(ctx, message)
	})
	if err != nil || entry.ID() != "entry-after-clock-panic" {
		t.Fatalf("second append = (%q, %v)", entry.ID(), err)
	}
	entries := manager.Entries()
	if len(entries) != 1 || entries[0].ID() != entry.ID() {
		t.Fatalf("manager entries = %#v", entries)
	}
	branch := awaitManagerBranch(t, manager)
	if len(branch) != 1 || branch[0].ID() != entry.ID() {
		t.Fatalf("manager branch = %#v", branch)
	}
	awaitManagerClose(t, manager)
}

func TestNormalizeRuntimePreservesNilZeroAndCallbackErrors(t *testing.T) {
	cwd := t.TempDir()
	var nilClock Clock
	var nilGenerator IDGenerator
	defaults, err := InMemorySessionManagerWithOptions(cwd, ManagerOptions{
		NewSession: NewSessionOptions{ID: "nil-runtime-callbacks"}, Now: nilClock, NewEntryID: nilGenerator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaults.AppendThinkingLevelChange(context.Background(), "medium"); err != nil {
		t.Fatalf("nil callbacks did not use defaults: %v", err)
	}
	awaitManagerClose(t, defaults)

	sourceErr := errors.New("ordinary generator failure")
	errorManager, err := InMemorySessionManagerWithOptions(cwd, ManagerOptions{
		NewSession: NewSessionOptions{ID: "ordinary-callback-error"},
		NewEntryID: func() (string, error) { return "", sourceErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := errorManager.AppendThinkingLevelChange(context.Background(), "medium"); !errors.Is(err, ErrIDGeneration) || !errors.Is(err, sourceErr) {
		t.Fatalf("ordinary callback error = %v", err)
	}
	awaitManagerClose(t, errorManager)

	clockCalls := 0
	zeroManager, err := InMemorySessionManagerWithOptions(cwd, ManagerOptions{
		NewSession: NewSessionOptions{ID: "zero-clock"},
		Now: func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return time.Date(2026, time.August, 6, 3, 0, 0, 0, time.UTC)
			}
			return time.Time{}
		},
		NewEntryID: func() (string, error) { return "zero-clock-entry", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zeroManager.AppendThinkingLevelChange(context.Background(), "medium"); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("zero clock error = %v, want ErrInvalidEntry", err)
	}
	awaitManagerClose(t, zeroManager)
}
