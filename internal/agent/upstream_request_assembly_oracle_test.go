package agent_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	catalogmodel "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

type upstreamRequestAssemblyScenario struct {
	Name            string                                 `json:"name"`
	SessionID       string                                 `json:"sessionId"`
	SystemPrompt    string                                 `json:"systemPrompt"`
	ThinkingLevel   string                                 `json:"thinkingLevel"`
	ThinkingBudgets upstreamRequestAssemblyThinkingBudgets `json:"thinkingBudgets"`
	Image           upstreamRequestAssemblyImage           `json:"image"`
	Skill           upstreamRequestAssemblySkill           `json:"skill"`
	Template        upstreamRequestAssemblyTemplate        `json:"template"`
	Tool            upstreamRequestAssemblyTool            `json:"tool"`
	Responses       []upstreamRequestAssemblyResponse      `json:"responses"`
}

type upstreamRequestAssemblyThinkingBudgets struct {
	Minimal uint64 `json:"minimal"`
	Low     uint64 `json:"low"`
	Medium  uint64 `json:"medium"`
	High    uint64 `json:"high"`
}

type upstreamRequestAssemblyImage struct {
	MIMEType string `json:"mimeType"`
	Base64   string `json:"base64"`
}

type upstreamRequestAssemblySkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Argument    string `json:"argument"`
}

type upstreamRequestAssemblyTemplate struct {
	Name     string `json:"name"`
	Content  string `json:"content"`
	Argument string `json:"argument"`
}

type upstreamRequestAssemblyTool struct {
	Name        string `json:"name"`
	CallID      string `json:"callId"`
	Description string `json:"description"`
	Argument    string `json:"argument"`
	ResultText  string `json:"resultText"`
}

type upstreamRequestAssemblyResponse struct {
	Text         string `json:"text"`
	ToolCall     bool   `json:"toolCall"`
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
}

type upstreamRequestAssemblyProvider struct {
	inner         *provider.ScriptedProvider
	responseModel provider.Model
	mu            sync.Mutex
	requests      []provider.Request
}

func (p *upstreamRequestAssemblyProvider) Stream(ctx context.Context, request provider.Request) provider.EventStream {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	var toolChoice *provider.ToolChoice
	if value, ok := request.ToolChoice(); ok {
		toolChoice = &value
	}
	rebound, err := provider.NewRequestWithOptions(
		p.responseModel, request.SystemPrompt(), request.Messages(), provider.RequestOptions{
			Tools: request.Tools(), AllowParallelToolCalls: request.ParallelToolCalls(), ToolChoice: toolChoice,
			ThinkingLevel: request.ThinkingLevel(), Metadata: request.Metadata(), Stream: request.StreamOptions(),
		},
	)
	if err != nil {
		panic(fmt.Sprintf("rebind request assembly response model: %v", err))
	}
	return p.inner.Stream(ctx, rebound)
}

func (p *upstreamRequestAssemblyProvider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.requests...)
}

type upstreamRequestAssemblyExecutor struct {
	readName, imageName string
	image               llm.ImageBlock
	resultText          string
	mu                  sync.Mutex
	runs                []any
}

func (*upstreamRequestAssemblyExecutor) Name() string { return "request-assembly-catalog" }

func (e *upstreamRequestAssemblyExecutor) Supports(name string) bool {
	return name == e.readName || name == e.imageName
}

func (e *upstreamRequestAssemblyExecutor) Execute(ctx context.Context, toolCallID string, arguments []byte, onUpdate func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return e.ExecuteNamed(ctx, toolCallID, e.readName, arguments, onUpdate)
}

func (e *upstreamRequestAssemblyExecutor) ExecuteNamed(_ context.Context, toolCallID, name string, arguments []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	if name == e.readName {
		return agent.ToolOutput{Content: []llm.ToolResultContentBlock{mustTextBlockForRequestAssembly("unused read")}}, nil
	}
	decoded, err := decodeWorkflowJSON(arguments)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	e.mu.Lock()
	e.runs = append(e.runs, map[string]any{"toolCallId": toolCallID, "arguments": decoded})
	e.mu.Unlock()
	return agent.ToolOutput{
		Content: []llm.ToolResultContentBlock{
			mustTextBlockForRequestAssembly(e.resultText), e.image, e.image,
		},
		Details: map[string]any{"label": decoded.(map[string]any)["label"]},
	}, nil
}

