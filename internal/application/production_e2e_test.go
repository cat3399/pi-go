package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// TestProductionApplicationToolPersistenceAndReopenEndToEnd deliberately keeps
// only the remote model deterministic. Everything on the product side is the
// production path: Application -> Runtime -> AgentSession -> Agent ->
// AgentLoop -> provider adapter/default tool registry -> JSONL -> reopen.
func TestProductionApplicationToolPersistenceAndReopenEndToEnd(t *testing.T) {
	const (
		modelID       = "deepseek-application-e2e"
		toolFile      = "application-e2e-tool.txt"
		toolContent   = "APPLICATION_E2E_TOOL_OK"
		firstAnswer   = "APPLICATION_E2E_FIRST_OK"
		resumedAnswer = "APPLICATION_E2E_RESUMED_OK"
	)

	workingDir, agentDir := t.TempDir(), t.TempDir()
	capture := &applicationE2EProviderCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload := make(map[string]any)
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		turn := capture.append(applicationE2ERequest{
			path: request.URL.Path, authorization: request.Header.Get("Authorization"), payload: payload,
		})
		writer.Header().Set("Content-Type", "text/event-stream")
		switch turn {
		case 1:
			arguments, err := json.Marshal(map[string]string{"path": toolFile, "content": toolContent})
			if err != nil {
				t.Errorf("encode tool arguments: %v", err)
				return
			}
			applicationE2EWriteSSE(t, writer,
				map[string]any{"id": "chat-tool", "model": modelID, "choices": []any{map[string]any{
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": "call-write", "type": "function",
						"function": map[string]any{"name": "write", "arguments": string(arguments)},
					}}}, "finish_reason": nil,
				}}},
				map[string]any{"id": "chat-tool", "choices": []any{map[string]any{
					"delta": map[string]any{}, "finish_reason": "tool_calls",
				}}},
				map[string]any{"id": "chat-tool", "choices": []any{}, "usage": map[string]any{
					"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14,
				}},
			)
		case 2:
			applicationE2EWriteTextSSE(t, writer, "chat-first", modelID, firstAnswer)
		case 3:
			applicationE2EWriteTextSSE(t, writer, "chat-resumed", modelID, resumedAnswer)
		default:
			http.Error(writer, fmt.Sprintf("unexpected provider turn %d", turn), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)

	models, err := json.Marshal(map[string]any{"providers": map[string]any{
		"deepseek": map[string]any{
			"api": "openai-completions", "baseUrl": server.URL + "/v1", "apiKey": "application-e2e-key",
			"models": []any{map[string]any{
				"id": modelID, "name": "DeepSeek Application E2E", "reasoning": false,
				"contextWindow": 32_000, "maxTokens": 1_024,
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0o600); err != nil {
		t.Fatal(err)
	}

	config := app.ProductionConfig{
		WorkingDir: workingDir, AgentDir: agentDir, Environment: []string{}, OpenAIHTTPClient: server.Client(),
	}
	firstService := applicationE2ENewService(t, config)
	thinking := provider.ThinkingOff
	state, err := firstService.NewSession(context.Background(), application.NewSessionOptions{
		CWD: workingDir, Provider: "deepseek", ModelID: modelID, ThinkingLevel: &thinking,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionFile == nil {
		t.Fatal("production Application session is not durable")
	}
	sessionID, sessionPath := state.SessionID, *state.SessionFile
	firstEvents, err := firstService.SubscribeEvents(firstService.CurrentRevision())
	if err != nil {
		t.Fatal(err)
	}
	applicationE2EDispatchAndWait(t, firstService, firstEvents, sessionID,
		"Use the write tool exactly once to create the requested marker, then report completion.")
	firstEvents.Close()

	written, err := os.ReadFile(filepath.Join(workingDir, toolFile))
	if err != nil || string(written) != toolContent {
		t.Fatalf("production write side effect = %q, %v", written, err)
	}
	firstState, live, err := firstService.LiveState(sessionID)
	if err != nil || !live || firstState.IsPromptRunning || firstState.IsStreaming || firstState.MessageCount != 4 {
		t.Fatalf("settled first Application state = %#v, live=%t, err=%v", firstState, live, err)
	}
	applicationE2ECloseService(t, firstService)

	secondService := applicationE2ENewService(t, config)
	secondEvents, err := secondService.SubscribeEvents(secondService.CurrentRevision())
	if err != nil {
		t.Fatal(err)
	}
	applicationE2EDispatchAndWait(t, secondService, secondEvents, sessionID,
		"Continue from the reopened conversation without a tool and confirm the prior work.")
	secondEvents.Close()
	secondState, live, err := secondService.LiveState(sessionID)
	if err != nil || !live || secondState.IsPromptRunning || secondState.IsStreaming || secondState.MessageCount != 6 ||
		secondState.SessionFile == nil || *secondState.SessionFile != sessionPath {
		t.Fatalf("settled reopened Application state = %#v, live=%t, err=%v", secondState, live, err)
	}
	applicationE2ECloseService(t, secondService)

	requests := capture.snapshot()
	if len(requests) != 3 {
		t.Fatalf("provider request count = %d, want tool turn, completion, and reopened continuation", len(requests))
	}
	for index, request := range requests {
		if request.path != "/v1/chat/completions" || request.authorization != "Bearer application-e2e-key" || request.payload["model"] != modelID {
			t.Fatalf("provider request %d route/auth/model = %#v", index+1, request)
		}
		tools, ok := request.payload["tools"].([]any)
		if !ok || !reflect.DeepEqual(applicationE2EToolNames(tools), []string{"read", "bash", "edit", "write"}) {
			t.Fatalf("provider request %d tools = %#v", index+1, request.payload["tools"])
		}
	}
	if roles := applicationE2ERequestRoles(requests[1].payload); !reflect.DeepEqual(roles, []string{"system", "user", "assistant", "tool"}) {
		t.Fatalf("post-tool provider roles = %v", roles)
	}
	if roles := applicationE2ERequestRoles(requests[2].payload); !reflect.DeepEqual(roles, []string{"system", "user", "assistant", "tool", "assistant", "user"}) {
		t.Fatalf("reopened provider roles = %v", roles)
	}
	encodedThird, err := json.Marshal(requests[2].payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{toolContent, firstAnswer} {
		if !strings.Contains(string(encodedThird), marker) {
			t.Fatalf("reopened provider context omitted %q", marker)
		}
	}

	reopened, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := reopened.BuildContext().Messages()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}
	gotRoles := make([]llm.Role, len(messages))
	for index, message := range messages {
		gotRoles[index] = message.Role()
	}
	if !reflect.DeepEqual(gotRoles, wantRoles) {
		t.Fatalf("durable reopened roles = %v", gotRoles)
	}
	toolUse, ok := messages[1].(llm.AssistantToolUseMessage)
	if !ok || len(toolUse.Blocks()) != 1 {
		t.Fatalf("durable tool-use message = %T %#v", messages[1], messages[1])
	}
	call, ok := toolUse.Blocks()[0].(llm.ToolCallBlock)
	result, resultOK := messages[2].(llm.ToolResultMessage)
	if !ok || call.Name() != "write" || !resultOK || result.IsError() || result.ToolName() != "write" || result.ToolCallID() != call.ID() {
		t.Fatalf("durable tool association = %#v / %#v", call, messages[2])
	}
	if got := applicationE2EAssistantText(messages[3]); got != firstAnswer {
		t.Fatalf("durable first answer = %q", got)
	}
	if got := applicationE2EAssistantText(messages[5]); got != resumedAnswer {
		t.Fatalf("durable resumed answer = %q", got)
	}
}

type applicationE2ERequest struct {
	path          string
	authorization string
	payload       map[string]any
}

type applicationE2EProviderCapture struct {
	mu       sync.Mutex
	requests []applicationE2ERequest
}

func (c *applicationE2EProviderCapture) append(request applicationE2ERequest) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	return len(c.requests)
}

func (c *applicationE2EProviderCapture) snapshot() []applicationE2ERequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]applicationE2ERequest(nil), c.requests...)
}

func applicationE2EWriteSSE(t *testing.T, writer http.ResponseWriter, events ...map[string]any) {
	t.Helper()
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Errorf("encode provider event: %v", err)
			return
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
			t.Errorf("write provider event: %v", err)
			return
		}
	}
	_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
}

