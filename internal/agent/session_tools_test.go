package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
)

type sessionEnvironmentRunner struct{ requests []tool.RunRequest }

func (r *sessionEnvironmentRunner) Run(_ context.Context, request tool.RunRequest, _ tool.OutputSink) (tool.ExitStatus, error) {
	r.requests = append(r.requests, request)
	return tool.NewExitStatus(0)
}

func TestSessionToolEnvironmentFollowsLiveSelectionAndToolReload(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	runner := &sessionEnvironmentRunner{}
	newExecutor := func() *BashExecutor {
		bash, err := tool.NewBash(tool.BashOptions{
			WorkingDir: cwd, Runner: runner,
			Environment: []string{"PI_MODEL=stale", "PI_SESSION_ID=stale", "PI_SESSION_FILE=stale", "KEEP=value"},
		})
		if err != nil {
			t.Fatal(err)
		}
		executor, err := NewBashExecutor(bash)
		if err != nil {
			t.Fatal(err)
		}
		return executor
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("bash", "Execute a shell command", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	executor := newExecutor()
	coordinator, err := NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: internalControlModel(t, "first"), ThinkingLevel: provider.ThinkingHigh,
		Tool: executor, Tools: []provider.ToolDefinition{definition}, StandaloneBash: executor,
		ReloadTools: func(context.Context) (ToolRuntime, error) {
			next := newExecutor()
			return ToolRuntime{Executor: next, Tools: []provider.ToolDefinition{definition}, StandaloneBash: next}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(ctx) })
	readEnvironment := func() map[string]string {
		environment := make(map[string]string)
		for _, entry := range runner.requests[len(runner.requests)-1].Environment() {
			key, value, _ := strings.Cut(entry, "=")
			environment[key] = value
		}
		return environment
	}
	invoke := func(ctx context.Context) {
		t.Helper()
		_, selected := coordinator.loop.runtimeSnapshot()
		if _, err := executeNamedToolSafely(selected, ctx, "call", "bash", []byte(`{"command":"inspect"}`), nil); err != nil {
			t.Fatal(err)
		}
	}
	assertSelection := func(model, thinking string) {
		t.Helper()
		invoke(ctx)
		want := map[string]string{"KEEP": "value", "PI_SESSION_ID": manager.SessionID(), "PI_PROVIDER": "scripted", "PI_MODEL": model, "PI_REASONING_LEVEL": thinking}
		if got := readEnvironment(); !reflect.DeepEqual(got, want) {
			t.Fatalf("environment = %v, want %v", got, want)
		}
	}
	assertSelection("first", "high")
	if err := coordinator.SetModel(internalControlModel(t, "second")); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.SetThinkingLevel(provider.ThinkingLow); err != nil {
		t.Fatal(err)
	}
	assertSelection("second", "low")
	if err := coordinator.SetActiveToolsByName([]string{}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.SetActiveToolsByName([]string{"bash"}); err != nil {
		t.Fatal(err)
	}
	assertSelection("second", "low")
	if err := coordinator.Reload(ctx, ReloadOptions{}); err != nil {
		t.Fatal(err)
	}
	assertSelection("second", "low")

	// Per-call overrides retain the same precedence as the upstream spawn hook.
	overrideDir := t.TempDir()
	invoke(tool.WithBashExecutionContext(ctx, tool.BashExecutionContext{WorkingDir: overrideDir, Environment: map[string]string{"EXTRA": "present"}}))
	if readEnvironment()["EXTRA"] != "present" || runner.requests[len(runner.requests)-1].WorkingDir() != overrideDir {
		t.Fatal("session metadata replaced caller execution options")
	}
	if _, err := coordinator.ExecuteBash(ctx, "standalone", nil, ExecuteBashOptions{ExcludeFromContext: true}); err != nil {
		t.Fatal(err)
	}
	for key := range readEnvironment() {
		if strings.HasPrefix(key, "PI_") {
			t.Fatalf("standalone command inherited session metadata: %s", key)
		}
	}
}
