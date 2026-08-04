package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

type fakeStorage struct {
	readData           []byte
	readErr            error
	createCreated      bool
	createErr          error
	appendStarted      bool
	appendErr          error
	createCalls        [][]byte
	appendCalls        [][]byte
	appendFunc         func(context.Context, []byte) (bool, error)
	replaceCalled      int
	replaceDone        bool
	replaceErr         error
	replaceData        []byte
	validateReplaceErr error
}

type poisonedSnapshotStorage struct {
	base         osSessionStorage
	createCalls  int
	appendCalls  int
	failAppendAt int
	appendErr    error
}

type fakeRewriteTemporary struct {
	name        string
	data        []byte
	chmodErr    error
	writeErr    error
	syncErr     error
	closeErrors []error
	closeCalls  int
}

func (temporary *fakeRewriteTemporary) Name() string { return temporary.name }
func (temporary *fakeRewriteTemporary) Chmod(os.FileMode) error {
	return temporary.chmodErr
}
func (temporary *fakeRewriteTemporary) Write(data []byte) (int, error) {
	if temporary.writeErr != nil {
		written := len(data) / 2
		temporary.data = append(temporary.data, data[:written]...)
		return written, temporary.writeErr
	}
	temporary.data = append(temporary.data, data...)
	return len(data), nil
}
func (temporary *fakeRewriteTemporary) Sync() error { return temporary.syncErr }
func (temporary *fakeRewriteTemporary) Close() error {
	index := temporary.closeCalls
	temporary.closeCalls++
	if index < len(temporary.closeErrors) {
		return temporary.closeErrors[index]
	}
	return nil
}

type fakeSessionRewriteOps struct {
	temporary       *fakeRewriteTemporary
	createErr       error
	replaced        bool
	replaceErr      error
	removeErr       error
	replaceCalls    int
	removeCalls     int
	replaceTempPath string
	replaceTarget   string
}

func (ops *fakeSessionRewriteOps) createTemp(string, string) (rewriteTemporary, error) {
	if ops.createErr != nil {
		return nil, ops.createErr
	}
	return ops.temporary, nil
}
func (ops *fakeSessionRewriteOps) replaceTemporary(temporaryPath, targetPath string) (bool, error) {
	ops.replaceCalls++
	ops.replaceTempPath = temporaryPath
	ops.replaceTarget = targetPath
	return ops.replaced, ops.replaceErr
}
func (ops *fakeSessionRewriteOps) remove(string) error {
	ops.removeCalls++
	return ops.removeErr
}

func (storage *poisonedSnapshotStorage) read(path string) ([]byte, error) {
	return storage.base.read(path)
}

func (storage *poisonedSnapshotStorage) create(path string, data []byte) (bool, error) {
	storage.createCalls++
	return storage.base.create(path, data)
}

func (storage *poisonedSnapshotStorage) append(ctx context.Context, path string, data []byte) (bool, error) {
	storage.appendCalls++
	if storage.appendCalls == storage.failAppendAt {
		started, err := storage.base.append(ctx, path, data)
		if err != nil {
			return started, errors.Join(err, storage.appendErr)
		}
		// The complete record is now written and synced. Injecting an error after
		// that durable operation models the exact disk-ahead-of-memory outcome
		// that ErrCommitUnknown requires callers to reconcile.
		return true, storage.appendErr
	}
	return storage.base.append(ctx, path, data)
}

func (storage *poisonedSnapshotStorage) replace(path string, data []byte) (bool, error) {
	return storage.base.replace(path, data)
}

