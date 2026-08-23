package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

func TestProjectCatalogCombinesDurableSessionsAndExplicitProjects(t *testing.T) {
	projectWithSession := t.TempDir()
	emptyProject := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	sessionDir, err := session.SessionDirForAgentDir(projectWithSession, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.CreateSessionManager(
		projectWithSession,
		sessionDir,
		session.NewSessionOptions{ID: "project-session"},
	)
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewUserTextMessage("project history", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("project response")
	if err != nil {
		t.Fatal(err)
	}
	response, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{text},
		llm.FinishStop,
		llm.Usage{},
		time.Now(),
		llm.AssistantProvenance{Provider: "test", API: "test", Model: "test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	sessionPath, ok := manager.SessionFile()
	if !ok {
		t.Fatal("session fixture was not persisted")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	service := projectTestService(t, projectWithSession, agentDir)
	projects, err := service.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != projectWithSession || projects[0].SessionCount != 1 {
		t.Fatalf("session projects = %#v", projects)
	}

	subscription, err := service.SubscribeEvents(service.CurrentRevision())
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	added, err := service.AddProject(context.Background(), emptyProject)
	if err != nil {
		t.Fatal(err)
	}
	if added.Path != emptyProject {
		t.Fatalf("added project = %#v", added)
	}
	select {
	case event := <-subscription.Events:
		projectEvent, ok := event.Value.(ProjectCatalogEvent)
		if !ok || projectEvent.Change != ProjectAdded || projectEvent.Path != emptyProject {
			t.Fatalf("project event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("project event was not published")
	}

	projects, err = service.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].Path != emptyProject {
		t.Fatalf("projects after add = %#v", projects)
	}
	if err := service.RemoveProject(context.Background(), projectWithSession); err != nil {
		t.Fatal(err)
	}
	projects, err = service.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != emptyProject {
		t.Fatalf("projects after remove = %#v", projects)
	}
	if _, err := os.Stat(projectWithSession); err != nil {
		t.Fatalf("project directory was changed: %v", err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("session history was changed: %v", err)
	}
}

func TestProjectCatalogPersistsAddsAndRemovals(t *testing.T) {
	defaultCWD := t.TempDir()
	project := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")

	first := projectTestService(t, defaultCWD, agentDir)
	if _, err := first.AddProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	second := projectTestService(t, defaultCWD, agentDir)
	projects, err := second.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != project {
		t.Fatalf("reopened projects = %#v", projects)
	}
	if _, err := second.authorizeResourcePath(project, true); err != nil {
		t.Fatalf("reopened project authorization: %v", err)
	}
	if err := second.RemoveProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	if _, err := second.authorizeResourcePath(project, true); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("removed project authorization error = %v", err)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	third := projectTestService(t, defaultCWD, agentDir)
	projects, err = third.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("removed projects after reopen = %#v", projects)
	}
}

func projectTestService(t *testing.T, cwd, agentDir string) *Service {
	t.Helper()
	service, err := NewService(ServiceOptions{
		Context: context.Background(),
		Production: app.ProductionConfig{
			WorkingDir:  cwd,
			AgentDir:    agentDir,
			Environment: []string{},
		},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Errorf("close service: %v", err)
		}
	})
	return service
}
