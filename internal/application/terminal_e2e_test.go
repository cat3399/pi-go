//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/terminal"
)

// Only the provider is deterministic. Tool registration/selection, PTY,
// Application commands, reload and durable session storage are production.
func TestProductionTerminalSharedByAgentAndSurfacesAcrossReload(t *testing.T) {
	const modelID = "terminal-e2e"
	var requests atomic.Int32
	var terminalID atomic.Value
	var requestPrefix []byte
	readOnlyChecks := make(chan func(), 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(writer, "bad request", 400)
			return
		}
		turn := requests.Add(1)
		tools, _ := payload["tools"].([]any)
		hasTerminal := false
		hasSearch := false
		for _, name := range applicationE2EToolNames(tools) {
			if name == "terminal" {
				hasTerminal = true
			}
			if name == "tool_search" {
				hasSearch = true
			}
		}
		if !hasTerminal || hasSearch {
			t.Errorf("built-in tools at turn %d: terminal=%t, search=%t", turn, hasTerminal, hasSearch)
		}
		messages := payload["messages"].([]any)
		prefix, err := json.Marshal(map[string]any{"system": messages[0], "tools": tools})
		if err != nil {
			t.Error(err)
			http.Error(writer, "invalid request prefix", 500)
			return
		}
		if turn == 1 {
			requestPrefix = prefix
			if !bytes.Contains(prefix, []byte("- terminal:")) {
				t.Error("terminal is missing from the initial system prompt")
			}
		} else if !bytes.Equal(prefix, requestPrefix) {
			t.Errorf("terminal use changed the system prompt or tool definitions at turn %d", turn)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		switch turn {
		case 1:
			applicationE2EWriteSSE(t, writer,
				map[string]any{"id": "open-call", "model": modelID, "choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "open-terminal", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"action":"open","wait_ms":0}`},
				}}}}}},
				map[string]any{"id": "open-call", "choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}},
			)
		case 2, 4:
			if turn == 2 {
				last := messages[len(messages)-1].(map[string]any)
				fields := strings.Fields(fmt.Sprint(last["content"]))
				if last["tool_call_id"] != "open-terminal" || len(fields) < 2 || fields[0] != "Terminal" {
					t.Errorf("open did not return a terminal ID: %#v", last)
					http.Error(writer, "terminal ID missing", 500)
					return
				}
				terminalID.Store(fields[1])
			}
			arguments := map[string]any{"action": "run", "id": terminalID.Load(), "command": "stty -echo; export TERMINAL_OWNER=agent; printf 'AGENT_OK\\n'", "wait_ms": 1000}
			if turn == 4 {
				(<-readOnlyChecks)()
				arguments["command"] = "printf 'owner=%s\\n' \"$TERMINAL_OWNER\""
			}
			encoded, _ := json.Marshal(arguments)
			applicationE2EWriteSSE(t, writer,
				map[string]any{"id": "terminal-call", "model": modelID, "choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": fmt.Sprintf("terminal-%d", turn), "type": "function", "function": map[string]any{"name": "terminal", "arguments": string(encoded)},
				}}}}}},
				map[string]any{"id": "terminal-call", "choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}},
			)
		case 3, 5:
			messages, _ := payload["messages"].([]any)
			last, _ := messages[len(messages)-1].(map[string]any)
			content, _ := json.Marshal(last["content"])
			expected := "AGENT_OK"
			if turn == 5 {
				expected = "owner=user"
			}
			if !bytes.Contains(content, []byte(expected)) {
				t.Errorf("tool result omitted %s: %s", expected, content)
			}
			if turn == 5 && bytes.Contains(content, []byte("USER_OK")) {
				t.Errorf("run repeated earlier user output: %s", content)
			}
			applicationE2EWriteTextSSE(t, writer, "done", modelID, "Terminal verified")
		default:
			t.Errorf("unexpected provider turn %d", turn)
		}
	}))
	defer server.Close()
	cwd, agentDir := t.TempDir(), t.TempDir()
	models, _ := json.Marshal(map[string]any{"providers": map[string]any{"terminal-test": map[string]any{
		"api": "openai-completions", "baseUrl": server.URL + "/v1", "apiKey": "terminal-test-key",
		"models": []any{map[string]any{"id": modelID, "name": "Terminal E2E", "contextWindow": 32000, "maxTokens": 1024}},
	}}})
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0600); err != nil {
		t.Fatal(err)
	}
	service := applicationE2ENewService(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{"PATH=/usr/bin:/bin", "PS1=", "PS2="}, BashShellPath: "/bin/sh", OpenAIHTTPClient: server.Client()})
	t.Cleanup(func() { applicationE2ECloseService(t, service) })
	state, err := service.NewSession(context.Background(), application.NewSessionOptions{CWD: cwd, Provider: "terminal-test", ModelID: modelID})
	if err != nil {
		t.Fatal(err)
	}
	id := state.SessionID
	events, err := service.SubscribeEvents(service.CurrentRevision())
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	// The built-in tool is available from the first model request. Open returns
	// the ID that the next run must use; no surface command enables the tool.
	applicationE2EDispatchAndWait(t, service, events, id, "Start a persistent terminal.")
	call := func(req terminal.Request) terminal.Result {
		t.Helper()
		value, err := service.Dispatch(context.Background(), id, application.TerminalCommand{Request: req})
		if err != nil {
			t.Fatal(err)
		}
		return value.(application.TerminalResult).Result
	}
	list := call(terminal.Request{Action: terminal.List}).Terminals
	if len(list) != 1 || list[0].State != "running" {
		t.Fatalf("Agent terminal not available to surface: %+v", list)
	}
	tid := list[0].ID
	if terminalID.Load() != tid {
		t.Fatalf("model and surface see different terminal IDs: %v / %s", terminalID.Load(), tid)
	}
	zero := time.Duration(0)
	after := int64(0)
	raw := call(terminal.Request{Action: terminal.Read, ID: tid, After: &after, Wait: &zero})
	if !bytes.Contains(raw.Data, []byte("AGENT_OK")) {
		t.Fatalf("surface output: %q", raw.Data)
	}
	second := call(terminal.Request{Action: terminal.Read, ID: tid, After: &after, Wait: &zero})
	if !bytes.Equal(raw.Data, second.Data) {
		t.Fatal("surface readers consumed each other's output")
	}
	before, _, err := service.LiveState(id)
	if err != nil {
		t.Fatal(err)
	}
	call(terminal.Request{Action: terminal.Run, ID: tid, Command: "export TERMINAL_OWNER=user; printf 'USER_OK\\n'", Wait: &zero})
	after = raw.Cursor
	var userOutput []byte
	deadline := time.Now().Add(3 * time.Second)
	for !bytes.Contains(userOutput, []byte("USER_OK")) && time.Now().Before(deadline) {
		r := call(terminal.Request{Action: terminal.Read, ID: tid, After: &after, Mode: "follow"})
		userOutput = append(userOutput, r.Data...)
		after = r.Cursor
	}
	if !bytes.Contains(userOutput, []byte("USER_OK")) {
		t.Fatalf("interactive user output: %q", userOutput)
	}
	afterUser, _, err := service.LiveState(id)
	if err != nil || afterUser.MessageCount != before.MessageCount || afterUser.IsBashRunning || afterUser.IsPromptRunning || len(service.RunningIDs()) != 0 {
		t.Fatalf("terminal input changed Agent state: %+v, %v", afterUser, err)
	}
	if _, err := service.Dispatch(context.Background(), id, application.ReloadCommand{}); err != nil {
		t.Fatal(err)
	}
	list = call(terminal.Request{Action: terminal.List}).Terminals
	if len(list) != 1 || list[0].ID != tid || list[0].State != "running" {
		t.Fatalf("reload replaced terminal owner: %+v", list)
	}
	readOnlyChecks <- func() {
		for _, action := range []terminal.Action{terminal.Open, terminal.Run, terminal.Write, terminal.Interrupt, terminal.Close} {
			_, err := service.Dispatch(context.Background(), id, application.TerminalCommand{Request: terminal.Request{
				Action: action, ID: tid, Command: "export TERMINAL_OWNER=unexpected", Input: "export TERMINAL_OWNER=unexpected\n", Wait: &zero,
			}})
			if !errors.Is(err, agent.ErrBusy) {
				t.Errorf("user %s during Agent run = %v, want ErrBusy", action, err)
			}
		}
		for _, req := range []terminal.Request{
			{Action: terminal.List},
			{Action: terminal.Read, ID: tid, Wait: &zero},
			{Action: terminal.Resize, ID: tid, Cols: 100, Rows: 30},
		} {
			if _, err := service.Dispatch(context.Background(), id, application.TerminalCommand{Request: req}); err != nil {
				t.Errorf("user view %s during Agent run: %v", req.Action, err)
			}
		}
	}
	applicationE2EDispatchAndWait(t, service, events, id, "Read state left by the user.")
	closed := call(terminal.Request{Action: terminal.Close, ID: tid})
	if closed.Terminal.State != "exited" {
		t.Fatalf("close failed: %+v", closed)
	}
	if _, err := os.Stat(list[0].LogPath); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(*state.SessionFile)
	if err != nil || !strings.Contains(string(stored), tid) || !strings.Contains(string(stored), "terminalId") {
		t.Fatalf("terminal tool details were not persisted: %v", err)
	}
	if bytes.Contains(stored, []byte(`"addedToolNames"`)) || bytes.Contains(stored, []byte(`"tool_search"`)) {
		t.Fatal("built-in terminal use introduced dynamic tool loading")
	}
	if requests.Load() != 5 {
		t.Fatalf("provider turn count = %d, want open, run, result, run, result", requests.Load())
	}
}