func (e *upstreamRequestAssemblyExecutor) Runs() []any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]any(nil), e.runs...)
}

func mustTextBlockForRequestAssembly(value string) llm.TextBlock {
	block, err := llm.NewTextBlock(value)
	if err != nil {
		panic(err)
	}
	return block
}

// TestUpstreamRequestAssemblyOracle pins the final request boundary shared by
// every surface: resource expansion remains durable, blockImages dynamically
// filters the complete provider context only, and thinking budgets accompany
// every request without being surface-specific state.
func TestUpstreamRequestAssemblyOracle(t *testing.T) {
	var corpus upstreamWorkflowCorpus
	if err := json.Unmarshal(upstreamWorkflowCorpusJSON, &corpus); err != nil {
		t.Fatalf("decode workflow corpus: %v", err)
	}
	expectedRoot, err := decodeWorkflowJSON(upstreamWorkflowOracleJSON)
	if err != nil {
		t.Fatalf("decode workflow oracle: %v", err)
	}
	expectedObject, ok := expectedRoot.(map[string]any)
	if !ok {
		t.Fatal("workflow oracle root is not an object")
	}
	expectedScenario, ok := expectedObject["requestAssemblyScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle requestAssemblyScenario is not an object")
	}
	scenario := corpus.RequestAssembly
	if len(scenario.Responses) != 3 || !scenario.Responses[0].ToolCall || scenario.ThinkingLevel != string(provider.ThinkingHigh) {
		t.Fatal("request assembly corpus no longer covers resources, tool images, and reasoning budgets")
	}

	root := t.TempDir()
	scenarioRoot := filepath.Join(root, "request-assembly")
	cwd := filepath.Join(scenarioRoot, "project")
	agentDir := filepath.Join(scenarioRoot, "agent")
	sessionDir := filepath.Join(scenarioRoot, "sessions")
	homeDir := filepath.Join(scenarioRoot, "home")
	explicitDir := filepath.Join(scenarioRoot, "explicit-resources")
	skillDir := filepath.Join(explicitDir, "skills", scenario.Skill.Name)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	promptPath := filepath.Join(explicitDir, "prompts", scenario.Template.Name+".md")
	for _, directory := range []string{cwd, agentDir, sessionDir, homeDir, skillDir, filepath.Dir(promptPath)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create request assembly directory: %v", err)
		}
	}
	skillSource := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", scenario.Skill.Name, scenario.Skill.Description, scenario.Skill.Body)
	if err := os.WriteFile(skillPath, []byte(skillSource), 0o600); err != nil {
		t.Fatalf("write request assembly skill: %v", err)
	}
	if err := os.WriteFile(promptPath, []byte(scenario.Template.Content), 0o600); err != nil {
		t.Fatalf("write request assembly template: %v", err)
	}
	settingsData, err := json.Marshal(map[string]any{
		"compaction":      map[string]any{"enabled": false},
		"retry":           map[string]any{"enabled": false},
		"transport":       "sse",
		"images":          map[string]any{"autoResize": false, "blockImages": true},
		"thinkingBudgets": scenario.ThinkingBudgets,
		"skills":          []string{skillPath},
		"prompts":         []string{promptPath},
	})
	if err != nil {
		t.Fatalf("encode request assembly settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settingsData, 0o600); err != nil {
		t.Fatalf("write request assembly settings: %v", err)
	}
	modelRuntime, err := catalogmodel.NewRuntime(catalogmodel.Options{AgentDir: agentDir, WorkingDir: cwd})
	if err != nil {
		t.Fatalf("create request assembly model runtime: %v", err)
	}
	settings := modelRuntime.Snapshot().Settings

	readDefinition, err := provider.NewToolDefinition(
		"read", "Read deterministic resources", false,
		[]byte(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}},"additionalProperties":false}`),
	)
	if err != nil {
		t.Fatalf("construct request assembly read tool: %v", err)
	}
	imageDefinition, err := provider.NewToolDefinition(
		scenario.Tool.Name, scenario.Tool.Description, false,
		[]byte(`{"type":"object","required":["label"],"properties":{"label":{"type":"string"}},"additionalProperties":false}`),
	)
	if err != nil {
		t.Fatalf("construct request assembly image tool: %v", err)
	}
	definitions := []provider.ToolDefinition{readDefinition, imageDefinition}
	resources, err := resource.New(resource.Config{
		CWD: cwd, AgentDir: agentDir, HomeDir: homeDir, NoContextFiles: true,
		SystemPromptSource: scenario.SystemPrompt,
		SkillPaths:         append([]string(nil), settings.Skills...),
		PromptPaths:        append([]string(nil), settings.Prompts...),
		Tools: []resource.Tool{
			{Name: "read"}, {Name: scenario.Tool.Name},
		},
		SelectedTools: []string{"read", scenario.Tool.Name},
	})
	if err != nil {
		t.Fatalf("construct request assembly resources: %v", err)
	}
	if err := resources.Reload(context.Background()); err != nil {
		t.Fatalf("load request assembly resources: %v", err)
	}
	resourceSnapshot, err := resources.Snapshot()
	if err != nil {
		t.Fatalf("snapshot request assembly resources: %v", err)
	}

	selected := catalogmodel.Model{
		Provider: "faux", API: "anthropic-messages", ID: "faux-reasoning", Name: "Faux Reasoning Model",
		BaseURL: "http://localhost:0", Reasoning: true,
		Input: []provider.InputKind{provider.InputText, provider.InputImage}, Cost: provider.CostRates{},
		ContextWindow: 128_000, MaxTokens: 16_384,
	}
	responseModel, err := provider.NewModel(provider.ModelSpec{
		Provider: "faux", API: "anthropic-messages", ID: "faux-1", Name: "Faux Model",
		BaseURL: "http://localhost:0", Reasoning: true,
		Input: []provider.InputKind{provider.InputText, provider.InputImage}, Cost: provider.CostRates{},
		ContextWindow: 128_000, MaxTokens: 16_384,
	})
	if err != nil {
		t.Fatalf("construct request assembly response model: %v", err)
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{ChunkRunes: 3, Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatalf("construct request assembly provider: %v", err)
	}
	toolArguments, err := json.Marshal(map[string]string{"label": scenario.Tool.Argument})
	if err != nil {
		t.Fatalf("encode request assembly tool arguments: %v", err)
	}
	toolCall, err := llm.NewToolCallBlock(scenario.Tool.CallID, scenario.Tool.Name, toolArguments)
	if err != nil {
		t.Fatalf("construct request assembly tool call: %v", err)
	}
	steps := make([]provider.ScriptStep, 0, len(scenario.Responses))
	for index, response := range scenario.Responses {
		var terminal llm.AssistantTerminal
		if response.ToolCall {
			terminal, err = newAssistantToolUseMessage(
				[]llm.AssistantBlock{mustTextBlock(t, response.Text), toolCall},
				mustUsage(t, response.InputTokens, response.OutputTokens), agentTestEpoch,
			)
		} else {
			terminal, err = newAssistantTextMessage(
				[]llm.TextBlock{mustTextBlock(t, response.Text)}, llm.FinishStop,
				mustUsage(t, response.InputTokens, response.OutputTokens), agentTestEpoch,
			)
		}
		if err != nil {
			t.Fatalf("construct request assembly response %d: %v", index, err)
		}
		step, stepErr := provider.FixedResponseStep(terminal)
		if stepErr != nil {
			t.Fatalf("construct request assembly response step %d: %v", index, stepErr)
		}
		steps = append(steps, step)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatalf("set request assembly responses: %v", err)
	}
	requestProvider := &upstreamRequestAssemblyProvider{inner: implementation, responseModel: responseModel}
	imageData, err := base64.StdEncoding.DecodeString(scenario.Image.Base64)
	if err != nil {
		t.Fatalf("decode request assembly image: %v", err)
	}
	imageBlock, err := llm.NewImageDataBlock(scenario.Image.MIMEType, imageData)
	if err != nil {
		t.Fatalf("construct request assembly image: %v", err)
	}
	executor := &upstreamRequestAssemblyExecutor{
		readName: "read", imageName: scenario.Tool.Name, image: imageBlock, resultText: scenario.Tool.ResultText,
	}

	var clockTick atomic.Int64
	var entrySequence atomic.Uint64
	manager, err := session.CreateSessionManagerWithOptions(cwd, sessionDir, session.ManagerOptions{
		NewSession: session.NewSessionOptions{ID: scenario.SessionID},
		Now:        func() time.Time { return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond) },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("go-request-assembly-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create request assembly session manager: %v", err)
	}
	managerOwned := true
	defer func() {
		if managerOwned {
			_ = manager.Close()
		}
	}()
	high := provider.ThinkingHigh
	services := &agentruntime.Services{
		CWD: cwd, AgentDir: agentDir, ModelRuntime: modelRuntime, ResourceService: resources,
		ResolveResourcePaths: func() ([]string, []string) {
			current := modelRuntime.Snapshot().Settings
			return append([]string(nil), current.Skills...), append([]string(nil), current.Prompts...)
		},
		Provider: requestProvider, Tool: executor, Tools: definitions,
	}
	created, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
		Services: services, Provider: requestProvider, SessionManager: manager,
		AllModels: []catalogmodel.Model{selected},
		Availability: catalogmodel.Availability{
			HasConfiguredAuth: func(string) bool { return true },
			SupportsRoute:     func(catalogmodel.Model) bool { return true },
		},
		ExplicitModel: &selected, ExplicitThinkingLevel: &high, Settings: settings,
		BaseConfig: agent.SessionConfig{
			Tool: executor, Tools: definitions, AllTools: definitions,
			ActiveToolNames: []string{"read", scenario.Tool.Name},
			Stream:          provider.StreamOptions{SessionID: scenario.SessionID, Transport: provider.TransportSSE},
			Now:             func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create request assembly AgentSession: %v", err)
	}
	runtime := created.Session
	runtimeOwned := true
	defer func() {
		if runtimeOwned {
			_ = runtime.Close(context.Background())
		}
	}()

	var eventMu sync.Mutex
	var observed []agent.SessionEvent
	unsubscribe := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		eventMu.Lock()
		observed = append(observed, event)
		eventMu.Unlock()
	})
	initialBlockImages := modelRuntime.Snapshot().Settings.Images.BlockImagesOrDefault()
	firstText := mustTextBlock(t, "/skill:"+scenario.Skill.Name+" "+scenario.Skill.Argument)
	first, err := runtime.PromptContent(context.Background(), []llm.UserContentBlock{firstText, imageBlock, imageBlock})
	if err != nil || !first.Succeeded() {
		t.Fatalf("first request assembly prompt = (%#v, %v)", first, err)
	}
	if err := modelRuntime.SetGlobalSettings(context.Background(), func(settings *catalogmodel.Settings) error {
		blocked := false
		settings.Images.BlockImages = &blocked
		return nil
	}); err != nil {
		t.Fatalf("unblock request assembly images: %v", err)
	}
	finalBlockImages := modelRuntime.Snapshot().Settings.Images.BlockImagesOrDefault()
	secondText := mustTextBlock(t, "/"+scenario.Template.Name+" "+scenario.Template.Argument)
	second, err := runtime.PromptContent(context.Background(), []llm.UserContentBlock{secondText, imageBlock})
	if err != nil || !second.Succeeded() {
		t.Fatalf("second request assembly prompt = (%#v, %v)", second, err)
	}
	if implementation.CallCount() != 3 || implementation.PendingResponses() != 0 || len(requestProvider.Requests()) != 3 {
		t.Fatalf("request assembly provider calls/pending = %d/%d/%d, want 3/0/3", implementation.CallCount(), implementation.PendingResponses(), len(requestProvider.Requests()))
	}

	unsubscribe()
	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	eventMu.Unlock()
	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("request assembly manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("request assembly stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputsWithThinkingBudgets(requestProvider.Requests(), scenarioRoot, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize request assembly provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize request assembly events: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("request assembly session lost selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize request assembly final messages: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize request assembly entries: %v", err)
	}
	loadedSkills := make([]any, len(resourceSnapshot.Skills))
	for index, skill := range resourceSnapshot.Skills {
		loadedSkills[index] = map[string]any{
			"name": skill.Name, "description": skill.Description, "filePath": skill.Path,
			"baseDir": skill.BaseDir, "disableModelInvocation": skill.DisableModelInvocation,
		}
	}
	loadedTemplates := make([]any, len(resourceSnapshot.Templates))
	for index, template := range resourceSnapshot.Templates {
		var argumentHint any
		if template.ArgumentHint != "" {
			argumentHint = template.ArgumentHint
		}
		loadedTemplates[index] = map[string]any{
			"name": template.Name, "description": template.Description, "argumentHint": argumentHint,
			"content": template.Content, "filePath": template.Path,
		}
	}
	finalIsStreaming := runtime.Activity().IsStreaming
	finalPendingMessageCount := runtime.PendingMessageCount()
	finalThinkingLevel := runtime.ThinkingLevel()
	finalActiveTools := runtime.ActiveToolNames()
	finalSystemPrompt := runtime.SystemPrompt()
	toolRuns := executor.Runs()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close request assembly runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, scenarioRoot, cwd)
	if err != nil {
		t.Fatalf("normalize request assembly JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-request-assembly-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen request assembly session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened request assembly entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened request assembly messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened request assembly selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"initialBlockImages": initialBlockImages, "finalBlockImages": finalBlockImages,
			"loadedSkills": loadedSkills, "loadedTemplates": loadedTemplates, "toolRuns": toolRuns,
		},
		"providerInputs": providerInputs,
		"events":         normalizedEvents,
		"finalState": map[string]any{
			"isStreaming": finalIsStreaming, "pendingMessageCount": finalPendingMessageCount,
			"model":         map[string]any{"provider": selectedRef.Provider(), "api": selectedRef.API(), "id": selectedRef.ID()},
			"thinkingLevel": string(finalThinkingLevel), "activeTools": finalActiveTools,
			"systemPrompt": finalSystemPrompt, "messages": finalMessages, "stats": normalizeWorkflowStats(stats),
		},
		"session": map[string]any{
			"header": normalizeWorkflowHeader(header, scenarioRoot, cwd), "entries": normalizedEntries, "fileEntries": fileEntries,
			"reopened": map[string]any{
				"header": normalizeWorkflowHeader(reopened.Header(), scenarioRoot, cwd), "entries": reopenedEntries,
				"context": map[string]any{
					"messages":      reopenedMessages,
					"model":         map[string]any{"provider": reopenedModel.Provider, "modelId": reopenedModel.ModelID},
					"thinkingLevel": reopenedThinking,
				},
			},
		},
	}
	canonicalActual := normalizeRequestAssemblyPaths(canonicalWorkflowJSON(t, actualScenario), scenarioRoot, cwd)
	if difference := workflowJSONDifference("requestAssemblyScenario", expectedScenario, canonicalActual); difference != "" {
		t.Fatalf("Go request assembly workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"requestAssemblyScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		normalizeRequestAssemblyPaths(fileHeader, scenarioRoot, cwd),
	); difference != "" {
		t.Fatalf("physical request assembly header differs from pinned TypeScript oracle: %s", difference)
	}
}

func normalizeRequestAssemblyPaths(value any, root, cwd string) any {
	switch typed := value.(type) {
	case string:
		return normalizeWorkflowPath(typed, root, cwd)
	case []any:
		for index := range typed {
			typed[index] = normalizeRequestAssemblyPaths(typed[index], root, cwd)
		}
	case map[string]any:
		for key := range typed {
			typed[key] = normalizeRequestAssemblyPaths(typed[key], root, cwd)
		}
	}
	return value
}
