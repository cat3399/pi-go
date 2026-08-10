package agent_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type reloadResourceFixture struct {
	mu      sync.Mutex
	version int
	fail    error
	order   *[]string
}

type reloadGenerationExecutor struct {
	mu    sync.Mutex
	label string
	calls int
}

func (e *reloadGenerationExecutor) Name() string         { return e.label }
func (e *reloadGenerationExecutor) Supports(string) bool { return true }
func (e *reloadGenerationExecutor) Execute(context.Context, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return e.execute()
}
func (e *reloadGenerationExecutor) ExecuteNamed(context.Context, string, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return e.execute()
}
func (e *reloadGenerationExecutor) execute() (agent.ToolOutput, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return agent.ToolOutput{Text: e.label}, nil
}
func (e *reloadGenerationExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (r *reloadResourceFixture) BuildSystemPrompt(names []string) (string, agent.BuildSystemPromptOptions, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prompt := fmt.Sprintf("resources:%d tools:%v", r.version, names)
	return prompt, agent.BuildSystemPromptOptions{SelectedTools: append([]string(nil), names...)}, nil
}

func (r *reloadResourceFixture) ExpandPromptInput(text string) (string, error) { return text, nil }

func (r *reloadResourceFixture) Reload(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order != nil {
		*r.order = append(*r.order, "resources")
	}
	if r.fail != nil {
		return r.fail
	}
	r.version++
	return nil
}

func TestAgentSessionReloadMatchesLifecycleSettingsResourceAndPromptOrder(t *testing.T) {
	definition, err := provider.NewToolDefinition("read", "read", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	first, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
		close(started)
		<-release
		return mustTextTerminal(t, "first"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.FixedResponseStep(mustTextTerminal(t, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
		t.Fatal(err)
	}
	var order []string
	resources := &reloadResourceFixture{order: &order}
	settingsMu := sync.Mutex{}
	settings := agent.RuntimeControlSettings{
		SteeringMode: agent.QueueOneAtATime, FollowUpMode: agent.QueueOneAtATime,
		AutoCompactionEnabled: true, AutoRetryEnabled: true,
		Retry: agent.RetryPolicy{MaxAttempts: 2},
	}
	shutdownFailure := errors.New("shutdown hook failure")
	startFailure := errors.New("start hook failure")
	var extensionErrors []agent.ExtensionErrorEvent
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Tool: sessionCatalogExecutor{}, Tools: []provider.ToolDefinition{definition}, AllTools: []provider.ToolDefinition{definition},
		ActiveToolNames: []string{"read"}, Resources: resources,
		ReloadRuntime: func(context.Context) error {
			order = append(order, "runtime")
			settingsMu.Lock()
			settings = agent.RuntimeControlSettings{
				SteeringMode: agent.QueueAll, FollowUpMode: agent.QueueAll,
				AutoCompactionEnabled: false, AutoRetryEnabled: false,
				Retry:                   agent.RetryPolicy{MaxAttempts: 4},
				CompactionReserveTokens: 100, CompactionReserveSet: true,
				CompactionKeepRecentTokens: 50, CompactionKeepRecentSet: true,
				BranchSummaryReserveTokens: 25, BranchSummaryReserveSet: true,
			}
			settingsMu.Unlock()
			return nil
		},
		ReloadTools: func(context.Context) (agent.ToolRuntime, error) {
			order = append(order, "tools")
			return agent.ToolRuntime{Executor: sessionCatalogExecutor{}, Tools: []provider.ToolDefinition{definition}}, nil
		},
		ResolveRuntimeSettings: func() agent.RuntimeControlSettings {
			settingsMu.Lock()
			defer settingsMu.Unlock()
			return settings
		},
		Hooks: agent.Hooks{
			SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
				if event.Reason != agent.ShutdownReload {
					t.Fatalf("shutdown reason = %q", event.Reason)
				}
				order = append(order, "shutdown")
				return shutdownFailure
			},
			SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
				if event.Reason == agent.SessionStartup {
					return nil
				}
				if event.Reason != agent.SessionReload {
					t.Fatalf("start reason = %q", event.Reason)
				}
				order = append(order, "start")
				return startFailure
			},
			ExtensionError: func(_ context.Context, event agent.ExtensionErrorEvent) {
				extensionErrors = append(extensionErrors, event)
			},
		},
		Retry: agent.RetryPolicy{MaxAttempts: 2},
		Now:   func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	order = nil
	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), "first")
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active prompt did not reach provider")
	}
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{BeforeSessionStart: func(context.Context) error {
		order = append(order, "before-start")
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"shutdown", "runtime", "resources", "tools", "before-start", "start"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("reload order = %v, want %v", order, want)
	}
	if runtime.SteeringMode() != agent.QueueAll || runtime.FollowUpMode() != agent.QueueAll ||
		runtime.AutoCompactionEnabled() || runtime.AutoRetryEnabled() {
		t.Fatalf("reloaded settings = %s/%s compaction=%t retry=%t", runtime.SteeringMode(), runtime.FollowUpMode(), runtime.AutoCompactionEnabled(), runtime.AutoRetryEnabled())
	}
	if !reflect.DeepEqual(runtime.ActiveToolNames(), []string{"read"}) || runtime.SystemPrompt() != "resources:1 tools:[read]" {
		t.Fatalf("reloaded prompt/tools = %q / %v", runtime.SystemPrompt(), runtime.ActiveToolNames())
	}
	if len(extensionErrors) != 2 || extensionErrors[0].Event != "session_shutdown" || !errors.Is(extensionErrors[0].Cause, shutdownFailure) ||
		extensionErrors[1].Event != "session_start" || !errors.Is(extensionErrors[1].Cause, startFailure) {
		t.Fatalf("reload hook errors = %#v", extensionErrors)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active run did not settle after reload")
	}
	if result, err := runtime.Run(context.Background(), "second"); err != nil || !result.Succeeded() {
		t.Fatalf("post-reload run = (%#v, %v)", result, err)
	}
	requests := implementation.Requests()
	if len(requests) != 2 || requests[0].SystemPrompt() != "resources:0 tools:[read]" || requests[1].SystemPrompt() != "resources:1 tools:[read]" {
		t.Fatalf("reload request prompts = %#v", requests)
	}
}

