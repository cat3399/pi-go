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
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
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
		{
			name:          "anthropic_messages",
			api:           provider.AnthropicMessagesAPI,
			thinkingLevel: provider.ThinkingOff,
			maxTokens:     512,
			new: func(key string) (provider.Provider, error) {
				return provider.NewAnthropicProvider(provider.AnthropicConfig{
					BaseURL: deepSeekLiveBaseURL + "/anthropic",
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
			catalogModel := deepSeekLiveModel(test.api)
			cycleSource := catalogModel
			cycleSource.ID = "cycle-source-not-requested"
			cycleSource.Name = "Cycle source (not requested)"
			cycleSourceRef, err := cycleSource.Ref()
			if err != nil {
				t.Fatal(err)
			}
			catalogRef, err := catalogModel.Ref()
			if err != nil {
				t.Fatal(err)
			}
			routeValidator, ok := implementation.(provider.RouteValidator)
			if !ok {
				t.Fatalf("concrete provider %T does not implement RouteValidator", implementation)
			}
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
			modelRuntime, err := model.NewRuntime(model.Options{AgentDir: directory, WorkingDir: directory})
			if err != nil {
				_ = manager.Close()
				t.Fatal(err)
			}

			echo := &deepSeekLiveEchoTool{}
			factoryResult, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
				Services:       &agentruntime.Services{CWD: directory, AgentDir: directory, ModelRuntime: modelRuntime},
				Provider:       implementation,
				SessionManager: manager,
				AllModels:      []model.Model{cycleSource, catalogModel},
				Availability: model.Availability{
					HasConfiguredAuth: func(providerID string) bool { return providerID == "deepseek" && apiKey != "" },
					SupportsRoute: func(candidate model.Model) bool {
						ref, refErr := candidate.Ref()
						return refErr == nil && routeValidator.SupportsModel(ref)
					},
				},
				ExplicitModel:         &cycleSource,
				ExplicitThinkingLevel: &test.thinkingLevel,
				BaseConfig: agent.SessionConfig{
					Tool: echo, Tools: []provider.ToolDefinition{definition},
					Stream: provider.StreamOptions{MaxTokens: &test.maxTokens},
					Retry:  agent.RetryPolicy{MaxAttempts: 1},
					ResolveAvailableModels: func(context.Context) ([]provider.Model, error) {
						return []provider.Model{cycleSourceRef, catalogRef}, nil
					},
				},
			})
			if err != nil {
				_ = manager.Close()
				t.Fatal(err)
			}
			coordinator := factoryResult.Session
			t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
			if err := coordinator.SetSteeringMode(agent.QueueAll); err != nil {
				t.Fatalf("SetSteeringMode(all) error: %v", err)
			}
			if err := coordinator.SetFollowUpMode(agent.QueueOneAtATime); err != nil {
				t.Fatalf("SetFollowUpMode(one-at-a-time) error: %v", err)
			}
			if err := coordinator.SetAutoCompactionEnabled(false); err != nil {
				t.Fatalf("SetAutoCompactionEnabled(false) error: %v", err)
			}
			if err := coordinator.SetAutoRetryEnabled(false); err != nil {
				t.Fatalf("SetAutoRetryEnabled(false) error: %v", err)
			}
			if coordinator.SteeringMode() != agent.QueueAll || coordinator.FollowUpMode() != agent.QueueOneAtATime ||
				coordinator.AutoCompactionEnabled() || coordinator.AutoRetryEnabled() {
				t.Fatalf("pre-cycle controls = %s/%s compaction=%t retry=%t", coordinator.SteeringMode(), coordinator.FollowUpMode(), coordinator.AutoCompactionEnabled(), coordinator.AutoRetryEnabled())
			}
			cycled, err := coordinator.CycleModel(context.Background(), agent.CycleForward)
			if err != nil {
				t.Fatalf("CycleModel() error: %v", err)
			}
			if cycled == nil || cycled.IsScoped || cycled.Model.Provider() != catalogModel.Provider ||
				cycled.Model.ID() != catalogModel.ID || cycled.ThinkingLevel != test.thinkingLevel {
				t.Fatalf("CycleModel() = %#v", cycled)
			}
			selected, selectedOK := coordinator.SelectedModel()
			if !selectedOK || selected.Provider() != catalogModel.Provider || selected.ID() != catalogModel.ID || selected.API() != catalogModel.API {
				_ = coordinator.Close(context.Background())
				t.Fatalf("factory selected model = %s/%s api=%s present=%t", selected.Provider(), selected.ID(), selected.API(), selectedOK)
			}
			if coordinator.ThinkingLevel() != test.thinkingLevel || factoryResult.ModelFallbackMessage != nil {
				_ = coordinator.Close(context.Background())
				t.Fatalf("factory thinking/fallback = %q / %#v", coordinator.ThinkingLevel(), factoryResult.ModelFallbackMessage)
			}
			initialEntries := manager.Entries()
			if len(initialEntries) != 3 {
				_ = coordinator.Close(context.Background())
				t.Fatalf("factory/cycle entries = %d, want initial model/thinking + cycled model", len(initialEntries))
			}
			modelChange, modelChangeOK := initialEntries[0].Payload().(session.ModelChangePayload)
			thinkingChange, thinkingChangeOK := initialEntries[1].Payload().(session.ThinkingLevelChangePayload)
			cycledChange, cycledChangeOK := initialEntries[2].Payload().(session.ModelChangePayload)
			if !modelChangeOK || modelChange.Provider != cycleSource.Provider || modelChange.ModelID != cycleSource.ID ||
				!thinkingChangeOK || thinkingChange.ThinkingLevel != string(test.thinkingLevel) {
				_ = coordinator.Close(context.Background())
				t.Fatalf("factory initial metadata = %#v / %#v", initialEntries[0].Payload(), initialEntries[1].Payload())
			}
			if !cycledChangeOK || cycledChange.Provider != catalogModel.Provider || cycledChange.ModelID != catalogModel.ID {
				t.Fatalf("cycle metadata = %#v", initialEntries[2].Payload())
			}
			if err := coordinator.SetSteeringMode(agent.QueueOneAtATime); err != nil {
				t.Fatalf("SetSteeringMode(one-at-a-time) error: %v", err)
			}
			if err := coordinator.SetFollowUpMode(agent.QueueAll); err != nil {
				t.Fatalf("SetFollowUpMode(all) error: %v", err)
			}
			if err := coordinator.SetAutoCompactionEnabled(true); err != nil {
				t.Fatalf("SetAutoCompactionEnabled(true) error: %v", err)
			}
			if err := coordinator.SetAutoRetryEnabled(true); err != nil {
				t.Fatalf("SetAutoRetryEnabled(true) error: %v", err)
			}
			if coordinator.SteeringMode() != agent.QueueOneAtATime || coordinator.FollowUpMode() != agent.QueueAll ||
				!coordinator.AutoCompactionEnabled() || !coordinator.AutoRetryEnabled() {
				t.Fatalf("pre-run controls = %s/%s compaction=%t retry=%t", coordinator.SteeringMode(), coordinator.FollowUpMode(), coordinator.AutoCompactionEnabled(), coordinator.AutoRetryEnabled())
			}

			var events deepSeekLiveEvents
			unsubscribe := coordinator.Subscribe(events.observe)
			runContext, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			result, runErr := coordinator.Run(runContext,
				`Call pi_go_live_echo exactly once with {"value":"ping"}. After receiving its result, reply with exactly: PI_GO_LIVE_OK`,
			)
			cancel()
			unsubscribe()
			if runErr != nil {
				t.Fatalf("AgentSession.Run() error: %v", runErr)
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
			stats, err := coordinator.GetSessionStats()
			if err != nil {
				t.Fatalf("GetSessionStats() error after live run: %v", err)
			}
			if stats.SessionID != manager.SessionID() || stats.SessionFile == nil || *stats.SessionFile != path ||
				stats.UserMessages != 1 || stats.AssistantMessages != 2 || stats.ToolCalls != 1 ||
				stats.ToolResults != 1 || stats.TotalMessages != 4 || stats.Tokens.Total == 0 {
				t.Fatalf("live session stats = %#v", stats)
			}
			if stats.ContextUsage == nil || stats.ContextUsage.Tokens == nil || stats.ContextUsage.Percent == nil ||
				*stats.ContextUsage.Tokens == 0 || stats.ContextUsage.ContextWindow != catalogModel.ContextWindow {
				t.Fatalf("live context usage = %#v", stats.ContextUsage)
			}
			forkMessages := coordinator.GetUserMessagesForForking()
			if len(forkMessages) != 1 || forkMessages[0].Text !=
				`Call pi_go_live_echo exactly once with {"value":"ping"}. After receiving its result, reply with exactly: PI_GO_LIVE_OK` {
				t.Fatalf("live fork messages = %#v", forkMessages)
			}
			if text, ok := coordinator.GetLastAssistantText(); !ok || !strings.Contains(text, "PI_GO_LIVE_OK") {
				t.Fatalf("live last assistant text = %q, present=%t", text, ok)
			}

			closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			closeErr := coordinator.Close(closeContext)
			closeCancel()
			if closeErr != nil {
				t.Fatalf("AgentSession.Close() error: %v", closeErr)
			}

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

func deepSeekLiveModel(api string) model.Model {
	catalogModel := model.Model{
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
	switch api {
	case provider.OpenAIResponsesAPI:
		supportsDeveloperRole := false
		catalogModel.Compat.OpenAIResponses = &provider.OpenAIResponsesCompat{
			SupportsDeveloperRole: &supportsDeveloperRole,
		}
	case provider.OpenAICompletionsAPI:
		supportsStore := false
		supportsDeveloperRole := false
		requiresReasoningContent := true
		thinkingFormat := "deepseek"
		catalogModel.Compat.OpenAICompletions = &provider.OpenAICompletionsCompat{
			SupportsStore:         &supportsStore,
			SupportsDeveloperRole: &supportsDeveloperRole,
			RequiresReasoningContentOnAssistantMessages: &requiresReasoningContent,
			ThinkingFormat: &thinkingFormat,
		}
	case provider.AnthropicMessagesAPI:
		// DeepSeek's Anthropic-compatible endpoint exposes the Messages wire
		// format, but not Claude-specific prompt-cache or tool-reference
		// extensions. Keep the Agent loop identical while advertising only the
		// portable surface that this endpoint implements.
		portableOnly := false
		catalogModel.BaseURL = deepSeekLiveBaseURL + "/anthropic"
		catalogModel.Compat.AnthropicMessages = &provider.AnthropicMessagesCompat{
			SupportsLongCacheRetention:  &portableOnly,
			SupportsCacheControlOnTools: &portableOnly,
			SupportsToolReferences:      &portableOnly,
		}
	}
	return catalogModel
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
