package tui

import (
	"context"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

type treeRecordingAPI struct {
	modelTestAPI
	snapshot application.SessionSnapshot
	commands []application.Command
}

func (a *treeRecordingAPI) SnapshotSession(string, string) (application.SessionSnapshot, error) {
	return a.snapshot, nil
}

func (a *treeRecordingAPI) Dispatch(_ context.Context, _ string, command application.Command) (application.CommandResult, error) {
	a.commands = append(a.commands, command)
	switch command.(type) {
	case application.ForkCommand:
		id := "forked-session"
		return application.ForkResult{SessionID: &id}, nil
	case application.NavigateTreeCommand:
		return application.NavigateTreeResult{}, nil
	case application.AbortBranchSummaryCommand:
		return application.AbortBranchSummaryResult{}, nil
	default:
		return nil, application.ErrInvalidCommand
	}
}

func TestTreeAndForkSelectorsProjectDurableSnapshot(t *testing.T) {
	snapshot := treeSelectorSnapshot(t)
	treeItems := treeSelectorItems(snapshot)
	if len(treeItems) != 2 || treeItems[0].Key == treeItems[1].Key || !treeItems[1].Current {
		t.Fatalf("tree items = %#v", treeItems)
	}
	if treeItems[0].Badge != "user" || treeItems[0].Title != "• first question" {
		t.Fatalf("first tree item = %#v", treeItems[0])
	}
	if treeItems[1].Title != "• second question" {
		t.Fatalf("linear child was indented as a branch = %#v", treeItems[1])
	}
	forkItems := forkSelectorItems(snapshot)
	if len(forkItems) != 2 || forkItems[0].Title != "first question" || forkItems[1].Title != "second question" {
		t.Fatalf("fork items = %#v", forkItems)
	}
}

func TestTreeNavigationTracksSummaryForDedicatedAbort(t *testing.T) {
	api := &treeRecordingAPI{}
	model := newModelWithAPIForTest(t, api)
	command := model.dispatchTreeNavigation("entry-1", agent.NavigateTreeOptions{Summarize: true})
	if command == nil || model.branchSummaryRequest == 0 || !model.busy() {
		t.Fatalf("summary request = %d, busy=%v", model.branchSummaryRequest, model.busy())
	}
	abort := model.abort()
	if abort == nil {
		t.Fatal("branch summary abort did not dispatch")
	}
	message, ok := abort().(commandFinishedMsg)
	if !ok || message.err != nil {
		t.Fatalf("abort message = %#v", message)
	}
	if len(api.commands) != 1 {
		t.Fatalf("commands before summary execution = %#v", api.commands)
	}
	if _, ok := api.commands[0].(application.AbortBranchSummaryCommand); !ok {
		t.Fatalf("abort command = %T", api.commands[0])
	}
}

func TestCloneUsesCurrentLeafAtExactPosition(t *testing.T) {
	api := &treeRecordingAPI{}
	model := newModelWithAPIForTest(t, api)
	leaf := "leaf-entry"
	model.snapshotLeafID = &leaf
	command := model.cloneCurrentSession()
	if command == nil {
		t.Fatal("clone did not dispatch")
	}
	message, ok := command().(commandFinishedMsg)
	if !ok || message.err != nil {
		t.Fatalf("clone message = %#v", message)
	}
	fork, ok := api.commands[0].(application.ForkCommand)
	if !ok || fork.EntryID != leaf || fork.Position != agent.ForkAt {
		t.Fatalf("clone command = %#v", api.commands[0])
	}
}

func treeSelectorSnapshot(t *testing.T) application.SessionSnapshot {
	t.Helper()
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	for index, text := range []string{"first question", "second question"} {
		message, messageErr := llm.NewUserTextMessage(text, time.UnixMilli(int64(index+1)))
		if messageErr != nil {
			t.Fatal(messageErr)
		}
		if _, appendErr := manager.AppendLLMMessage(context.Background(), message); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	leaf, ok := manager.LeafID()
	if !ok {
		t.Fatal("snapshot has no leaf")
	}
	return application.SessionSnapshot{SessionID: "session-1", Entries: manager.Entries(), Tree: manager.Tree(), LeafID: &leaf}
}
