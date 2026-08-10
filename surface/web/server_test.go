package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	supervisor := testSupervisor(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}, nil)
	server, err := New(Options{Version: "test", Assets: fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><title>real webui</title>")},
		"sw.js":             {Data: []byte("self.addEventListener('fetch',()=>{})")},
		"_next/static/a.js": {Data: []byte("console.log('asset')")},
	}, Supervisor: supervisor})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func testSupervisor(t *testing.T, config app.ProductionConfig, opener application.RuntimeOpener) *application.Supervisor {
	t.Helper()
	supervisor, err := application.NewSupervisor(application.SupervisorOptions{
		Context: context.Background(), Production: config, OpenRuntime: opener, DisableReaper: true,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(func() {
		if err := supervisor.Close(context.Background()); err != nil {
			t.Errorf("close Supervisor: %v", err)
		}
	})
	return supervisor
}

func TestCapabilitiesReportUnavailableModulesExplicitly(t *testing.T) {
	response := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var body struct {
		Capabilities []Capability `json:"capabilities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Capabilities) == 0 {
		t.Fatal("capability manifest is empty")
	}
	for _, capability := range body.Capabilities {
		if capability.ID == "agent_chat" && capability.Status != CapabilityConnected {
			t.Fatalf("agent_chat status = %q", capability.Status)
		}
	}
}

func TestUnknownAPIReturnsStructuredNotImplemented(t *testing.T) {
	response := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/not-real", nil))
	if response.Code != http.StatusNotImplemented || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response = %d, %q", response.Code, response.Header().Get("Content-Type"))
	}
	if response.Body.String() == "" {
		t.Fatal("missing unsupported response")
	}
}

func TestAPIOnlyServerKeepsAPIRoutesAndDoesNotServeAnApplicationShell(t *testing.T) {
	cwd := t.TempDir()
	supervisor := testSupervisor(t, app.ProductionConfig{
		WorkingDir: cwd,
		AgentDir:   filepath.Join(t.TempDir(), "agent"),
	}, nil)
	server, err := New(Options{Version: "test", Supervisor: supervisor})
	if err != nil {
		t.Fatal(err)
	}

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	root := httptest.NewRecorder()
	server.Handler().ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusNotFound {
		t.Fatalf("API-only root status = %d", root.Code)
	}
}

func scriptedWebRuntimeOpener(t *testing.T, steps ...provider.ScriptStep) application.RuntimeOpener {
	t.Helper()
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time {
		return time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatal(err)
	}
	model, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "web-model", Name: "Web Model",
		Input: []provider.InputKind{provider.InputText, provider.InputImage}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, config app.ProductionConfig, selection app.ProductionRuntimeOptions) (*agentruntime.Runtime, error) {
		var manager *session.SessionManager
		var err error
		if selection.SessionPath != "" {
			manager, err = session.OpenSessionManager(selection.SessionPath, filepath.Dir(selection.SessionPath), "")
		} else {
			directory, dirErr := session.SessionDirForAgentDir(config.WorkingDir, config.AgentDir)
			if dirErr != nil {
				return nil, dirErr
			}
			manager, err = session.CreateSessionManager(config.WorkingDir, directory, session.NewSessionOptions{})
		}
		if err != nil {
			return nil, err
		}
		factory := func(_ context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
			coordinator, createErr := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: options.SessionManager, Model: model,
				AllModels: []provider.Model{model}, ThinkingLevel: provider.ThinkingOff,
				SystemPrompt: "web test", SettlementTimeout: time.Second,
			})
			if createErr != nil {
				return agentruntime.CreateResult{}, createErr
			}
			return agentruntime.CreateResult{
				Session: coordinator,
				Services: &agentruntime.Services{
					CWD: options.SessionManager.Cwd(), AgentDir: config.AgentDir, Provider: implementation,
				},
			}, nil
		}
		runtime, createErr := agentruntime.Create(ctx, factory, agentruntime.InitialOptions{
			CWD: manager.Cwd(), AgentDir: config.AgentDir, SessionManager: manager,
		})
		if createErr != nil {
			_ = manager.Close()
			return nil, createErr
		}
		return runtime, nil
	}
}

func webTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, time.Now(),
		llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "web-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func postJSON(t *testing.T, client *http.Client, url string, value any) map[string]any {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", url, response.StatusCode, data)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
}

func TestNativeWebAgentSessionAndSSEFlow(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	step, err := provider.FixedResponseStep(webTerminal(t, "pong"))
	if err != nil {
		t.Fatal(err)
	}
	supervisor := testSupervisor(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}, scriptedWebRuntimeOpener(t, step))
	surface, err := New(Options{
		Version: "test", Assets: fstest.MapFS{"index.html": {Data: []byte("web")}},
		Supervisor: supervisor,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(surface.Handler())
	t.Cleanup(httpServer.Close)

	created := postJSON(t, httpServer.Client(), httpServer.URL+"/api/agent/new", map[string]any{
		"cwd": cwd, "type": "ensure_session",
	})
	sessionID, _ := created["sessionId"].(string)
	if sessionID == "" || created["model"] == nil {
		t.Fatalf("create response = %#v", created)
	}

	streamContext, cancelStream := context.WithCancel(context.Background())
	t.Cleanup(cancelStream)
	streamRequest, _ := http.NewRequestWithContext(streamContext, http.MethodGet, httpServer.URL+"/api/agent/"+sessionID+"/events", nil)
	streamResponse, err := httpServer.Client().Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = streamResponse.Body.Close() })
	reader := bufio.NewReader(streamResponse.Body)
	if event := readSSEEvent(t, reader); event["type"] != "connected" {
		t.Fatalf("first SSE event = %#v", event)
	}

	result := postJSON(t, httpServer.Client(), httpServer.URL+"/api/agent/"+sessionID, map[string]any{
		"type": "prompt", "message": "ping",
	})
	if result["success"] != true {
		t.Fatalf("prompt response = %#v", result)
	}

	seen := map[string]bool{}
	for !seen["prompt_done"] {
		event := readSSEEvent(t, reader)
		typeName, _ := event["type"].(string)
		seen[typeName] = true
	}
	for _, expected := range []string{"agent_start", "message_end", "agent_end", "agent_settled", "prompt_done"} {
		if !seen[expected] {
			t.Fatalf("missing %s in SSE events: %#v", expected, seen)
		}
	}
	cancelStream()

	response, err := httpServer.Client().Get(httpServer.URL + "/api/sessions/" + sessionID + "?deferThinking=1&deferMedia=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var view sessionViewWire
	if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || view.SessionID != sessionID || len(view.Context.Messages) != 2 || len(view.Context.EntryIDs) != 2 {
		t.Fatalf("session view = status %d, %#v", response.StatusCode, view)
	}

	forked := postJSON(t, httpServer.Client(), httpServer.URL+"/api/agent/"+sessionID, map[string]any{
		"type": "fork", "entryId": view.Context.EntryIDs[0],
	})
	data, _ := forked["data"].(map[string]any)
	newSessionID, _ := data["newSessionId"].(string)
	if newSessionID == "" || newSessionID == sessionID || data["cancelled"] != false {
		t.Fatalf("fork response = %#v", forked)
	}

	activeResponse, err := httpServer.Client().Get(httpServer.URL + "/api/agent/" + newSessionID)
	if err != nil {
		t.Fatal(err)
	}
	var activeState map[string]any
	if err := json.NewDecoder(activeResponse.Body).Decode(&activeState); err != nil {
		_ = activeResponse.Body.Close()
		t.Fatal(err)
	}
	_ = activeResponse.Body.Close()
	state, _ := activeState["state"].(map[string]any)
	if activeResponse.StatusCode != http.StatusOK || activeState["running"] != true || state["sessionId"] != newSessionID {
		t.Fatalf("forked agent state = status %d, %#v", activeResponse.StatusCode, activeState)
	}

	sourceResponse, err := httpServer.Client().Get(httpServer.URL + "/api/agent/" + sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var sourceState map[string]any
	if err := json.NewDecoder(sourceResponse.Body).Decode(&sourceState); err != nil {
		_ = sourceResponse.Body.Close()
		t.Fatal(err)
	}
	_ = sourceResponse.Body.Close()
	if sourceResponse.StatusCode != http.StatusOK || sourceState["running"] != false {
		t.Fatalf("source agent state = status %d, %#v", sourceResponse.StatusCode, sourceState)
	}

	forkViewResponse, err := httpServer.Client().Get(httpServer.URL + "/api/sessions/" + newSessionID)
	if err != nil {
		t.Fatal(err)
	}
	var forkView sessionViewWire
	if err := json.NewDecoder(forkViewResponse.Body).Decode(&forkView); err != nil {
		_ = forkViewResponse.Body.Close()
		t.Fatal(err)
	}
	_ = forkViewResponse.Body.Close()
	if forkViewResponse.StatusCode != http.StatusOK || forkView.SessionID != newSessionID {
		t.Fatalf("forked session view = status %d, %#v", forkViewResponse.StatusCode, forkView)
	}
}

func TestStaticAssetsAndSPAFallback(t *testing.T) {
	server := testServer(t)
	for _, test := range []struct {
		path, contains string
	}{
		{path: "/", contains: "real webui"},
		{path: "/session/example", contains: "real webui"},
		{path: "/_next/static/a.js", contains: "asset"},
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.contains) {
			t.Fatalf("GET %s = %d, %q", test.path, response.Code, response.Body.String())
		}
	}
}
