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
	service := testService(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}, nil)
	server, err := New(Options{Version: "test", Assets: fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><title>real webui</title>")},
		"sw.js":             {Data: []byte("self.addEventListener('fetch',()=>{})")},
		"_next/static/a.js": {Data: []byte("console.log('asset')")},
	}, Application: service, AllowedHosts: []string{"example.com"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func testService(t *testing.T, config app.ProductionConfig, opener application.RuntimeOpener) *application.Service {
	t.Helper()
	service, err := application.NewService(application.ServiceOptions{
		Context: context.Background(), Production: config, OpenRuntime: opener, DisableReaper: true,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Errorf("close Service: %v", err)
		}
	})
	return service
}

func TestCapabilitiesReportUnavailableModulesExplicitly(t *testing.T) {
	response := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
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
	testServer(t).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/not-real", nil))
	if response.Code != http.StatusNotImplemented || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response = %d, %q", response.Code, response.Header().Get("Content-Type"))
	}
	if response.Body.String() == "" {
		t.Fatal("missing unsupported response")
	}
}

func TestAPIRejectsUntrustedHostsCrossSiteBrowsersAndNonJSONMutations(t *testing.T) {
	server := testServer(t)

	untrustedHost := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	untrustedHost.Host = "attacker.example"
	untrustedHostResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(untrustedHostResponse, untrustedHost)
	if untrustedHostResponse.Code != http.StatusForbidden {
		t.Fatalf("untrusted Host status = %d", untrustedHostResponse.Code)
	}

	crossSite := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSiteResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(crossSiteResponse, crossSite)
	if crossSiteResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-site status = %d", crossSiteResponse.Code)
	}

	plain := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"cwd":"/tmp"}`))
	plain.Header.Set("Content-Type", "text/plain")
	plainResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(plainResponse, plain)
	if plainResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain mutation status = %d, body = %s", plainResponse.Code, plainResponse.Body.String())
	}
}

func TestAPIOnlyServerKeepsAPIRoutesAndDoesNotServeAnApplicationShell(t *testing.T) {
	cwd := t.TempDir()
	service := testService(t, app.ProductionConfig{
		WorkingDir: cwd,
		AgentDir:   filepath.Join(t.TempDir(), "agent"),
	}, nil)
	server, err := New(Options{Version: "test", Application: service, AllowedHosts: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	root := httptest.NewRecorder()
	server.Handler().ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusNotFound {
		t.Fatalf("API-only root status = %d", root.Code)
	}
}

type cursorTestAPI struct {
	application.API
	revision uint64
}

func (a cursorTestAPI) CurrentRevision() uint64 { return a.revision }

func (a cursorTestAPI) SubscribeEvents(after uint64) (*application.EventSubscription, error) {
	if after != a.revision {
		return nil, application.ErrEventCursorUnavailable
	}
	events := make(chan application.Event)
	close(events)
	return &application.EventSubscription{Events: events, Revision: a.revision}, nil
}

func TestApplicationEventStreamRequestsSnapshotForExpiredCursor(t *testing.T) {
	server, err := New(Options{Version: "test", Application: cursorTestAPI{revision: 42}, AllowedHosts: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	request.Header.Set("Last-Event-ID", "1")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %d, %q", response.Code, response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if !strings.Contains(body, "id: 42\n") || !strings.Contains(body, `"type":"reset_required"`) || !strings.Contains(body, `"type":"connected"`) {
		t.Fatalf("SSE body = %q", body)
	}
}

func TestApplicationEventCursorPrefersReconnectHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=3", nil)
	request.Header.Set("Last-Event-ID", "7")
	cursor, present, err := eventCursor(request)
	if err != nil || !present || cursor != 7 {
		t.Fatalf("eventCursor = (%d, %v, %v)", cursor, present, err)
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

func webThinkingTerminal(t *testing.T, thinkingText, text string) llm.AssistantRichMessage {
	t.Helper()
	thinking, err := llm.NewThinkingBlock(thinkingText)
	if err != nil {
		t.Fatal(err)
	}
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantRichMessage(
		[]llm.AssistantBlock{thinking, block}, llm.FinishStop, llm.Usage{}, time.Now(),
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
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
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
	step, err := provider.FixedResponseStep(webThinkingTerminal(t, "inspect the request", "pong"))
	if err != nil {
		t.Fatal(err)
	}
	titleStep, err := provider.FixedResponseStep(webTerminal(t, "Web session flow"))
	if err != nil {
		t.Fatal(err)
	}
	service := testService(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}, scriptedWebRuntimeOpener(t, step, titleStep))
	surface, err := New(Options{
		Version: "test", Assets: fstest.MapFS{"index.html": {Data: []byte("web")}},
		Application: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(surface.Handler())
	t.Cleanup(httpServer.Close)

	streamContext, cancelStream := context.WithCancel(context.Background())
	t.Cleanup(cancelStream)
	streamRequest, _ := http.NewRequestWithContext(streamContext, http.MethodGet, httpServer.URL+"/api/v1/events", nil)
	streamResponse, err := httpServer.Client().Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = streamResponse.Body.Close() })
	reader := bufio.NewReader(streamResponse.Body)
	if event := readSSEEvent(t, reader); event["type"] != "connected" {
		t.Fatalf("first SSE event = %#v", event)
	}

	created := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sessions", map[string]any{
		"cwd": cwd,
	})
	sessionID, _ := created["sessionId"].(string)
	if sessionID == "" || created["model"] == nil {
		t.Fatalf("create response = %#v", created)
	}

	result := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sessions/"+sessionID+"/commands", map[string]any{
		"type": "prompt", "message": "ping",
	})
	if data, ok := result["data"].(map[string]any); !ok || data["operationId"] == nil {
		t.Fatalf("prompt response = %#v", result)
	}

	seen := map[string]bool{}
	for !seen["operation"] {
		envelope := readSSEEvent(t, reader)
		if envelope["sessionId"] != sessionID {
			continue
		}
		event, _ := envelope["event"].(map[string]any)
		typeName, _ := event["type"].(string)
		seen[typeName] = true
		if typeName == "operation" && event["status"] != "completed" {
			t.Fatalf("operation event = %#v", event)
		}
	}
	for _, expected := range []string{"session_catalog", "agent_start", "message_end", "agent_end", "agent_settled", "operation"} {
		if !seen[expected] {
			t.Fatalf("missing %s in SSE events: %#v", expected, seen)
		}
	}
	cancelStream()

	response, err := httpServer.Client().Get(httpServer.URL + "/api/v1/sessions/" + sessionID + "?deferThinking=1&deferMedia=1")
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
	thinkingResponse, err := httpServer.Client().Get(
		httpServer.URL + "/api/v1/sessions/" + sessionID + "/entries/" + view.Context.EntryIDs[1] + "/thinking?blockIndex=0",
	)
	if err != nil {
		t.Fatal(err)
	}
	var thinkingBody map[string]any
	if err := json.NewDecoder(thinkingResponse.Body).Decode(&thinkingBody); err != nil {
		_ = thinkingResponse.Body.Close()
		t.Fatal(err)
	}
	_ = thinkingResponse.Body.Close()
	if thinkingResponse.StatusCode != http.StatusOK || thinkingBody["thinking"] != "inspect the request" {
		t.Fatalf("thinking response = %d, %#v", thinkingResponse.StatusCode, thinkingBody)
	}

	named := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sessions/"+sessionID+"/auto-name", map[string]any{})
	if named["title"] != "Web session flow" {
		t.Fatalf("auto-name response = %#v", named)
	}
	exportResponse, err := httpServer.Client().Get(httpServer.URL + "/api/v1/sessions/" + sessionID + "/export")
	if err != nil {
		t.Fatal(err)
	}
	exportBody, err := io.ReadAll(exportResponse.Body)
	_ = exportResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if exportResponse.StatusCode != http.StatusOK || !strings.HasPrefix(string(exportBody), "<!doctype html>") || !strings.HasPrefix(exportResponse.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("export response = %d, %q, %q", exportResponse.StatusCode, exportResponse.Header.Get("Content-Disposition"), exportBody[:min(len(exportBody), 40)])
	}

	forked := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/sessions/"+sessionID+"/commands", map[string]any{
		"type": "fork", "entryId": view.Context.EntryIDs[0],
	})
	data, _ := forked["data"].(map[string]any)
	newSessionID, _ := data["newSessionId"].(string)
	if newSessionID == "" || newSessionID == sessionID || data["cancelled"] != false {
		t.Fatalf("fork response = %#v", forked)
	}

	activeResponse, err := httpServer.Client().Get(httpServer.URL + "/api/v1/sessions/" + newSessionID + "/state")
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

	sourceResponse, err := httpServer.Client().Get(httpServer.URL + "/api/v1/sessions/" + sessionID + "/state")
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

	forkViewResponse, err := httpServer.Client().Get(httpServer.URL + "/api/v1/sessions/" + newSessionID)
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

	deleteRequest, err := http.NewRequest(http.MethodDelete, httpServer.URL+"/api/v1/sessions/"+sessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteResponse, err := httpServer.Client().Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	deleteData, _ := io.ReadAll(deleteResponse.Body)
	_ = deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusOK {
		t.Fatalf("delete response = %d: %s", deleteResponse.StatusCode, deleteData)
	}
	forkAfterDelete, err := httpServer.Client().Get(httpServer.URL + "/api/v1/sessions/" + newSessionID)
	if err != nil {
		t.Fatal(err)
	}
	_ = forkAfterDelete.Body.Close()
	if forkAfterDelete.StatusCode != http.StatusOK {
		t.Fatalf("fork after parent deletion = %d", forkAfterDelete.StatusCode)
	}
}

func TestWebProjectTrustAndSkillToggleUseApplicationResources(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	skillPath := filepath.Join(cwd, ".pi", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("---\nname: review\ndescription: Review code safely\n---\n\n# Review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}, nil)
	surface, err := New(Options{Version: "test", Application: service})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(surface.Handler())
	t.Cleanup(httpServer.Close)

	trustResponse, err := httpServer.Client().Get(httpServer.URL + "/api/v1/system/project-trust?cwd=" + cwd)
	if err != nil {
		t.Fatal(err)
	}
	var trust map[string]any
	if err := json.NewDecoder(trustResponse.Body).Decode(&trust); err != nil {
		_ = trustResponse.Body.Close()
		t.Fatal(err)
	}
	_ = trustResponse.Body.Close()
	if trust["requiresTrust"] != true || trust["trusted"] != false {
		t.Fatalf("initial trust response = %#v", trust)
	}
	trusted := postJSON(t, httpServer.Client(), httpServer.URL+"/api/v1/system/project-trust", map[string]any{"cwd": cwd})
	if trusted["trusted"] != true {
		t.Fatalf("trusted response = %#v", trusted)
	}

	skillsResponse, err := httpServer.Client().Get(httpServer.URL + "/api/v1/skills?cwd=" + cwd)
	if err != nil {
		t.Fatal(err)
	}
	var skillsBody struct {
		Skills []struct {
			Name     string `json:"name"`
			FilePath string `json:"filePath"`
		} `json:"skills"`
		ProjectResourcesLoaded bool `json:"projectResourcesLoaded"`
	}
	if err := json.NewDecoder(skillsResponse.Body).Decode(&skillsBody); err != nil {
		_ = skillsResponse.Body.Close()
		t.Fatal(err)
	}
	_ = skillsResponse.Body.Close()
	if skillsResponse.StatusCode != http.StatusOK || !skillsBody.ProjectResourcesLoaded || len(skillsBody.Skills) != 1 || skillsBody.Skills[0].Name != "review" {
		t.Fatalf("skills response = %d, %#v", skillsResponse.StatusCode, skillsBody)
	}
	toggleBody, err := json.Marshal(map[string]any{
		"cwd": cwd, "filePath": skillsBody.Skills[0].FilePath, "disableModelInvocation": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	toggleRequest, err := http.NewRequest(http.MethodPatch, httpServer.URL+"/api/v1/skills", bytes.NewReader(toggleBody))
	if err != nil {
		t.Fatal(err)
	}
	toggleRequest.Header.Set("Content-Type", "application/json")
	toggleResponse, err := httpServer.Client().Do(toggleRequest)
	if err != nil {
		t.Fatal(err)
	}
	toggleData, _ := io.ReadAll(toggleResponse.Body)
	_ = toggleResponse.Body.Close()
	if toggleResponse.StatusCode != http.StatusOK {
		t.Fatalf("toggle response = %d: %s", toggleResponse.StatusCode, toggleData)
	}
	updated, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated, []byte("disable-model-invocation: true")) {
		t.Fatalf("updated skill = %s", updated)
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
