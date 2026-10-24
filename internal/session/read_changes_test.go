package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
)

func TestReadChangesCoalesceWithoutBlockingAndKeepActualLeaf(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	initial, updates, cancel := m.SubscribeChanges()
	defer cancel()
	var root Entry
	for i := 0; i < 1000; i++ {
		entry, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "same text", time.UnixMilli(1)))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			root = entry
		}
	}
	if err := m.Branch(root.ID()); err != nil {
		t.Fatal(err)
	}
	change := <-updates
	snapshot := mustReadSnapshot(t, m, SnapshotOptions{})
	if change.Version != snapshot.Version || change.LeafID != root.ID() || change.Version.Generation != initial.Version.Generation || change.Version.Log != 1000 {
		t.Fatalf("change = %#v, snapshot = %#v", change, snapshot)
	}
	if err := m.Branch("missing"); !errors.Is(err, ErrEntryNotFound) {
		t.Fatal(err)
	}
	select {
	case got := <-updates:
		t.Fatalf("failed no-op published a change: %#v", got)
	default:
	}
	if _, _, err := m.NewSession(NewSessionOptions{ID: initial.SessionID}); err != nil {
		t.Fatal(err)
	}
	if replacement := <-updates; replacement.Version.Generation == initial.Version.Generation || replacement.Version.Log != 0 {
		t.Fatalf("replacement = %#v", replacement)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if closed, ok := <-updates; !ok || !closed.Closed {
		t.Fatalf("close notification = %#v, %v", closed, ok)
	}
	if _, ok := <-updates; ok {
		t.Fatal("close left subscription open")
	}
}

func TestAppendReceiptPreservesAcceptedEntryWhenDelayedFlushFails(t *testing.T) {
	dir := t.TempDir()
	m, err := CreateSessionManager(dir, dir, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, updates, cancel := m.SubscribeChanges()
	defer cancel()
	user, _ := agentmsg.NewLLM(mustUserMessage(t, "user", time.UnixMilli(1)))
	first, err := m.AppendMessageWithReceipt(context.Background(), user)
	if err != nil || !first.Accepted || first.Durable || first.Uncertain {
		t.Fatalf("user receipt = %#v, %v", first, err)
	}
	file, _ := m.SessionFile()
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	assistant, _ := agentmsg.NewLLM(managerAssistant(t, "assistant", time.UnixMilli(2)))
	failed, err := m.AppendMessageWithReceipt(context.Background(), assistant)
	if err == nil || !failed.Accepted || failed.Durable || failed.Uncertain || failed.Version.Log != 2 {
		t.Fatalf("failed snapshot receipt = %#v, %v", failed, err)
	}
	if entry, ok := m.Entry(failed.Entry.ID()); !ok || entry.ID() != failed.Entry.ID() {
		t.Fatal("accepted receipt has no matching manager entry")
	}
	if change := <-updates; change.Version != failed.Version || change.Durable || change.Uncertain {
		t.Fatalf("failed snapshot change = %#v", change)
	}
	if err := os.Rename(file, file+".blocked"); err != nil {
		t.Fatal(err)
	}
	last, err := m.AppendMessageWithReceipt(context.Background(), user)
	if err != nil || !last.Accepted || !last.Durable || last.Version.Generation != first.Version.Generation || last.Version.Log != 3 {
		t.Fatalf("retry flush receipt = %#v, %v", last, err)
	}
}

func TestReadChangesConcurrentSubscribeCancelAndClose(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	message := mustUserMessage(t, "message", time.UnixMilli(1))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				_, _, cancel := m.SubscribeChanges()
				cancel()
				cancel()
			}
		})
	}
	wg.Go(func() {
		for i := 0; i < 100; i++ {
			if _, err := m.AppendLLMMessage(context.Background(), message); err != nil {
				t.Error(err)
				return
			}
		}
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
}

func TestAppendReceiptInvalidatesOnUncertainWriteWithoutInventingEntry(t *testing.T) {
	storage := &fakeStorage{createCreated: true, appendStarted: true, appendErr: fmt.Errorf("sync failed")}
	store, err := createWithStorage(storage, filepath.Join(t.TempDir(), "uncertain.jsonl"), CreateOptions{ID: "uncertain", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := &SessionManager{store: store, persist: true, flushed: true}
	defer m.Close()
	initial, updates, cancel := m.SubscribeChanges()
	defer cancel()
	message, _ := agentmsg.NewLLM(mustUserMessage(t, "uncertain", time.UnixMilli(1)))
	receipt, err := m.AppendMessageWithReceipt(context.Background(), message)
	if !errors.Is(err, ErrCommitUnknown) || receipt.Accepted || receipt.Durable || !receipt.Uncertain || receipt.Entry.ID() != "" {
		t.Fatalf("uncertain receipt = %#v, %v", receipt, err)
	}
	if change := <-updates; !change.Uncertain || change.Version.Log != initial.Version.Log || change.Durable {
		t.Fatalf("uncertain change = %#v", change)
	}
}