func applicationE2EWriteTextSSE(t *testing.T, writer http.ResponseWriter, id, modelID, text string) {
	t.Helper()
	applicationE2EWriteSSE(t, writer,
		map[string]any{"id": id, "model": modelID, "choices": []any{map[string]any{
			"delta": map[string]any{"content": text}, "finish_reason": nil,
		}}},
		map[string]any{"id": id, "model": modelID, "choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "stop",
		}}},
		map[string]any{"id": id, "choices": []any{}, "usage": map[string]any{
			"prompt_tokens": 16, "completion_tokens": 4, "total_tokens": 20,
		}},
	)
}

func applicationE2ENewService(t *testing.T, config app.ProductionConfig) *application.Service {
	t.Helper()
	service, err := application.NewService(application.ServiceOptions{
		Context: context.Background(), Production: config, DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func applicationE2ECloseService(t *testing.T, service *application.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func applicationE2EDispatchAndWait(
	t *testing.T,
	service *application.Service,
	events *application.EventSubscription,
	sessionID string,
	prompt string,
) {
	t.Helper()
	result, err := service.Dispatch(context.Background(), sessionID, application.PromptCommand{Message: prompt})
	if err != nil {
		t.Fatal(err)
	}
	started, ok := result.(application.PromptStartedResult)
	if !ok || started.OperationID == 0 {
		t.Fatalf("prompt dispatch result = %T %#v", result, result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		select {
		case event, open := <-events.Events:
			if !open {
				t.Fatal("Application event subscription closed before prompt settlement")
			}
			operation, operationEvent := event.Value.(application.OperationEvent)
			if !operationEvent || operation.OperationID != started.OperationID {
				continue
			}
			if event.SessionID != sessionID || operation.Command != application.CommandPrompt ||
				operation.Status != application.OperationCompleted || operation.Error != "" {
				t.Fatalf("prompt operation closure = %#v in session %q", operation, event.SessionID)
			}
			return
		case <-ctx.Done():
			t.Fatal("timed out waiting for Application prompt settlement")
		}
	}
}

func applicationE2EToolNames(tools []any) []string {
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		item, _ := raw.(map[string]any)
		function, _ := item["function"].(map[string]any)
		name, _ := function["name"].(string)
		names = append(names, name)
	}
	return names
}

func applicationE2ERequestRoles(payload map[string]any) []string {
	messages, _ := payload["messages"].([]any)
	roles := make([]string, 0, len(messages))
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		roles = append(roles, role)
	}
	return roles
}

func applicationE2EAssistantText(message llm.ConversationMessage) string {
	var blocks []llm.AssistantBlock
	switch value := message.(type) {
	case llm.AssistantTextMessage:
		blocks = value.Blocks()
	case llm.AssistantRichMessage:
		blocks = value.Blocks()
	default:
		return ""
	}
	var text strings.Builder
	for _, block := range blocks {
		if value, ok := block.(llm.TextBlock); ok {
			text.WriteString(value.Text())
		}
	}
	return text.String()
}
