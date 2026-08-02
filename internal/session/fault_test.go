package session

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

type fakeStorage struct {
	readData      []byte
	readErr       error
	createCreated bool
	createErr     error
	appendStarted bool
	appendErr     error
	createCalls   [][]byte
	appendCalls   [][]byte
	appendFunc    func(context.Context, []byte) (bool, error)
}

func (storage *fakeStorage) read(string) ([]byte, error) {
	return append([]byte(nil), storage.readData...), storage.readErr
}

func (storage *fakeStorage) create(_ string, data []byte) (bool, error) {
	storage.createCalls = append(storage.createCalls, append([]byte(nil), data...))
	return storage.createCreated, storage.createErr
}

func (storage *fakeStorage) append(ctx context.Context, _ string, data []byte) (bool, error) {
	storage.appendCalls = append(storage.appendCalls, append([]byte(nil), data...))
	if storage.appendFunc != nil {
		return storage.appendFunc(ctx, data)
	}
	return storage.appendStarted, storage.appendErr
}

func TestCreateClassifiesKnownAndUnknownDurabilityFailures(t *testing.T) {
	t.Parallel()

	storageFailure := errors.New("create failed")
	for _, tt := range []struct {
		name    string
		created bool
		want    error
	}{
		{name: "before publication", created: false, want: ErrStorage},
		{name: "after publication", created: true, want: ErrDurabilityUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			storage := &fakeStorage{createCreated: tt.created, createErr: storageFailure}
			_, err := createWithStorage(storage, filepath.Join(t.TempDir(), "session.jsonl"), CreateOptions{
				ID: "session-1", WorkingDir: t.TempDir(),
				Now: func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) },
			})
			if !errors.Is(err, tt.want) || !errors.Is(err, storageFailure) {
				t.Fatalf("create error = %v, want %v and cause", err, tt.want)
			}
			if len(storage.createCalls) != 1 || !bytes.HasSuffix(storage.createCalls[0], []byte("\n")) {
				t.Fatalf("create calls = %#v", storage.createCalls)
			}
		})
	}
}

func TestExtractClassifiesPublicationFailureAndNeverAppendsTarget(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		created bool
		want    error
	}{
		{name: "before publication", want: ErrStorage},
		{name: "after publication", created: true, want: ErrDurabilityUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			storage := &fakeStorage{createCreated: tt.created, createErr: errors.New("publish failed")}
			_, err := extractWithStorage(context.Background(), storage, filepath.Join(t.TempDir(), "source.jsonl"), nil, ExtractOptions{
				TargetPath: filepath.Join(t.TempDir(), "target.jsonl"), ID: "target", WorkingDir: t.TempDir(),
				Now: func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) },
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("extract error = %v, want %v", err, tt.want)
			}
			if len(storage.createCalls) != 1 || len(storage.appendCalls) != 0 {
				t.Fatalf("extract writes: create=%d append=%d", len(storage.createCalls), len(storage.appendCalls))
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := &fakeStorage{createCreated: true}
	_, err := extractWithStorage(ctx, storage, filepath.Join(t.TempDir(), "source.jsonl"), nil, ExtractOptions{TargetPath: filepath.Join(t.TempDir(), "target.jsonl"), ID: "target", WorkingDir: t.TempDir()})
	if !errors.Is(err, ErrAppendCanceled) || len(storage.createCalls) != 0 {
		t.Fatalf("canceled extraction = %v, creates=%d", err, len(storage.createCalls))
	}
}

func TestAppendPreWriteFailureLeavesWriterUsable(t *testing.T) {
	t.Parallel()

	storage := &fakeStorage{createCreated: true}
	session := newFakeSession(t, storage, sequenceIDs("entry-1", "entry-2"))
	storage.appendErr = errors.New("open failed")
	storage.appendStarted = false
	message := mustUserMessage(t, "first", time.UnixMilli(1))
	if _, err := session.Append(context.Background(), message, AppendOptions{}); !errors.Is(err, ErrStorage) || errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("Append() error = %v, want retryable ErrStorage", err)
	}
	if session.Poisoned() || len(session.Entries()) != 0 {
		t.Fatalf("pre-write failure changed session: poisoned=%t entries=%d", session.Poisoned(), len(session.Entries()))
	}

	storage.appendErr = nil
	storage.appendStarted = true
	entry, err := session.Append(context.Background(), message, AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID() != "entry-2" || len(session.Entries()) != 1 {
		t.Fatalf("retry entry = %q, entries=%d", entry.ID(), len(session.Entries()))
	}
}

func TestAppendWriteSyncOrCloseFailurePoisonsWithoutAdvancingLeaf(t *testing.T) {
	t.Parallel()

	for _, stage := range []string{"write", "sync", "close"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			stageErr := errors.New(stage + " failed")
			storage := &fakeStorage{createCreated: true, appendStarted: true, appendErr: stageErr}
			session := newFakeSession(t, storage, sequenceIDs("entry-1", "entry-2"))
			_, err := session.Append(context.Background(), mustUserMessage(t, "first", time.UnixMilli(1)), AppendOptions{})
			if !errors.Is(err, ErrCommitUnknown) || !errors.Is(err, stageErr) {
				t.Fatalf("Append() error = %v, want ErrCommitUnknown and cause", err)
			}
			if !session.Poisoned() || len(session.Entries()) != 0 {
				t.Fatalf("uncertain failure state: poisoned=%t entries=%d", session.Poisoned(), len(session.Entries()))
			}
			calls := len(storage.appendCalls)
			storage.appendErr = nil
			if _, err := session.Append(context.Background(), mustUserMessage(t, "retry", time.UnixMilli(2)), AppendOptions{}); !errors.Is(err, ErrPoisoned) {
				t.Fatalf("retry error = %v, want ErrPoisoned", err)
			}
			if len(storage.appendCalls) != calls {
				t.Fatal("poisoned writer attempted another storage append")
			}
		})
	}
}

