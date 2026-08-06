package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

const (
	deepSeekLiveBaseURL = "https://api.deepseek.com"
	deepSeekLiveModelID = "deepseek-v4-flash"
)

// TestLiveDeepSeekV4FlashAgentSession is an opt-in production smoke test. It
// deliberately exercises the real AgentSession -> Agent -> AgentLoop path and
// the real remote model; deterministic providers are not valid substitutes.
func TestLiveDeepSeekV4FlashAgentSession(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY is not set")
	}

	tests := []struct {
		name          string
		api           string
		thinkingLevel provider.ThinkingLevel
		maxTokens     uint64
		new           func(string) (provider.Provider, error)
	}{
		{
			name:          "openai_responses",
			api:           provider.OpenAIResponsesAPI,
			thinkingLevel: provider.ThinkingOff,
			maxTokens:     128,
			new: func(key string) (provider.Provider, error) {
				return provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{
					BaseURL: deepSeekLiveBaseURL,
					APIKey:  key,
				})
			},
		},
		{
			name:          "openai_chat_completions",
			api:           provider.OpenAICompletionsAPI,
			thinkingLevel: provider.ThinkingHigh,
			maxTokens:     1024,
			new: func(key string) (provider.Provider, error) {
				return provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
					BaseURL: deepSeekLiveBaseURL,
					APIKey:  key,
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			implementation, err := test.new(apiKey)
			if err != nil {
				t.Fatal(err)
			}
			model := deepSeekLiveModel(t, test.api)
			definition, err := provider.NewToolDefinition(
				"pi_go_live_echo",
				"Return the supplied value unchanged. Use this tool when explicitly requested.",
				false,
				[]byte(`{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"}},"required":["value"]}`),
			)
			if err != nil {
				t.Fatal(err)
			}

			directory := t.TempDir()
			path := filepath.Join(directory, "live-session.jsonl")
			manager, err := session.OpenSessionManagerWithOptions(path, directory, directory, session.ManagerOptions{
				NewSession: session.NewSessionOptions{ID: "live-deepseek-" + strings.ReplaceAll(test.name, "_", "-")},
			})
			if err != nil {
				t.Fatal(err)
			}

			echo := &deepSeekLiveEchoTool{}
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider:       implementation,
				SessionManager: manager,
				Model:          model,
				ThinkingLevel:  test.thinkingLevel,
				Tool:           echo,
				Tools:          []provider.ToolDefinition{definition},
				Stream:         provider.StreamOptions{MaxTokens: &test.maxTokens},
				Retry:          agent.RetryPolicy{MaxAttempts: 1},
			})
			if err != nil {
				_ = manager.Close()
				t.Fatal(err)
			}

			var events deepSeekLiveEvents
			unsubscribe := coordinator.Subscribe(events.observe)
			runContext, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			result, runErr := coordinator.Run(runContext,
				`Call pi_go_live_echo exactly once with {"value":"ping"}. After receiving its result, reply with exactly: PI_GO_LIVE_OK`,
			)
			cancel()
			unsubscribe()

			closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			closeErr := coordinator.Close(closeContext)
			closeCancel()
			if runErr != nil {
				t.Fatalf("AgentSession.Run() error: %v", runErr)
			}
			if closeErr != nil {
				t.Fatalf("AgentSession.Close() error: %v", closeErr)
			}
			if !result.Succeeded() {
				terminal, _ := result.Terminal()
				t.Fatalf("live run failed: turns=%d tools=%d terminal=%T %#v", result.ProviderTurns(), result.ToolExecutions(), terminal, terminal)
			}
			if result.ProviderTurns() != 2 || result.ToolExecutions() != 1 || echo.calls.Load() != 1 {
				t.Fatalf("live loop = turns %d, tool executions %d, executor calls %d; want 2/1/1", result.ProviderTurns(), result.ToolExecutions(), echo.calls.Load())
			}
			terminal, ok := result.Terminal()
			if !ok || terminal.FinishReason() != llm.FinishStop || !strings.Contains(strings.TrimSpace(deepSeekLiveAssistantText(terminal)), "PI_GO_LIVE_OK") {
				t.Fatalf("live terminal text = %q (%T)", deepSeekLiveAssistantText(terminal), terminal)
			}
			events.assertClosed(t, result.ProviderTurns())

			reopened, err := session.OpenSessionManager(path, directory, "")
			if err != nil {
				t.Fatal(err)
			}
			messages := reopened.BuildContext().Messages()
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if len(messages) != 4 {
				t.Fatalf("reopened messages = %d, want user/tool-call/tool-result/final-assistant", len(messages))
			}
			wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleAssistant}
			for index, want := range wantRoles {
				if messages[index].Role() != want {
					t.Fatalf("reopened message %d role = %s, want %s", index, messages[index].Role(), want)
				}
			}
			last, ok := messages[len(messages)-1].(llm.AssistantTerminal)
			if !ok || last.FinishReason() != llm.FinishStop || !strings.Contains(strings.TrimSpace(deepSeekLiveAssistantText(last)), "PI_GO_LIVE_OK") {
				t.Fatalf("reopened terminal = %T %#v", messages[len(messages)-1], messages[len(messages)-1])
			}
		})
	}
}

