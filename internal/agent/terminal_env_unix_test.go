//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/terminal"
	"github.com/cat3399/pi-go/internal/tool"
)

func TestSessionTerminalEnvironmentInjectsOnlyStableVariables(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	agentDir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	baseEnv := []string{
		"PATH=/usr/bin:/bin", "PS1=", "PS2=",
		"KEEP=value",
		"PI_EXECUTABLE=stale",
		"PI_SESSION_ID=stale",
		"PI_SESSION_FILE=stale",
		"PI_SESSION_AGENT_DIR=stale",
		"PI_SESSION_ENDPOINT=stale",
		"PI_SESSION_TOKEN=stale",
		"PI_PROVIDER=stale",
		"PI_MODEL=stale",
		"PI_REASONING_LEVEL=stale",
	}
	terminals := terminal.New(terminal.Options{CWD: cwd, Shell: "/bin/sh", Environment: baseEnv})
	bash, err := tool.NewBash(tool.BashOptions{WorkingDir: cwd, Runner: &sessionEnvironmentRunner{}, Environment: baseEnv})
	if err != nil {
		t.Fatal(err)
	}
	filesystem, err := tool.NewFilesystemSuite(tool.FilesystemOptions{WorkingDir: cwd})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewBuiltInRegistryWithTerminal(bash, filesystem, terminals)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewRegistryExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.CreateSessionManager(cwd, filepath.Join(agentDir, "sessions"), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sessionFile, _ := manager.SessionFile()
	definition, err := provider.NewToolDefinition(tool.TerminalToolName, "Persistent terminal", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: internalControlModel(t, "active-model"), ThinkingLevel: provider.ThinkingHigh,
		Tool: executor, Tools: []provider.ToolDefinition{definition}, Terminals: terminals,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(ctx) })
	if err := coordinator.ConfigureContextAccess(ContextAccessInstructions{}, map[string]string{
		"PI_SESSION_AGENT_DIR": agentDir,
		"PI_SESSION_ENDPOINT":  "http://127.0.0.1:12345/session",
		"PI_SESSION_TOKEN":     "session-token",
	}, nil); err != nil {
		t.Fatal(err)
	}

	_, selected := coordinator.loop.runtimeSnapshot()
	opened, err := executeNamedToolSafely(selected, ctx, "call-open", tool.TerminalToolName, []byte(`{"action":"open","wait_ms":0}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	details, _ := opened.Details.(map[string]any)
	agentTermID, _ := details["terminalId"].(string)
	if agentTermID == "" {
		t.Fatalf("missing terminalId in open details: %+v", opened.Details)
	}

	wantAgent := "AGENT|" + strings.Join([]string{
		"value", executable, manager.SessionID(), sessionFile, agentDir,
		"http://127.0.0.1:12345/session", "session-token", "", "", "",
	}, "|") + "|END\n"
	runArgs, _ := json.Marshal(map[string]any{
		"action":  "run",
		"id":      agentTermID,
		"command": `stty -echo; printf 'AGENT|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|END\n' "$KEEP" "$PI_EXECUTABLE" "$PI_SESSION_ID" "$PI_SESSION_FILE" "$PI_SESSION_AGENT_DIR" "$PI_SESSION_ENDPOINT" "$PI_SESSION_TOKEN" "$PI_PROVIDER" "$PI_MODEL" "$PI_REASONING_LEVEL"`,
		"wait_ms": 1000,
	})
	out, err := executeNamedToolSafely(selected, ctx, "call-run", tool.TerminalToolName, runArgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentText := out.Text
	deadline := time.Now().Add(4 * time.Second)
	for !strings.Contains(agentText, wantAgent) && time.Now().Before(deadline) {
		readArgs, _ := json.Marshal(map[string]any{"action": "read", "id": agentTermID, "wait_ms": 250})
		more, readErr := executeNamedToolSafely(selected, ctx, "call-read", tool.TerminalToolName, readArgs, nil)
		if readErr != nil {
			t.Fatal(readErr)
		}
		agentText += more.Text
	}
	if !strings.Contains(agentText, wantAgent) {
		t.Fatalf("agent terminal env output:\n%s\nwant substring:\n%s", agentText, wantAgent)
	}

	// User-opened terminal through AgentSession.Terminal strips all PI_* session variables.
	zero := time.Duration(0)
	wait := time.Second
	userOpened, err := coordinator.Terminal(ctx, terminal.Request{Action: terminal.Open, Wait: &zero})
	if err != nil {
		t.Fatal(err)
	}
	wantUser := "USER|value||||||||||END\n"
	userRes, err := coordinator.Terminal(ctx, terminal.Request{
		Action:  terminal.Run,
		ID:      userOpened.Terminal.ID,
		Command: `stty -echo; printf 'USER|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|END\n' "$KEEP" "$PI_EXECUTABLE" "$PI_SESSION_ID" "$PI_SESSION_FILE" "$PI_SESSION_AGENT_DIR" "$PI_SESSION_ENDPOINT" "$PI_SESSION_TOKEN" "$PI_PROVIDER" "$PI_MODEL" "$PI_REASONING_LEVEL"`,
		Wait:    &wait,
	})
	if err != nil {
		t.Fatal(err)
	}
	userText := strings.ReplaceAll(string(userRes.Data), "\r\n", "\n")
	cursor := userRes.Cursor
	deadline = time.Now().Add(4 * time.Second)
	for !strings.Contains(userText, wantUser) && time.Now().Before(deadline) {
		poll := 250 * time.Millisecond
		more, readErr := coordinator.Terminal(ctx, terminal.Request{Action: terminal.Read, ID: userOpened.Terminal.ID, After: &cursor, Wait: &poll})
		if readErr != nil {
			t.Fatal(readErr)
		}
		userText += strings.ReplaceAll(string(more.Data), "\r\n", "\n")
		cursor = more.Cursor
	}
	if !strings.Contains(userText, wantUser) {
		t.Fatalf("user terminal env output:\n%q\nwant substring:\n%q", userText, wantUser)
	}
}
