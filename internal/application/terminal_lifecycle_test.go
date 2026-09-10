//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package application

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/terminal"
)

func TestLiveTerminalRetainsIdleSessionWithoutMarkingAgentBusy(t *testing.T) {
	cwd, agentDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(`{"providers":{"terminal-test":{"api":"openai-completions","baseUrl":"http://127.0.0.1:1","apiKey":"test","models":[{"id":"test","contextWindow":4096,"maxTokens":512}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(ServiceOptions{Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{"PATH=/usr/bin:/bin", "PS1="}, BashShellPath: "/bin/sh"}, DisableReaper: true, IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	state, err := s.NewSession(context.Background(), NewSessionOptions{CWD: cwd, Provider: "terminal-test", ModelID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Dispatch(context.Background(), state.SessionID, GetToolsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	wait := time.Duration(0)
	opened, err := s.Dispatch(context.Background(), state.SessionID, TerminalCommand{Request: terminal.Request{Action: terminal.Open, Wait: &wait}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Dispatch(context.Background(), state.SessionID, GetToolsCommand{})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("surface terminal creation changed Agent tools: %v", err)
	}
	managed := s.sessions[state.SessionID]
	managed.lastAccess.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	s.reapOnce(time.Now())
	if len(s.RunningIDs()) != 0 {
		t.Fatal("live terminal made the Agent appear busy")
	}
	if _, live, err := s.LiveState(state.SessionID); err != nil || !live {
		t.Fatalf("terminal session was reaped: %t %v", live, err)
	}
	tid := opened.(TerminalResult).Result.Terminal.ID
	if _, err := s.Dispatch(context.Background(), state.SessionID, TerminalCommand{Request: terminal.Request{Action: terminal.Close, ID: tid}}); err != nil {
		t.Fatal(err)
	}
	after, err = s.Dispatch(context.Background(), state.SessionID, GetToolsCommand{})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("surface terminal shutdown changed Agent tools: %v", err)
	}
	managed.lastAccess.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	s.reapOnce(time.Now())
	if _, live, err := s.LiveState(state.SessionID); err != nil || live {
		t.Fatalf("exited terminal retained idle session: %t %v", live, err)
	}
}