func deepSeekLiveModel(t *testing.T, api string) provider.Model {
	t.Helper()
	modelSpec := provider.ModelSpec{
		Provider:      "deepseek",
		API:           api,
		ID:            deepSeekLiveModelID,
		Name:          "DeepSeek V4 Flash",
		BaseURL:       deepSeekLiveBaseURL,
		Reasoning:     true,
		Input:         []provider.InputKind{provider.InputText},
		ContextWindow: 1_000_000,
		MaxTokens:     384_000,
	}
	if api == provider.OpenAIResponsesAPI {
		supportsDeveloperRole := false
		modelSpec.Compat.OpenAIResponses = &provider.OpenAIResponsesCompat{
			SupportsDeveloperRole: &supportsDeveloperRole,
		}
	} else {
		supportsStore := false
		supportsDeveloperRole := false
		requiresReasoningContent := true
		thinkingFormat := "deepseek"
		modelSpec.Compat.OpenAICompletions = &provider.OpenAICompletionsCompat{
			SupportsStore:         &supportsStore,
			SupportsDeveloperRole: &supportsDeveloperRole,
			RequiresReasoningContentOnAssistantMessages: &requiresReasoningContent,
			ThinkingFormat: &thinkingFormat,
		}
	}
	model, err := provider.NewModel(modelSpec)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

type deepSeekLiveEchoTool struct{ calls atomic.Uint32 }

func (*deepSeekLiveEchoTool) Name() string { return "pi_go_live_echo" }

func (t *deepSeekLiveEchoTool) Execute(_ context.Context, arguments []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return agent.ToolOutput{}, fmt.Errorf("decode echo input: %w", err)
	}
	if input.Value != "ping" {
		return agent.ToolOutput{}, fmt.Errorf("unexpected echo value %q", input.Value)
	}
	t.calls.Add(1)
	return agent.ToolOutput{Text: input.Value}, nil
}

type deepSeekLiveEvents struct {
	mu                                         sync.Mutex
	agentStart, turnStart, turnEnd             int
	messageStart, messageUpdate, messageEnd    int
	toolStart, toolEnd, agentEnd, agentSettled int
	textDeltaBytes                             int
}

func (e *deepSeekLiveEvents) observe(_ context.Context, event agent.SessionEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch value := event.(type) {
	case agent.AgentStartEvent:
		e.agentStart++
	case agent.TurnStartEvent:
		e.turnStart++
	case agent.TurnEndEvent:
		e.turnEnd++
	case agent.MessageStartEvent:
		e.messageStart++
	case agent.MessageUpdateEvent:
		e.messageUpdate++
		if delta, ok := value.AssistantMessageEvent.Event().(llm.TextDeltaEvent); ok {
			e.textDeltaBytes += len(delta.Delta())
		}
	case agent.MessageEndEvent:
		e.messageEnd++
	case agent.ToolExecutionStartEvent:
		e.toolStart++
	case agent.ToolExecutionEndEvent:
		e.toolEnd++
	case agent.SessionAgentEndEvent:
		e.agentEnd++
	case agent.AgentSettledEvent:
		e.agentSettled++
	}
}

func (e *deepSeekLiveEvents) assertClosed(t *testing.T, turns uint32) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.agentStart != 1 || e.agentEnd != 1 || e.agentSettled != 1 {
		t.Fatalf("agent event closure = start/end/settled %d/%d/%d", e.agentStart, e.agentEnd, e.agentSettled)
	}
	if e.turnStart != int(turns) || e.turnEnd != int(turns) {
		t.Fatalf("turn event closure = start/end %d/%d, want %d/%d", e.turnStart, e.turnEnd, turns, turns)
	}
	if e.messageStart != e.messageEnd || e.messageEnd < int(turns) || e.messageUpdate == 0 || e.textDeltaBytes == 0 {
		t.Fatalf("message streaming = start/update/end/text-bytes %d/%d/%d/%d", e.messageStart, e.messageUpdate, e.messageEnd, e.textDeltaBytes)
	}
	if e.toolStart != 1 || e.toolEnd != 1 {
		t.Fatalf("tool event closure = start/end %d/%d", e.toolStart, e.toolEnd)
	}
}

func deepSeekLiveAssistantText(message llm.AssistantTerminal) string {
	var blocks []llm.AssistantBlock
	switch value := message.(type) {
	case llm.AssistantTextMessage:
		blocks = value.Blocks()
	case llm.AssistantRichMessage:
		blocks = value.Blocks()
	case llm.AssistantToolUseMessage:
		blocks = value.Blocks()
	case llm.AssistantFailureMessage:
		blocks = value.Blocks()
	}
	var text strings.Builder
	for _, block := range blocks {
		if value, ok := block.(llm.TextBlock); ok {
			text.WriteString(value.Text())
		}
	}
	return text.String()
}
