package application

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

func TestServiceImportSessionReconcilesReplacementIdentity(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	externalDir := t.TempDir()
	imported, err := session.CreateSessionManager(cwd, externalDir, session.NewSessionOptions{ID: "imported-session"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewUserTextMessage("imported prompt", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imported.AppendLLMMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("imported answer")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{text}, llm.FinishStop, llm.Usage{}, time.UnixMilli(2),
		llm.AssistantProvenance{Provider: "test", API: "test", Model: "model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imported.AppendLLMMessage(context.Background(), answer); err != nil {
		t.Fatal(err)
	}
	importPath, ok := imported.SessionFile()
	if !ok {
		t.Fatal("import fixture was not persisted")
	}
	if err := imported.Close(); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(ServiceOptions{
		Context: context.Background(), DisableReaper: true,
		Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	current, err := service.NewSession(context.Background(), NewSessionOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ImportSession(context.Background(), current.SessionID, importPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Cancelled || result.State.SessionID != "imported-session" {
		t.Fatalf("import result = %#v", result)
	}
	if _, active, err := service.LiveState(current.SessionID); err != nil || active {
		t.Fatalf("old identity active=%t err=%v", active, err)
	}
	if _, active, err := service.LiveState("imported-session"); err != nil || !active {
		t.Fatalf("new identity active=%t err=%v", active, err)
	}
	snapshot, err := service.SnapshotSession("imported-session", "")
	if err != nil || len(snapshot.Entries) < 2 {
		t.Fatalf("imported snapshot entries=%d err=%v", len(snapshot.Entries), err)
	}
}
