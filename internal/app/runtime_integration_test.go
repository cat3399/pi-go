package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
)

type rejectingHTTPDoer struct{ calls atomic.Uint32 }

func (d *rejectingHTTPDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, errors.New("unexpected provider request")
}

type dynamicAuthDoer struct {
	mu             sync.Mutex
	authorizations []string
	nextID         uint64
}

type dynamicProviderSettingsDoer struct {
	mu        sync.Mutex
	deadlines []time.Duration
	calls     int
}

type reloadToolSettingsDoer struct {
	mu    sync.Mutex
	calls int
}

func (d *reloadToolSettingsDoer) Do(request *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()
	var events []map[string]any
	if call%2 == 1 {
		itemID, callID := fmt.Sprintf("fc-%d", call), fmt.Sprintf("call-%d", call)
		arguments := `{"command":"model command"}`
		item := map[string]any{"type": "function_call", "id": itemID, "call_id": callID, "name": "bash", "arguments": arguments}
		events = []map[string]any{
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID, "name": "bash", "arguments": ""}},
			{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": itemID, "delta": arguments},
			{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": itemID, "arguments": arguments},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}},
		}
	} else {
		item := map[string]any{"type": "message", "id": fmt.Sprintf("msg-%d", call), "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}
		events = []map[string]any{
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}},
		}
	}
	var body strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&body, "data: %s\n\n", encoded)
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(body.String())), Request: request,
	}, nil
}

type reloadToolSettingsRunner struct {
	mu       sync.Mutex
	commands []string
}

func (r *reloadToolSettingsRunner) Run(_ context.Context, request tool.RunRequest, sink tool.OutputSink) (tool.ExitStatus, error) {
	r.mu.Lock()
	r.commands = append(r.commands, request.Command())
	r.mu.Unlock()
	if err := sink([]byte("ok")); err != nil {
		return tool.ExitStatus{}, err
	}
	return tool.NewExitStatus(0)
}

func (r *reloadToolSettingsRunner) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

func (d *dynamicProviderSettingsDoer) Do(request *http.Request) (*http.Response, error) {
	remaining := time.Duration(-1)
	if deadline, ok := request.Context().Deadline(); ok {
		remaining = time.Until(deadline)
	}
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.deadlines = append(d.deadlines, remaining)
	d.mu.Unlock()
	if call == 2 {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header: http.Header{
				"Content-Type":   []string{"application/json"},
				"Retry-After-Ms": []string{"0"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"error":{"message":"retry","code":"rate_limit_exceeded"}}`)),
			Request: request,
		}, nil
	}
	item := fmt.Sprintf(`{"type":"message","id":"settings-%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}`, call)
	body := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}, nil
}

func (d *dynamicProviderSettingsDoer) snapshot() []time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Duration(nil), d.deadlines...)
}

func (d *dynamicAuthDoer) Do(request *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.authorizations = append(d.authorizations, request.Header.Get("Authorization"))
	d.nextID++
	id := d.nextID
	d.mu.Unlock()
	item := fmt.Sprintf(`{"type":"message","id":"msg-%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}`, id)
	body := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" + item + "],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}, nil
}

func (d *dynamicAuthDoer) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.authorizations...)
}

