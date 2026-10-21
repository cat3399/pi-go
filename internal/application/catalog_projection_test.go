package application

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

func addCatalogManagedSession(t testing.TB, service *Service, manager *session.SessionManager) *managedSession {
	t.Helper()
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.NewModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "catalog-model", Name: "Catalog model", Input: []provider.InputKind{provider.InputText}, ContextWindow: 200000, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	factory := func(_ context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		coordinator, err := agent.NewSession(agent.SessionConfig{Provider: implementation, Model: model, SessionManager: options.SessionManager, ThinkingLevel: provider.ThinkingOff})
		if err != nil {
			return agentruntime.CreateResult{}, err
		}
		return agentruntime.CreateResult{Session: coordinator, Services: &agentruntime.Services{CWD: manager.Cwd(), AgentDir: service.AgentDir(), Provider: implementation}}, nil
	}
	runtime, err := agentruntime.Create(context.Background(), factory, agentruntime.InitialOptions{CWD: manager.Cwd(), AgentDir: service.AgentDir(), SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := service.adoptRuntime(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.register(managed); err != nil {
		t.Fatal(err)
	}
	return managed
}

func appendCatalogMessage(t testing.TB, manager *session.SessionManager, text string, at time.Time, assistant bool) session.Entry {
	t.Helper()
	var message llm.ConversationMessage
	var err error
	if assistant {
		block, blockErr := llm.NewTextBlock(text)
		if blockErr != nil {
			t.Fatal(blockErr)
		}
		message, err = llm.NewAssistantTextMessage([]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, at, llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "catalog-model"})
	} else {
		message, err = llm.NewUserTextMessage(text, at)
	}
	if err != nil {
		t.Fatal(err)
	}
	entry, err := manager.AppendLLMMessage(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestApplicationSnapshotKeepsLiveAndDurableCatalogConsistent(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	cwd := t.TempDir()
	dir, err := session.SessionDirForAgentDir(cwd, service.AgentDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.CreateSessionManager(cwd, dir, session.NewSessionOptions{ID: "live-catalog"})
	if err != nil {
		t.Fatal(err)
	}
	firstAt, lastAt := time.UnixMilli(1000), time.UnixMilli(3000)
	first := appendCatalogMessage(t, manager, "pending question", firstAt, false)
	addCatalogManagedSession(t, service, manager)
	snapshot, err := service.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].MessageCount != 1 || snapshot.Sessions[0].FirstMessage != "pending question" {
		t.Fatalf("pending catalog = %#v", snapshot)
	}
	path, _ := manager.SessionFile()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog flushed user-only session: %v", err)
	}
	if _, err := service.ListFiles(context.Background(), cwd); err != nil {
		t.Fatalf("pending session CWD not authorized: %v", err)
	}
	appendCatalogMessage(t, manager, "answer", time.UnixMilli(2000), true)
	appendCatalogMessage(t, manager, "next question", lastAt, false)
	if _, err := manager.AppendSessionInfo(context.Background(), "Live title"); err != nil {
		t.Fatal(err)
	}
	fork, err := manager.CloneBranchedSession(context.Background(), first.ID())
	if err != nil {
		t.Fatal(err)
	}
	addCatalogManagedSession(t, service, fork)
	snapshot, err = service.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != service.CurrentRevision() || snapshot.AgentDir != service.AgentDir() || snapshot.DefaultCWD != service.DefaultCWD() || len(snapshot.RunningSessionIDs) != 0 {
		t.Fatalf("application fields = %#v", snapshot)
	}
	if len(snapshot.Sessions) != 2 || len(snapshot.Projects) != 1 || snapshot.Projects[0].SessionCount != 2 || snapshot.Projects[0].Path != cwd || !snapshot.Projects[0].Modified.Equal(lastAt) {
		t.Fatalf("project/session disagreement = %#v", snapshot)
	}
	parent, child := snapshot.Sessions[0], snapshot.Sessions[1]
	if parent.ID != "live-catalog" || parent.Name != "Live title" || parent.MessageCount != 3 || child.ParentSessionID != parent.ID || child.MessageCount != 1 {
		t.Fatalf("live history/fork relationship = %#v", snapshot.Sessions)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	updated, err := service.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if updated[0].Name != "" || !updated[0].Modified.Equal(lastAt) || parent.Name != "Live title" {
		t.Fatalf("clear or snapshot ownership changed: %#v", updated)
	}
}

func TestApplicationSnapshotUsesSameActivityForProjectsDuringAppend(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	cwd := service.DefaultCWD()
	dir, err := session.SessionDirForAgentDir(cwd, service.AgentDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.CreateSessionManager(cwd, dir, session.NewSessionOptions{ID: "snapshot-writer"})
	if err != nil {
		t.Fatal(err)
	}
	appendCatalogMessage(t, manager, "answer", time.UnixMilli(1), true)
	addCatalogManagedSession(t, service, manager)
	done := make(chan error, 1)
	go func() {
		for i := 2; i <= 30; i++ {
			message, err := llm.NewUserTextMessage("next", time.UnixMilli(int64(i)))
			if err == nil {
				_, err = manager.AppendLLMMessage(context.Background(), message)
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		snapshot, err := service.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Sessions) != 1 || len(snapshot.Projects) != 1 || !snapshot.Sessions[0].Modified.Equal(snapshot.Projects[0].Modified) {
			t.Fatalf("snapshot crossed catalog versions: %#v", snapshot)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
	}
}

func TestResourceDirectoriesRespectLiveCWDOverridesAndHiddenProjects(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	original, override, explicit := t.TempDir(), t.TempDir(), t.TempDir()
	dir, err := session.SessionDirForAgentDir(original, service.AgentDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.CreateSessionManager(original, dir, session.NewSessionOptions{ID: "cwd-override"})
	if err != nil {
		t.Fatal(err)
	}
	appendCatalogMessage(t, manager, "question", time.UnixMilli(1000), false)
	appendCatalogMessage(t, manager, "answer", time.UnixMilli(2000), true)
	path, _ := manager.SessionFile()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err = session.OpenSessionManager(path, dir, override)
	if err != nil {
		t.Fatal(err)
	}
	addCatalogManagedSession(t, service, manager)
	if _, err := service.ListFiles(context.Background(), override); err != nil {
		t.Fatalf("live CWD = %v", err)
	}
	if _, err := service.ListFiles(context.Background(), original); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("stale header CWD was authorized: %v", err)
	}
	for _, project := range []string{explicit, override} {
		if _, err := service.AddProject(context.Background(), project); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ListFiles(context.Background(), project); err != nil {
			t.Fatal(err)
		}
		if err := service.RemoveProject(context.Background(), project); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.ListFiles(context.Background(), explicit); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("hidden project was authorized: %v", err)
	}
	if _, err := service.ListFiles(context.Background(), override); err != nil {
		t.Fatalf("hiding a project revoked its live session CWD: %v", err)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), "new name"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListFiles(context.Background(), original); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("header refresh lost live override: %v", err)
	}
}

func TestResourceDirectoriesPreserveCatalogChoiceForDuplicateSessionIDs(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	paths := []string{t.TempDir(), t.TempDir()}
	for index, cwd := range paths {
		dir, err := session.SessionDirForAgentDir(cwd, service.AgentDir())
		if err != nil {
			t.Fatal(err)
		}
		manager, err := session.CreateSessionManager(cwd, dir, session.NewSessionOptions{ID: "duplicate-id"})
		if err != nil {
			t.Fatal(err)
		}
		appendCatalogMessage(t, manager, "answer", time.UnixMilli(int64(index+1)), true)
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := service.ListSessions()
	if err != nil || len(listed) != 1 {
		t.Fatalf("duplicate discovery = %#v, %v", listed, err)
	}
	for _, cwd := range paths {
		_, err := service.ListFiles(context.Background(), cwd)
		if cwd == listed[0].CWD && err != nil || cwd != listed[0].CWD && !errors.Is(err, ErrResourceAccessDenied) {
			t.Fatalf("resource root %q differs from catalog %q: %v", cwd, listed[0].CWD, err)
		}
	}
}

func TestRunningIDsReadsLifecycleWithoutProjectingHistory(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	manager, err := session.InMemorySessionManager(service.DefaultCWD(), session.NewSessionOptions{ID: "busy-summary"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		appendCatalogMessage(t, manager, strings.Repeat("history ", 1024), time.UnixMilli(int64(i+1)), false)
	}
	managed := addCatalogManagedSession(t, service, manager)
	if ids := service.RunningIDs(); len(ids) != 0 {
		t.Fatalf("idle IDs = %v", ids)
	}
	// Prompt admission is observable before AgentSession starts streaming.
	if _, _, err := managed.session.beginPrompt(); err != nil {
		t.Fatal(err)
	}
	defer managed.session.operations.Done()
	defer managed.session.finishPromptState()
	want := []string{"busy-summary"}
	if ids := service.RunningIDs(); !reflect.DeepEqual(ids, want) {
		t.Fatalf("admitted prompt IDs = %v", ids)
	}
	allocations := testing.AllocsPerRun(10, func() { _ = service.RunningIDs() })
	if allocations > 20 {
		t.Fatalf("busy query still projects history: %.0f allocations", allocations)
	}
	managed.session.finishPromptState()
	if ids := service.RunningIDs(); len(ids) != 0 {
		t.Fatalf("settled prompt IDs = %v", ids)
	}
}