func TestAgentSessionReloadReplacesCompleteToolRuntimeAndPreservesActiveNames(t *testing.T) {
	read, err := provider.NewToolDefinition("read", "read old", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	reloadedRead, err := provider.NewToolDefinition("read", "read new", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	write, err := provider.NewToolDefinition("write", "write new", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	oldBashCalled := false
	oldBash := standaloneBashFunc(func(context.Context, string, func(string)) (agent.BashResult, error) {
		oldBashCalled = true
		return agent.BashResult{}, nil
	})
	newBashCalled := false
	newBash := standaloneBashFunc(func(_ context.Context, command string, _ func(string)) (agent.BashResult, error) {
		newBashCalled = true
		if command != "after reload" {
			t.Fatalf("reloaded standalone command = %q", command)
		}
		code := 0
		return agent.BashResult{ExitCode: &code}, nil
	})
	resources := &reloadResourceFixture{}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Tool: sessionCatalogExecutor{}, Tools: []provider.ToolDefinition{read}, AllTools: []provider.ToolDefinition{read},
		ActiveToolNames: []string{"read"}, Resources: resources, StandaloneBash: oldBash,
		ReloadTools: func(context.Context) (agent.ToolRuntime, error) {
			return agent.ToolRuntime{
				Executor: sessionCatalogExecutor{}, Tools: []provider.ToolDefinition{reloadedRead, write}, StandaloneBash: newBash,
			}, nil
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{}); err != nil {
		t.Fatal(err)
	}
	all := runtime.AllTools()
	if len(all) != 2 || all[0].Name() != "read" || all[0].Description() != "read new" || all[1].Name() != "write" {
		t.Fatalf("reloaded tool registry = %#v", all)
	}
	if active := runtime.ActiveToolNames(); !reflect.DeepEqual(active, []string{"read"}) {
		t.Fatalf("reloaded active tools = %v", active)
	}
	if runtime.SystemPrompt() != "resources:1 tools:[read]" {
		t.Fatalf("reloaded system prompt = %q", runtime.SystemPrompt())
	}
	if _, err := runtime.ExecuteBash(context.Background(), "after reload", nil, agent.ExecuteBashOptions{}); err != nil {
		t.Fatal(err)
	}
	if oldBashCalled || !newBashCalled {
		t.Fatalf("standalone bash generation old=%t new=%t", oldBashCalled, newBashCalled)
	}
}

func TestAgentSessionReloadRejectsInvalidToolGenerationWithoutReplacingLastHealthyRuntime(t *testing.T) {
	read, err := provider.NewToolDefinition("read", "healthy", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := provider.NewToolDefinition("read", "invalid duplicate", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	oldBashCalled := false
	oldBash := standaloneBashFunc(func(_ context.Context, _ string, _ func(string)) (agent.BashResult, error) {
		oldBashCalled = true
		code := 0
		return agent.BashResult{ExitCode: &code}, nil
	})
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Tool: sessionCatalogExecutor{}, Tools: []provider.ToolDefinition{read}, StandaloneBash: oldBash,
		ReloadTools: func(context.Context) (agent.ToolRuntime, error) {
			return agent.ToolRuntime{
				Executor: sessionCatalogExecutor{}, Tools: []provider.ToolDefinition{duplicate, duplicate},
			}, nil
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("invalid tool reload error = %v", err)
	}
	all := runtime.AllTools()
	if len(all) != 1 || all[0].Name() != "read" || all[0].Description() != "healthy" {
		t.Fatalf("last healthy tool registry = %#v", all)
	}
	if _, err := runtime.ExecuteBash(context.Background(), "still healthy", nil, agent.ExecuteBashOptions{}); err != nil {
		t.Fatal(err)
	}
	if !oldBashCalled {
		t.Fatal("invalid reload replaced or removed last healthy standalone bash")
	}
}

func TestAgentSessionReloadKeepsActiveTurnToolSnapshotAndAppliesNewRuntimeNextRun(t *testing.T) {
	definition, err := provider.NewToolDefinition("which", "which generation", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	first, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
		close(started)
		<-release
		return mustToolUseTerminal(t, "call-old", "which", []byte(`{}`)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := []provider.ScriptStep{first}
	for _, terminal := range []llm.AssistantTerminal{
		mustTextTerminal(t, "first done"),
		mustToolUseTerminal(t, "call-new", "which", []byte(`{}`)),
		mustTextTerminal(t, "second done"),
	} {
		step, err := provider.FixedResponseStep(terminal)
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, step)
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatal(err)
	}
	oldExecutor := &reloadGenerationExecutor{label: "old"}
	newExecutor := &reloadGenerationExecutor{label: "new"}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Tool: oldExecutor, Tools: []provider.ToolDefinition{definition},
		ReloadTools: func(context.Context) (agent.ToolRuntime, error) {
			return agent.ToolRuntime{Executor: newExecutor, Tools: []provider.ToolDefinition{definition}}, nil
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), "first")
		firstDone <- runErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first provider request did not start")
	}
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not settle")
	}
	if oldExecutor.callCount() != 1 || newExecutor.callCount() != 0 {
		t.Fatalf("active turn executor calls old=%d new=%d", oldExecutor.callCount(), newExecutor.callCount())
	}
	if result, err := runtime.Run(context.Background(), "second"); err != nil || !result.Succeeded() {
		t.Fatalf("second run = (%#v, %v)", result, err)
	}
	if oldExecutor.callCount() != 1 || newExecutor.callCount() != 1 {
		t.Fatalf("post-reload executor calls old=%d new=%d", oldExecutor.callCount(), newExecutor.callCount())
	}
}

func TestAgentSessionReloadStopsAfterFailedLastHealthyResourceRefresh(t *testing.T) {
	wantErr := errors.New("resource reload failed")
	var order []string
	resources := &reloadResourceFixture{fail: wantErr, order: &order}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Resources: resources,
		ReloadRuntime: func(context.Context) error {
			order = append(order, "runtime")
			return nil
		},
		Hooks: agent.Hooks{
			SessionShutdown: func(context.Context, agent.SessionShutdownHookEvent) error {
				order = append(order, "shutdown")
				return nil
			},
			SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
				if event.Reason == agent.SessionReload {
					order = append(order, "start")
				}
				return nil
			},
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	order = nil
	beforeCalled := false
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{BeforeSessionStart: func(context.Context) error {
		beforeCalled = true
		return nil
	}}); !errors.Is(err, wantErr) {
		t.Fatalf("Reload error = %v", err)
	}
	if !reflect.DeepEqual(order, []string{"shutdown", "runtime", "resources"}) || beforeCalled {
		t.Fatalf("failed reload order = %v before=%t", order, beforeCalled)
	}
	if runtime.SystemPrompt() != "resources:0 tools:[]" {
		t.Fatalf("failed reload changed prompt = %q", runtime.SystemPrompt())
	}
}

func TestAgentSessionReloadRejectsOverlapAndLeavesLifecycleHooksReentrant(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var runtime *agent.AgentSession
	created, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ReloadRuntime: func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		},
		Hooks: agent.Hooks{
			SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
				if event.Reason != agent.ShutdownReload {
					return nil
				}
				return runtime.SetSteeringMode(agent.QueueAll)
			},
			SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
				if event.Reason != agent.SessionReload {
					return nil
				}
				return runtime.SetFollowUpMode(agent.QueueAll)
			},
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime = created
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	done := make(chan error, 1)
	go func() { done <- runtime.Reload(context.Background(), agent.ReloadOptions{}) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reload hook could not re-enter runtime controls")
	}
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{}); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("overlapping Reload error = %v, want ErrBusy", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first reload did not finish")
	}
	if runtime.SteeringMode() != agent.QueueAll || runtime.FollowUpMode() != agent.QueueAll {
		t.Fatalf("hook controls = %s/%s, want all/all", runtime.SteeringMode(), runtime.FollowUpMode())
	}
}
