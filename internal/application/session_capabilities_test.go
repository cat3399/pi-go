package application

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

func TestSessionCapabilitiesReadDurableContentAndDeleteWithReparenting(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	artifactDir := t.TempDir()
	artifactPath := filepath.Join(artifactDir, "pi-bash-capabilities.log")
	if err := os.WriteFile(artifactPath, []byte("complete bash output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionDir, err := session.SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := session.CreateSessionManager(cwd, sessionDir, session.NewSessionOptions{ID: "target-session"})
	if err != nil {
		t.Fatal(err)
	}
	targetPath, ok := target.SessionFile()
	if !ok {
		t.Fatal("target session is not persistent")
	}
	user, err := llm.NewUserTextMessage("render <script>alert(1)</script> safely", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.AppendLLMMessage(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	thinking, err := llm.NewThinkingBlock("private planning details")
	if err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("public answer")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantRichMessage(
		[]llm.AssistantBlock{thinking, text}, llm.FinishStop, llm.Usage{}, time.UnixMilli(2),
		llm.AssistantProvenance{Provider: "test", API: "test", Model: "model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	assistantEntry, err := target.AppendLLMMessage(context.Background(), assistant)
	if err != nil {
		t.Fatal(err)
	}
	bash, err := agentmsg.NewBashExecution(agentmsg.BashExecution{
		Command: "long-command", Output: "truncated", Truncated: true,
		FullOutputPath: artifactPath, At: time.UnixMilli(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.AppendMessage(context.Background(), bash); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}

	child, err := session.CreateSessionManager(cwd, sessionDir, session.NewSessionOptions{
		ID: "child-session", ParentSession: targetPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	childPath, ok := child.SessionFile()
	if !ok {
		t.Fatal("child session is not persistent")
	}
	childUser, err := llm.NewUserTextMessage("continue in child", time.UnixMilli(4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.AppendLLMMessage(context.Background(), childUser); err != nil {
		t.Fatal(err)
	}
	childText, err := llm.NewTextBlock("child answer")
	if err != nil {
		t.Fatal(err)
	}
	childAssistant, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{childText}, llm.FinishStop, llm.Usage{}, time.UnixMilli(5),
		llm.AssistantProvenance{Provider: "test", API: "test", Model: "model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.AppendLLMMessage(context.Background(), childAssistant); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(ServiceOptions{
		Context: context.Background(), DisableReaper: true,
		Production: app.ProductionConfig{
			WorkingDir: cwd, AgentDir: agentDir, BashArtifactDirectory: artifactDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	gotThinking, err := service.SessionThinking(context.Background(), "target-session", assistantEntry.ID(), 0)
	if err != nil || gotThinking != "private planning details" {
		t.Fatalf("SessionThinking() = %q, %v", gotThinking, err)
	}
	if _, err := service.SessionThinking(context.Background(), "target-session", assistantEntry.ID(), 1); !errors.Is(err, ErrThinkingNotFound) {
		t.Fatalf("missing thinking error = %v", err)
	}
	output, err := service.OpenBashOutput(context.Background(), "target-session", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(output.Reader)
	closeErr := output.Reader.Close()
	if err != nil || closeErr != nil || string(content) != "complete bash output\n" {
		t.Fatalf("OpenBashOutput() = %q, read=%v close=%v", content, err, closeErr)
	}
	unreferenced := filepath.Join(artifactDir, "pi-bash-unreferenced.log")
	if _, err := service.OpenBashOutput(context.Background(), "target-session", unreferenced); !errors.Is(err, ErrBashOutputForbidden) {
		t.Fatalf("unreferenced bash output error = %v", err)
	}

	exported, err := service.ExportSession(context.Background(), "target-session")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(exported.HTML), "<!doctype html>") || !strings.HasSuffix(exported.FileName, ".html") {
		t.Fatalf("export = %q, %q", exported.FileName, exported.HTML[:min(len(exported.HTML), 40)])
	}
	if strings.Contains(string(exported.HTML), "<script>alert(1)</script>") {
		t.Fatal("session content was embedded as executable HTML")
	}

	if err := service.DeleteSession(context.Background(), "target-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted target still exists: %v", err)
	}
	reopened, err := session.OpenSessionManager(childPath, sessionDir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if parent, present := reopened.Header().ParentSession(); present || parent != "" {
		t.Fatalf("child still has deleted parent %q", parent)
	}
	sessions, err := service.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "child-session" || sessions[0].ParentSessionID != "" {
		t.Fatalf("sessions after deletion = %#v", sessions)
	}
}