func TestAppendValidationAndEncodingFailuresDoNotWrite(t *testing.T) {
	t.Parallel()

	storage := &fakeStorage{createCreated: true, appendStarted: true}
	session := newFakeSession(t, storage, sequenceIDs("entry-1", "entry-2"))
	assistant, err := llmAssistantForFaultTest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), assistant, AppendOptions{}); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("assistant without identity error = %v, want ErrInvalidEntry", err)
	}
	if len(storage.appendCalls) != 0 || session.Poisoned() {
		t.Fatal("encoding failure reached storage or poisoned writer")
	}

	session.runtime.newEntryID = func() (string, error) { return "", errors.New("entropy failed") }
	if _, err := session.Append(context.Background(), mustUserMessage(t, "user", time.UnixMilli(1)), AppendOptions{}); !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("id failure error = %v, want ErrIDGeneration", err)
	}
	if len(storage.appendCalls) != 0 || session.Poisoned() {
		t.Fatal("id failure reached storage or poisoned writer")
	}
}

func TestDuplicateGeneratedIDsExhaustWithoutWrite(t *testing.T) {
	t.Parallel()

	storage := &fakeStorage{createCreated: true, appendStarted: true}
	session := newFakeSession(t, storage, func() (string, error) { return "duplicate", nil })
	first, err := session.Append(context.Background(), mustUserMessage(t, "first", time.UnixMilli(1)), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != "duplicate" {
		t.Fatalf("first ID = %q", first.ID())
	}
	calls := len(storage.appendCalls)
	if _, err := session.Append(context.Background(), mustUserMessage(t, "second", time.UnixMilli(2)), AppendOptions{}); !errors.Is(err, ErrEntryIDExhausted) {
		t.Fatalf("duplicate ID error = %v, want ErrEntryIDExhausted", err)
	}
	if len(storage.appendCalls) != calls || len(session.Entries()) != 1 || session.Poisoned() {
		t.Fatal("duplicate ID exhaustion changed storage or session")
	}
}

func TestAppendCancellationBeforeWriteDoesNotCommit(t *testing.T) {
	t.Parallel()

	storage := &fakeStorage{createCreated: true, appendStarted: true}
	session := newFakeSession(t, storage, sequenceIDs("entry-1"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := session.Append(ctx, mustUserMessage(t, "canceled", time.UnixMilli(1)), AppendOptions{})
	if !errors.Is(err, ErrAppendCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Append() error = %v, want ErrAppendCanceled and context.Canceled", err)
	}
	if len(storage.appendCalls) != 0 || len(session.Entries()) != 0 || session.Poisoned() {
		t.Fatalf("pre-canceled append changed state: calls=%d entries=%d poisoned=%t", len(storage.appendCalls), len(session.Entries()), session.Poisoned())
	}
}

func TestAppendCancellationWhileWaitingForWriterDoesNotWriteLater(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	storage := &fakeStorage{createCreated: true}
	storage.appendFunc = func(context.Context, []byte) (bool, error) {
		close(entered)
		<-release
		return true, nil
	}
	session := newFakeSession(t, storage, sequenceIDs("entry-1", "entry-2"))
	firstMessage := mustUserMessage(t, "first", time.UnixMilli(1))
	secondMessage := mustUserMessage(t, "second", time.UnixMilli(2))
	firstDone := make(chan error, 1)
	go func() {
		_, err := session.Append(context.Background(), firstMessage, AppendOptions{})
		firstDone <- err
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := session.Append(ctx, secondMessage, AppendOptions{})
		secondDone <- err
	}()
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrAppendCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting Append() error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting Append did not observe cancellation")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	if len(storage.appendCalls) != 1 || len(session.Entries()) != 1 {
		t.Fatalf("canceled waiter wrote later: calls=%d entries=%d", len(storage.appendCalls), len(session.Entries()))
	}
}

func TestAppendCancellationAtStorageBoundaryIsRetryable(t *testing.T) {
	t.Parallel()

	storage := &fakeStorage{createCreated: true}
	session := newFakeSession(t, storage, sequenceIDs("entry-1"))
	ctx, cancel := context.WithCancel(context.Background())
	storage.appendFunc = func(context.Context, []byte) (bool, error) {
		cancel()
		return false, context.Canceled
	}
	_, err := session.Append(ctx, mustUserMessage(t, "canceled", time.UnixMilli(1)), AppendOptions{})
	if !errors.Is(err, ErrAppendCanceled) || errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("Append() error = %v, want retryable ErrAppendCanceled", err)
	}
	if len(session.Entries()) != 0 || session.Poisoned() {
		t.Fatalf("pre-write cancellation changed state: entries=%d poisoned=%t", len(session.Entries()), session.Poisoned())
	}
}

func TestAppendSettlesAfterWriteStartsDespiteCancellation(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		writeErr error
	}{
		{name: "durable success"},
		{name: "uncertain failure", writeErr: errors.New("sync failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			storage := &fakeStorage{createCreated: true}
			session := newFakeSession(t, storage, sequenceIDs("entry-1"))
			ctx, cancel := context.WithCancel(context.Background())
			storage.appendFunc = func(context.Context, []byte) (bool, error) {
				cancel()
				return true, tt.writeErr
			}
			entry, err := session.Append(ctx, mustUserMessage(t, "settle", time.UnixMilli(1)), AppendOptions{})
			if tt.writeErr == nil {
				if err != nil || entry.ID() != "entry-1" || len(session.Entries()) != 1 {
					t.Fatalf("settled success = (%q, %v), entries=%d", entry.ID(), err, len(session.Entries()))
				}
				return
			}
			if !errors.Is(err, ErrCommitUnknown) || !errors.Is(err, tt.writeErr) || errors.Is(err, ErrAppendCanceled) {
				t.Fatalf("settled failure error = %v, want ErrCommitUnknown and write cause", err)
			}
			if !session.Poisoned() || len(session.Entries()) != 0 {
				t.Fatalf("uncertain failure state: poisoned=%t entries=%d", session.Poisoned(), len(session.Entries()))
			}
		})
	}
}

func newFakeSession(t *testing.T, storage *fakeStorage, ids IDGenerator) *Session {
	t.Helper()
	session, err := createWithStorage(storage, filepath.Join(t.TempDir(), "session.jsonl"), CreateOptions{
		ID:         "session-1",
		WorkingDir: t.TempDir(),
		Now:        func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) },
		NewEntryID: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func llmAssistantForFaultTest() (llm.ConversationMessage, error) {
	block, err := llm.NewTextBlock("assistant")
	if err != nil {
		return nil, err
	}
	usage, err := llm.NewUsage(llm.UsageSpec{})
	if err != nil {
		return nil, err
	}
	return llm.NewAssistantTextMessage([]llm.TextBlock{block}, llm.FinishStop, usage, time.UnixMilli(1))
}