func writeProductionCatalog(t *testing.T, agentDir string, withAuth bool) {
	t.Helper()
	configured := map[string]any{"baseUrl": "https://fixture.invalid/v1"}
	if withAuth {
		configured["apiKey"] = "fixture-key"
	}
	data, err := json.Marshal(map[string]any{"providers": map[string]any{"openai": configured}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixedProductionConfig(cwd, agentDir, docsDir string) ProductionConfig {
	return ProductionConfig{
		WorkingDir: cwd, AgentDir: agentDir, DocsDir: docsDir, Environment: []string{},
		SessionID: "runtime-integration", SessionNow: func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
		AgentNow: func() time.Time { return time.Date(2026, 8, 6, 12, 0, 1, 0, time.UTC) },
	}
}

func TestOpenProductionRuntimeUsesProductionRestoreWithoutRunningPrompt(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, true)
	sessionPath := filepath.Join(t.TempDir(), "rpc-session.jsonl")
	runtime, err := OpenProductionRuntime(context.Background(), fixedProductionConfig(cwd, agentDir, docsDir), ProductionRuntimeOptions{
		SessionPath: sessionPath, ProviderID: "openai", ModelID: "gpt-5.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := runtime.Dispose(context.Background()); err != nil {
			t.Errorf("dispose runtime: %v", err)
		}
	}()
	productSession := runtime.Session()
	selected, ok := productSession.SelectedModel()
	if !ok || selected.Provider() != "openai" || selected.ID() != "gpt-5.5" {
		t.Fatalf("selected model = %#v, %t", selected, ok)
	}
	if path, ok := productSession.SessionManager().SessionFile(); !ok || path != sessionPath {
		t.Fatalf("session file = %q, %t", path, ok)
	}
	messages := productSession.State().Active.Messages()
	if productSession.SessionManager().Cwd() != cwd || len(messages) != 0 {
		t.Fatalf("opened session = cwd %q messages %#v", productSession.SessionManager().Cwd(), messages)
	}
}

func TestProductionToolRuntimeOptionsUseLiveShellAndImageSettings(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	autoResize := false
	plan := productionRuntimePlan{config: ProductionConfig{}, environment: []string{"PATH=/fixture"}}
	options, err := plan.toolRuntimeOptions(t.TempDir(), model.Settings{
		ShellPath: "~/custom-shell", ShellCommandPrefix: "prepare-shell",
		Images: model.ImageSettings{AutoResize: &autoResize},
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Bash.ShellPath != filepath.Join(home, "custom-shell") || options.Bash.CommandPrefix != "prepare-shell" ||
		options.Filesystem.AutoResizeImages == nil || *options.Filesystem.AutoResizeImages {
		t.Fatalf("production tool options = %#v", options)
	}
	plan.config.BashShellPath = "/explicit/shell"
	overridden, err := plan.toolRuntimeOptions(t.TempDir(), model.Settings{ShellPath: "/settings/shell"})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.Bash.ShellPath != "/explicit/shell" {
		t.Fatalf("explicit shell override = %q", overridden.Bash.ShellPath)
	}
}

func TestProductionRuntimeRestoresSavedModelAndThinking(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, true)
	sessionPath := filepath.Join(t.TempDir(), "saved.jsonl")
	stored, err := session.Create(sessionPath, session.CreateOptions{ID: "saved", WorkingDir: cwd})
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserTextMessage("saved prompt", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stored.Append(context.Background(), user, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	answer, err := llm.NewTextBlock("saved answer")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := llm.NewUsage(llm.UsageSpec{})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{answer}, llm.FinishStop, usage, time.Now(),
		llm.AssistantProvenance{Provider: "openai", API: "openai-responses", Model: "gpt-5.5"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stored.Append(context.Background(), assistant, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := stored.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := session.OpenSessionManager(sessionPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendModelChange(context.Background(), "openai", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendThinkingLevelChange(context.Background(), string(provider.ThinkingHigh)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	runtimeDeps, err := assembleProductionRuntime(context.Background(), fixedProductionConfig(cwd, agentDir, docsDir), options{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err = session.OpenSessionManager(sessionPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := runtimeDeps.factory(context.Background(), agentruntime.CreateOptions{CWD: manager.Cwd(), AgentDir: agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := created.Session.Close(context.Background()); err != nil {
			t.Errorf("close restored session: %v", err)
		}
	}()
	selected, ok := created.Session.SelectedModel()
	if !ok || selected.Provider() != "openai" || selected.ID() != "gpt-5.5" {
		t.Fatalf("restored model = %#v, %t", selected, ok)
	}
	if got := created.Session.ThinkingLevel(); got != provider.ThinkingHigh {
		t.Fatalf("restored thinking = %q, want %q", got, provider.ThinkingHigh)
	}
}

func TestProductionRuntimeResolvesCurrentAuthForEveryTurn(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, true)
	doer := &dynamicAuthDoer{}
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	config.OpenAIHTTPClient = doer
	runtimeDeps, err := assembleProductionRuntime(context.Background(), config, options{modelID: "openai/gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), runtimeDeps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer productRuntime.Dispose(context.Background())
	if _, err := productRuntime.Session().Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := productRuntime.Services().AuthRuntime.SetAPIKey("openai", "replacement-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := productRuntime.Session().Run(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if got := doer.snapshot(); len(got) != 2 || got[0] != "Bearer fixture-key" || got[1] != "Bearer replacement-key" {
		t.Fatalf("authorization sequence = %#v", got)
	}
}

func TestProductionRuntimeRefreshesProviderSettingsForEveryTurn(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, true)
	doer := &dynamicProviderSettingsDoer{}
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	config.OpenAIHTTPClient = doer
	runtimeDeps, err := assembleProductionRuntime(context.Background(), config, options{modelID: "openai/gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), runtimeDeps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer productRuntime.Dispose(context.Background())
	if _, err := productRuntime.Session().Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	timeout, zero, one := uint64(1_500), uint64(0), uint64(1)
	if err := productRuntime.Services().ModelRuntime.SetGlobalSettings(context.Background(), func(settings *model.Settings) error {
		settings.Transport = provider.TransportSSE
		settings.HTTPIdleTimeoutMS = &zero
		settings.WebsocketConnectTimeoutMS = &zero
		settings.Retry.Provider.TimeoutMS = &timeout
		settings.Retry.Provider.MaxRetries = &one
		settings.Retry.Provider.MaxRetryDelayMS = &zero
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := productRuntime.Session().Run(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	deadlines := doer.snapshot()
	if len(deadlines) != 3 {
		t.Fatalf("provider attempts = %d, deadlines %#v", len(deadlines), deadlines)
	}
	if deadlines[0] < time.Minute {
		t.Fatalf("first turn did not use initial 300s timeout: %s", deadlines[0])
	}
	for index, remaining := range deadlines[1:] {
		if remaining <= 0 || remaining > 2*time.Second {
			t.Fatalf("second-turn attempt %d deadline = %s, want refreshed 1500ms timeout", index+1, remaining)
		}
	}
	settings := productRuntime.Services().ModelRuntime.Snapshot().Settings
	stream, err := productionProviderStreamOptions(settings, manager.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if stream.Transport != provider.TransportSSE || stream.WebsocketConnectTimeoutMS == nil || *stream.WebsocketConnectTimeoutMS != 0 ||
		stream.MaxRetries == nil || *stream.MaxRetries != 1 || stream.MaxRetryDelayMS == nil || *stream.MaxRetryDelayMS != 0 {
		t.Fatalf("refreshed stream options = %#v", stream)
	}
}

func TestProductionReloadRebuildsModelAndStandaloneBashFromSettings(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, true)
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"shellPath":"initial-shell","shellCommandPrefix":"prefix-one","images":{"autoResize":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	doer := &reloadToolSettingsDoer{}
	runner := &reloadToolSettingsRunner{}
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	config.OpenAIHTTPClient = doer
	config.BashRunner = runner
	runtimeDeps, err := assembleProductionRuntime(context.Background(), config, options{modelID: "openai/gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), runtimeDeps.factory, agentruntime.InitialOptions{
		CWD: cwd, AgentDir: agentDir, SessionManager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer productRuntime.Dispose(context.Background())
	if result, err := productRuntime.Session().Run(context.Background(), "first"); err != nil || !result.Succeeded() {
		t.Fatalf("first run = (%#v, %v)", result, err)
	}
	autoResize := false
	if err := productRuntime.Services().ModelRuntime.SetGlobalSettings(context.Background(), func(settings *model.Settings) error {
		settings.ShellPath = "reloaded-shell"
		settings.ShellCommandPrefix = "prefix-two"
		settings.Images.AutoResize = &autoResize
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := productRuntime.Session().Reload(context.Background(), agent.ReloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if result, err := productRuntime.Session().Run(context.Background(), "second"); err != nil || !result.Succeeded() {
		t.Fatalf("second run = (%#v, %v)", result, err)
	}
	if _, err := productRuntime.Session().ExecuteBash(context.Background(), "standalone", nil, agent.ExecuteBashOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"prefix-one\nmodel command", "prefix-two\nmodel command", "prefix-two\nstandalone"}
	if got := runner.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("bash commands across reload = %#v, want %#v", got, want)
	}
	settings := productRuntime.Services().ModelRuntime.Snapshot().Settings
	if settings.ShellPath != "reloaded-shell" || settings.ShellCommandPrefix != "prefix-two" || settings.Images.AutoResizeOrDefault() {
		t.Fatalf("reloaded settings snapshot = %#v", settings)
	}
}

func TestProductionRuntimeLoadsAndReloadsSettingsResourcePaths(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, true)
	firstRoot, secondRoot := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	firstSkill := filepath.Join(firstRoot, "skills", "settings-first", "SKILL.md")
	secondSkill := filepath.Join(secondRoot, "skills", "settings-second", "SKILL.md")
	firstPrompt := filepath.Join(firstRoot, "prompts", "settings-first.md")
	secondPrompt := filepath.Join(secondRoot, "prompts", "settings-second.md")
	for path, content := range map[string]string{
		firstSkill:   "---\nname: settings-first\ndescription: First settings skill\n---\nfirst skill body",
		secondSkill:  "---\nname: settings-second\ndescription: Second settings skill\n---\nsecond skill body",
		firstPrompt:  "first expansion $@",
		secondPrompt: "second expansion $@",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSettings := func(skill, prompt string) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"skills": []string{skill}, "prompts": []string{prompt}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSettings(firstSkill, firstPrompt)
	runtimeDeps, err := assembleProductionRuntime(context.Background(), fixedProductionConfig(cwd, agentDir, docsDir), options{modelID: "openai/gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), runtimeDeps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer productRuntime.Dispose(context.Background())
	assertResources := func(skillName, promptName, expanded, absentSkill string) {
		t.Helper()
		snapshot, err := productRuntime.Services().ResourceService.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		foundSkill, foundPrompt, foundAbsent := false, false, false
		for _, skill := range snapshot.Skills {
			foundSkill = foundSkill || skill.Name == skillName
			foundAbsent = foundAbsent || skill.Name == absentSkill
		}
		for _, prompt := range snapshot.Templates {
			foundPrompt = foundPrompt || prompt.Name == promptName
		}
		if !foundSkill || !foundPrompt || foundAbsent || !strings.Contains(productRuntime.Session().SystemPrompt(), "<name>"+skillName+"</name>") {
			t.Fatalf("resource snapshot after settings load = skills %#v templates %#v prompt %q", snapshot.Skills, snapshot.Templates, productRuntime.Session().SystemPrompt())
		}
		value, err := productRuntime.Services().ResourceService.ExpandInput("/" + promptName + " value")
		if err != nil || value != expanded {
			t.Fatalf("settings prompt expansion = (%q, %v), want %q", value, err, expanded)
		}
	}
	assertResources("settings-first", "settings-first", "first expansion value", "settings-second")
	writeSettings(secondSkill, secondPrompt)
	if err := productRuntime.Session().Reload(context.Background(), agent.ReloadOptions{}); err != nil {
		t.Fatal(err)
	}
	assertResources("settings-second", "settings-second", "second expansion value", "settings-first")
}

func TestProductionExplicitModelCanRunAfterRuntimeCredentialAdded(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, false)
	doer := &dynamicAuthDoer{}
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	config.OpenAIHTTPClient = doer
	runtimeDeps, err := assembleProductionRuntime(context.Background(), config, options{modelID: "openai/gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), runtimeDeps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer productRuntime.Dispose(context.Background())
	baseline := len(manager.Entries())
	if err := productRuntime.Session().SetModel(productRuntime.Session().Model()); err == nil || err.Error() != "No API key for openai/gpt-5.5" {
		t.Fatalf("SetModel error = %v", err)
	}
	if got := len(manager.Entries()); got != baseline {
		t.Fatalf("failed SetModel changed entries from %d to %d", baseline, got)
	}
	if err := productRuntime.Services().AuthRuntime.SetAPIKey("openai", "late-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := productRuntime.Session().Run(context.Background(), "after login"); err != nil {
		t.Fatal(err)
	}
	if got := doer.snapshot(); len(got) != 1 || got[0] != "Bearer late-key" {
		t.Fatalf("authorization sequence = %#v", got)
	}
}

func TestProductionAPIKeyWithoutModelRemainsDiagnosticOnly(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	runtimeDeps, err := assembleProductionRuntime(context.Background(), config, options{
		hasAPIKey: true,
		apiKey:    "diagnostic-only-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := runtimeDeps.factory(context.Background(), agentruntime.CreateOptions{
		CWD: cwd, AgentDir: agentDir, SessionManager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer created.Session.Close(context.Background())
	if created.Session.HasModel() {
		t.Fatal("diagnostic-only CLI key made an automatic model available")
	}
	credential, exists, err := created.Services.AuthRuntime.Read(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if exists || credential.Key != "" {
		t.Fatal("diagnostic-only CLI key was installed in the auth runtime")
	}
	if len(created.Diagnostics) != 1 || created.Diagnostics[0].Kind != agentruntime.DiagnosticError {
		t.Fatalf("diagnostics = %#v", created.Diagnostics)
	}
}

func TestProductionRuntimeReplacementRebuildsCwdBoundServices(t *testing.T) {
	firstCWD, secondCWD := t.TempDir(), t.TempDir()
	agentDir, docsDir := t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, true)
	for _, fixture := range []struct{ cwd, prompt, marker string }{
		{firstCWD, "first project prompt", "first marker"},
		{secondCWD, "second project prompt", "second marker"},
	} {
		if err := os.MkdirAll(filepath.Join(fixture.cwd, ".pi"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.cwd, ".pi", "SYSTEM.md"), []byte(fixture.prompt), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.cwd, "marker.txt"), []byte(fixture.marker), 0o600); err != nil {
			t.Fatal(err)
		}
		service, err := resource.New(resource.Config{CWD: fixture.cwd, AgentDir: agentDir})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Trust().Set(context.Background(), fixture.cwd, true); err != nil {
			t.Fatal(err)
		}
	}

	runtimeDeps, err := assembleProductionRuntime(context.Background(), fixedProductionConfig(firstCWD, agentDir, docsDir), options{providerID: "openai", modelID: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	initialManager, err := session.InMemorySessionManager(firstCWD, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), runtimeDeps.factory, agentruntime.InitialOptions{CWD: firstCWD, AgentDir: agentDir, SessionManager: initialManager})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := productRuntime.Dispose(context.Background()); err != nil {
			t.Errorf("dispose runtime: %v", err)
		}
	}()
	firstServices := productRuntime.Services()
	assertCwdBoundServices(t, firstServices, firstCWD, "first project prompt", "first marker")
	entries := productRuntime.Session().SessionManager().Entries()
	if len(entries) != 2 {
		t.Fatalf("new session initialization entries = %d, want model and thinking", len(entries))
	}
	if _, ok := entries[0].Payload().(session.ModelChangePayload); !ok {
		t.Fatalf("initial entry 0 = %T, want model change", entries[0].Payload())
	}
	if _, ok := entries[1].Payload().(session.ThinkingLevelChangePayload); !ok {
		t.Fatalf("initial entry 1 = %T, want thinking change", entries[1].Payload())
	}

	targetPath := filepath.Join(t.TempDir(), "target.jsonl")
	target, err := session.Create(targetPath, session.CreateOptions{ID: "target", WorkingDir: secondCWD})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := productRuntime.SwitchSession(context.Background(), targetPath, agentruntime.SwitchOptions{}); err != nil {
		t.Fatal(err)
	}
	secondServices := productRuntime.Services()
	if secondServices == firstServices || secondServices.ResourceService == firstServices.ResourceService ||
		secondServices.ModelRuntime == firstServices.ModelRuntime || secondServices.AuthRuntime == firstServices.AuthRuntime ||
		secondServices.Provider == firstServices.Provider || secondServices.Tool == firstServices.Tool {
		t.Fatal("session replacement retained a cwd-bound service instance")
	}
	if len(secondServices.Tools) == 0 || len(secondServices.Tools) != len(firstServices.Tools) {
		t.Fatalf("replacement tools = %d, initial tools = %d", len(secondServices.Tools), len(firstServices.Tools))
	}
	for index := range firstServices.Tools {
		if firstServices.Tools[index].Name() != secondServices.Tools[index].Name() {
			t.Fatalf("replacement tool %d = %q, want %q", index, secondServices.Tools[index].Name(), firstServices.Tools[index].Name())
		}
	}
	assertCwdBoundServices(t, secondServices, secondCWD, "second project prompt", "second marker")
}

func assertCwdBoundServices(t *testing.T, services *agentruntime.Services, cwd, prompt, marker string) {
	t.Helper()
	if services == nil || services.CWD != filepath.Clean(cwd) {
		t.Fatalf("services cwd = %#v, want %q", services, cwd)
	}
	snapshot, err := services.ResourceService.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.SystemPrompt, prompt) || !strings.Contains(snapshot.SystemPrompt, filepath.Clean(cwd)) {
		t.Fatalf("system prompt = %q, want project prompt %q and cwd %q", snapshot.SystemPrompt, prompt, cwd)
	}
	named, ok := services.Tool.(agent.NamedToolExecutor)
	if !ok {
		t.Fatalf("tool executor = %T, want named executor", services.Tool)
	}
	output, err := named.ExecuteNamed(context.Background(), "call-read", "read", []byte(`{"path":"marker.txt"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Text, marker) {
		t.Fatalf("read output = %q, want marker %q", output.Text, marker)
	}
}

func TestRunProductionMissingStoredCwdPrecedesServiceConstruction(t *testing.T) {
	storedCWD, startupCWD := t.TempDir(), t.TempDir()
	agentDir, docsDir := t.TempDir(), t.TempDir()
	sessionPath := filepath.Join(t.TempDir(), "missing-cwd.jsonl")
	stored, err := session.Create(sessionPath, session.CreateOptions{ID: "missing-cwd", WorkingDir: storedCWD})
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.Close(); err != nil {
		t.Fatal(err)
	}
	movedParent, err := os.MkdirTemp("/tmp", "pi-go-missing-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	movedCWD := filepath.Join(movedParent, "stored")
	if err := os.Rename(storedCWD, movedCWD); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(movedCWD, storedCWD); err != nil {
			t.Errorf("restore test cwd: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(`{"providers":`), 0o600); err != nil {
		t.Fatal(err)
	}
	doer := &rejectingHTTPDoer{}
	config := fixedProductionConfig(startupCWD, agentDir, docsDir)
	config.OpenAIHTTPClient = doer
	var stdout, stderr bytes.Buffer
	code := RunProduction(context.Background(), config, []string{"--session", sessionPath, "--model", "openai/gpt-5.5", "-p", "hello"}, &stdout, &stderr)
	if code != ExitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), "Stored session working directory does not exist") {
		t.Fatalf("RunProduction = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "models.json") {
		t.Fatalf("service construction ran before cwd admission: %q", stderr.String())
	}
	if doer.calls.Load() != 0 {
		t.Fatalf("provider calls = %d", doer.calls.Load())
	}
}

func TestRunProductionModelLessDoesNotInvokeProvider(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	doer := &rejectingHTTPDoer{}
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	config.OpenAIHTTPClient = doer
	sessionPath := filepath.Join(t.TempDir(), "no-model.jsonl")
	var stdout, stderr bytes.Buffer
	code := RunProduction(context.Background(), config, []string{"--session", sessionPath, "-p", "hello"}, &stdout, &stderr)
	if code != ExitFailure || stdout.Len() != 0 {
		t.Fatalf("RunProduction = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"No models available", filepath.Join(docsDir, "providers.md"), filepath.Join(docsDir, "models.md")} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr = %q, missing %q", stderr.String(), expected)
		}
	}
	if doer.calls.Load() != 0 {
		t.Fatalf("provider calls = %d", doer.calls.Load())
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model-less metadata-only session unexpectedly persisted: %v", err)
	}
}

func TestResolveProductionDocsDirUsesExecutableSibling(t *testing.T) {
	got, err := resolveProductionDocsDir("")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join(filepath.Dir(executable), "docs"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(want) {
		t.Fatalf("docs dir = %q, want %q", got, want)
	}
}

func TestRunApplicationReportsDiagnosticsAndBlocksOnError(t *testing.T) {
	cwd, agentDir := t.TempDir(), t.TempDir()
	sessionPath := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "diagnostic-model", Name: "Diagnostic Model",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var beforeAgentCalls atomic.Uint32
	build := func(context.Context, options) (runtimeDependencies, error) {
		return runtimeDependencies{
			workingDir: cwd, agentDir: agentDir, sessionID: "diagnostics", sessionNow: time.Now,
			defaultSessionPath: func(string) (string, error) { return sessionPath, nil },
			factory: func(_ context.Context, create agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
				created, err := agent.NewSession(agent.SessionConfig{
					Provider: implementation, SessionManager: create.SessionManager, Model: selected,
					Hooks: agent.Hooks{BeforeAgentStart: func(context.Context, agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
						beforeAgentCalls.Add(1)
						return agent.BeforeAgentStartResult{}, nil
					}},
				})
				return agentruntime.CreateResult{
					Session: created, Services: &agentruntime.Services{CWD: create.SessionManager.Cwd(), AgentDir: agentDir},
					Diagnostics: []agentruntime.Diagnostic{
						{Kind: agentruntime.DiagnosticInfo, Message: "catalog info"},
						{Kind: agentruntime.DiagnosticWarning, Message: "catalog warning"},
						{Kind: agentruntime.DiagnosticError, Message: "catalog error"},
					},
				}, err
			},
		}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runApplication(context.Background(), []string{"-p", "must not run"}, &stdout, &stderr, build)
	if code != ExitFailure || stdout.Len() != 0 || stderr.String() != "catalog info\nWarning: catalog warning\nError: catalog error\n" {
		t.Fatalf("runApplication = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if beforeAgentCalls.Load() != 0 || len(implementation.Requests()) != 0 {
		t.Fatalf("diagnostic error reached prompt: hooks=%d requests=%d", beforeAgentCalls.Load(), len(implementation.Requests()))
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diagnostic-only session persisted: %v", err)
	}
}

func TestInjectedFactoryRebindsStreamSessionIDAfterReplacement(t *testing.T) {
	cwd := t.TempDir()
	var observed []string
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{"first", "second"} {
		answer := answer
		step, err := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
			observed = append(observed, request.StreamOptions().SessionID)
			return integrationTextTerminal(answer)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := implementation.AppendResponses([]provider.ScriptStep{step}); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "session-id", Name: "Session ID",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	deps, err := validateDependencies(Dependencies{
		Provider: implementation, Model: selected, WorkingDir: cwd,
		SessionID: "static-dependency-id", SessionNow: time.Now,
		Stream: provider.StreamOptions{SessionID: "stale-stream-id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{ID: "first-session"})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), deps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: deps.agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer productRuntime.Dispose(context.Background())
	firstID := productRuntime.Session().SessionManager().SessionID()
	if _, err := productRuntime.Session().Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := productRuntime.NewSession(context.Background(), agentruntime.NewOptions{}); err != nil {
		t.Fatal(err)
	}
	secondID := productRuntime.Session().SessionManager().SessionID()
	if firstID == secondID {
		t.Fatalf("replacement session id did not change: %q", firstID)
	}
	if _, err := productRuntime.Session().Run(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] != firstID || observed[1] != secondID {
		t.Fatalf("request session ids = %#v, want [%q %q]", observed, firstID, secondID)
	}
}

func integrationTextTerminal(text string) (llm.AssistantTerminal, error) {
	block, err := llm.NewTextBlock(text)
	if err != nil {
		return nil, err
	}
	usage, err := llm.NewUsage(llm.UsageSpec{})
	if err != nil {
		return nil, err
	}
	message, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{block}, llm.FinishStop, usage, time.Now(),
		llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "session-id"},
	)
	if err != nil {
		return nil, err
	}
	return message, nil
}