func (storage *poisonedSnapshotStorage) validateReplace(path string) error {
	return storage.base.validateReplace(path)
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

func (storage *fakeStorage) replace(_ string, data []byte) (bool, error) {
	storage.replaceCalled++
	storage.replaceData = append([]byte(nil), data...)
	return storage.replaceDone, storage.replaceErr
}

func (storage *fakeStorage) validateReplace(string) error { return storage.validateReplaceErr }

func TestRefreshAfterRewriteVerifiesLockedIdentity(t *testing.T) {
	for _, test := range []struct {
		name        string
		claim       func(t *testing.T, directory string) processIdentityWriterClaimer
		wantCause   error
		wantUnlocks int
	}{
		{
			name: "missing lock",
			claim: func(*testing.T, string) processIdentityWriterClaimer {
				return func(string) (func(), os.FileInfo, error) { return nil, nil, nil }
			},
			wantCause: errRewrittenIdentityLockMissing,
		},
		{
			name: "locked handle has different identity",
			claim: func(t *testing.T, directory string) processIdentityWriterClaimer {
				otherPath := filepath.Join(directory, "other.jsonl")
				if err := os.WriteFile(otherPath, []byte("other"), 0o600); err != nil {
					t.Fatal(err)
				}
				otherInfo, err := os.Stat(otherPath)
				if err != nil {
					t.Fatal(err)
				}
				return func(string) (func(), os.FileInfo, error) {
					return func() {}, otherInfo, nil
				}
			},
			wantCause:   errRewrittenIdentityChanged,
			wantUnlocks: rewrittenIdentityLockAttempts,
		},
		{
			name: "path changes after matching handle is locked",
			claim: func(t *testing.T, directory string) processIdentityWriterClaimer {
				attempt := 0
				return func(path string) (func(), os.FileInfo, error) {
					unlock, info, err := claimProcessIdentityWriterWithInfo(path)
					if err != nil {
						t.Fatal(err)
					}
					moved := filepath.Join(directory, fmt.Sprintf("moved-%d.jsonl", attempt))
					attempt++
					if err := os.Rename(path, moved); err != nil {
						unlock()
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("raced"), 0o600); err != nil {
						unlock()
						t.Fatal(err)
					}
					return unlock, info, nil
				}
			},
			wantCause:   errRewrittenIdentityChanged,
			wantUnlocks: rewrittenIdentityLockAttempts,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "session.jsonl")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			claim, err := claimSessionWriter(path)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseSessionWriter(claim)
			replaced, err := (osSessionStorage{}).replace(path, []byte("new"))
			if err != nil || !replaced {
				t.Fatalf("prepare rewritten identity = (%t, %v)", replaced, err)
			}

			claimIdentity := test.claim(t, directory)
			unlockCalls := 0
			wrappedClaim := func(path string) (func(), os.FileInfo, error) {
				unlock, info, err := claimIdentity(path)
				if unlock == nil {
					return nil, info, err
				}
				return func() {
					unlockCalls++
					unlock()
				}, info, err
			}
			err = refreshSessionWriterAfterRewriteWith(claim, path, wrappedClaim)
			if !errors.Is(err, ErrStorage) || !errors.Is(err, test.wantCause) {
				t.Fatalf("refresh rewritten identity = %v, want storage + %v", err, test.wantCause)
			}
			if unlockCalls != test.wantUnlocks {
				t.Fatalf("mismatched identity unlock calls = %d, want %d", unlockCalls, test.wantUnlocks)
			}
		})
	}
}

func TestRewriteTemporaryIsRemovedOnEveryPrePublicationFailure(t *testing.T) {
	t.Parallel()
	stageFailure := errors.New("stage failed")
	tests := []struct {
		name      string
		configure func(*fakeRewriteTemporary, *fakeSessionRewriteOps)
	}{
		{name: "chmod", configure: func(file *fakeRewriteTemporary, _ *fakeSessionRewriteOps) { file.chmodErr = stageFailure }},
		{name: "write", configure: func(file *fakeRewriteTemporary, _ *fakeSessionRewriteOps) { file.writeErr = stageFailure }},
		{name: "sync", configure: func(file *fakeRewriteTemporary, _ *fakeSessionRewriteOps) { file.syncErr = stageFailure }},
		{name: "close", configure: func(file *fakeRewriteTemporary, _ *fakeSessionRewriteOps) {
			file.closeErrors = []error{stageFailure, nil}
		}},
		{name: "rename", configure: func(_ *fakeRewriteTemporary, ops *fakeSessionRewriteOps) { ops.replaceErr = stageFailure }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			temporary := &fakeRewriteTemporary{name: filepath.Join(t.TempDir(), ".pi-go-session-rewrite-fault")}
			ops := &fakeSessionRewriteOps{temporary: temporary}
			test.configure(temporary, ops)
			replaced, err := replaceSessionFile(ops, filepath.Join(t.TempDir(), "session.jsonl"), []byte("secret session bytes"))
			if replaced || !errors.Is(err, stageFailure) {
				t.Fatalf("replace = (%t, %v), want pre-publication stage failure", replaced, err)
			}
			if ops.removeCalls != 1 {
				t.Fatalf("temporary remove calls = %d, want 1", ops.removeCalls)
			}
			if test.name == "rename" && (ops.replaceCalls != 1 || ops.replaceTempPath != temporary.name) {
				t.Fatalf("rename seam = calls %d path %q", ops.replaceCalls, ops.replaceTempPath)
			}
		})
	}
}

func TestRewriteTemporaryCleanupFailurePreservesPrimaryCause(t *testing.T) {
	t.Parallel()
	primary := errors.New("sync failed")
	cleanup := errors.New("remove failed")
	temporary := &fakeRewriteTemporary{name: ".pi-go-session-rewrite-cleanup", syncErr: primary}
	ops := &fakeSessionRewriteOps{temporary: temporary, removeErr: cleanup}
	replaced, err := replaceSessionFile(ops, "session.jsonl", []byte("private session bytes"))
	if replaced || !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("replace cleanup error = (%t, %v), want both causes", replaced, err)
	}
	if ops.removeCalls != 1 || !strings.Contains(err.Error(), "remove rewrite temporary") {
		t.Fatalf("cleanup diagnostic = calls %d, error %q", ops.removeCalls, err)
	}
}

func TestRewritePostPublicationFailureNeverRemovesByTemporaryPath(t *testing.T) {
	t.Parallel()
	postRename := errors.New("directory sync failed")
	temporary := &fakeRewriteTemporary{name: ".pi-go-session-rewrite-published"}
	ops := &fakeSessionRewriteOps{temporary: temporary, replaced: true, replaceErr: postRename}
	replaced, err := replaceSessionFile(ops, "session.jsonl", []byte("published session bytes"))
	if !replaced || !errors.Is(err, postRename) {
		t.Fatalf("post-publication replace = (%t, %v)", replaced, err)
	}
	if ops.removeCalls != 0 {
		t.Fatalf("post-publication cleanup removed via temporary path %d times", ops.removeCalls)
	}
}

func TestRewriteRenameFailureLeavesNoRealTemporaryFile(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "non-empty-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced, err := replaceSessionFile(osSessionRewriteOps{}, target, []byte("session bytes must not remain"))
	if replaced || err == nil {
		t.Fatalf("rename over non-empty directory = (%t, %v)", replaced, err)
	}
	matches, globErr := filepath.Glob(filepath.Join(directory, ".pi-go-session-rewrite-*"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("rewrite temporaries after rename failure = %v, %v", matches, globErr)
	}
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

func TestPoisonedSessionForkAndExtractFailClosedBeforeTargetCreate(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "poisoned-source.jsonl")
	diskFailure := errors.New("injected error after synced append")
	storage := &poisonedSnapshotStorage{failAppendAt: 2, appendErr: diskFailure}
	session, err := createWithStorage(storage, sourcePath, CreateOptions{
		ID: "poisoned-source", WorkingDir: directory,
		Now:        sequenceClock(time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)),
		NewEntryID: sequenceIDs("root", "uncertain"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	root, err := session.Append(context.Background(), mustUserMessage(t, "durable root", time.UnixMilli(1)), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sourceAtRoot, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Append(context.Background(), mustUserMessage(t, "uncertain tail", time.UnixMilli(2)), AppendOptions{})
	if !errors.Is(err, ErrCommitUnknown) || !errors.Is(err, diskFailure) || !session.Poisoned() {
		t.Fatalf("poisoning append = %v, poisoned=%t", err, session.Poisoned())
	}
	entries := session.Entries()
	if len(entries) != 1 || entries[0].ID() != root.ID() {
		t.Fatalf("poisoned memory entries = %v, want only durable root", entryIDs(entries))
	}
	if leaf, ok := session.LeafID(); !ok || leaf != root.ID() {
		t.Fatalf("poisoned memory leaf = (%q, %t), want %q", leaf, ok, root.ID())
	}
	sourceWithUncertainTail, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sourceAtRoot, sourceWithUncertainTail) {
		t.Fatal("fault seam did not put disk ahead of memory")
	}
	_, diskEntries, _, _, err := decodeSessionFile(sourcePath, sourceWithUncertainTail)
	if err != nil {
		t.Fatalf("synced uncertain tail is not a complete session record: %v", err)
	}
	if got, want := entryIDs(diskEntries), []string{"root", "uncertain"}; !equalIDs(got, want) {
		t.Fatalf("disk entries = %v, want %v", got, want)
	}
	createCalls := storage.createCalls
	tests := []struct {
		name string
		path string
		call func() (*Session, error)
	}{
		{
			name: "fork", path: filepath.Join(directory, "fork-target.jsonl"),
			call: func() (*Session, error) {
				return session.Fork(context.Background(), ExtractOptions{TargetPath: filepath.Join(directory, "fork-target.jsonl"), ID: "fork-target", WorkingDir: directory})
			},
		},
		{
			name: "extract", path: filepath.Join(directory, "extract-target.jsonl"),
			call: func() (*Session, error) {
				return session.ExtractBranch(context.Background(), "not-in-memory", ExtractOptions{TargetPath: filepath.Join(directory, "extract-target.jsonl"), ID: "extract-target", WorkingDir: directory})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := tt.call()
			if target != nil || !errors.Is(err, ErrPoisoned) {
				t.Fatalf("snapshot result = (%v, %v), want nil ErrPoisoned", target, err)
			}
			if storage.createCalls != createCalls {
				t.Fatalf("poisoned snapshot reached target create: calls=%d, want %d", storage.createCalls, createCalls)
			}
			if _, err := os.Stat(tt.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("poisoned snapshot created target: %v", err)
			}
		})
	}
	sourceAfter, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceWithUncertainTail, sourceAfter) {
		t.Fatal("poisoned Fork/Extract changed source bytes")
	}
}

func TestAppendValidationAndEncodingFailuresDoNotWrite(t *testing.T) {
	t.Parallel()

	storage := &fakeStorage{createCreated: true, appendStarted: true}
	session := newFakeSession(t, storage, sequenceIDs("entry-1", "entry-2"))
	if _, err := session.Append(context.Background(), llm.AssistantTextMessage{}, AppendOptions{}); err == nil {
		t.Fatal("invalid assistant message was accepted")
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
