package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

type productionSummaryRequest struct {
	path          string
	authorization string
	sessionID     string
	payload       map[string]any
}

type productionSummaryServer struct {
	mu       sync.Mutex
	requests []productionSummaryRequest
}

func (s *productionSummaryServer) serve(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	defer request.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Errorf("decode summary request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	s.record(request, payload)
	writer.Header().Set("Content-Type", "text/event-stream")
	if strings.HasSuffix(request.URL.Path, "/chat/completions") {
		_, _ = fmt.Fprint(writer, `data: {"choices":[{"delta":{"content":"production summary"},"finish_reason":null}]}`+"\n\n"+
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n"+
			"data: [DONE]\n\n")
		return
	}
	writeProductionResponsesSSE(writer, "production summary")
}

func (s *productionSummaryServer) record(request *http.Request, payload map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionID := request.Header.Get("session_id")
	if sessionID == "" {
		sessionID = request.Header.Get("x-client-request-id")
	}
	if sessionID == "" {
		sessionID = request.Header.Get("x-session-id")
	}
	s.requests = append(s.requests, productionSummaryRequest{
		path: request.URL.Path, authorization: request.Header.Get("Authorization"), sessionID: sessionID, payload: payload,
	})
}

func writeProductionResponsesSSE(writer http.ResponseWriter, text string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	item := `{"type":"message","id":"summary-message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + fmt.Sprintf("%q", text) + `}]}`
	_, _ = fmt.Fprint(writer,
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":"+item+"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":["+item+"],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"+
			"data: [DONE]\n\n")
}

func (s *productionSummaryServer) snapshot() []productionSummaryRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]productionSummaryRequest(nil), s.requests...)
}

func writeCompactionProductionConfig(t *testing.T, agentDir, baseURL, api, key string, models ...string) {
	t.Helper()
	configuredModels := make([]map[string]any, 0, len(models))
	for _, id := range models {
		configured := map[string]any{
			"id": id, "name": id, "api": api, "reasoning": true,
			"contextWindow": 100_000, "maxTokens": 8_000,
		}
		if api == provider.OpenAICompletionsAPI {
			configured["compat"] = map[string]any{
				"sendSessionAffinityHeaders": true, "sessionAffinityFormat": "openai", "maxTokensField": "max_completion_tokens",
			}
		}
		configuredModels = append(configuredModels, configured)
	}
	modelsJSON, err := json.Marshal(map[string]any{"providers": map[string]any{"openai": map[string]any{
		"baseUrl": baseURL, "apiKey": key, "models": configuredModels,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), modelsJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsJSON, err := json.Marshal(map[string]any{
		"defaultProvider": "openai", "defaultModel": models[0], "defaultThinkingLevel": "low",
		"compaction": map[string]any{"enabled": true, "reserveTokens": 16_384, "keepRecentTokens": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settingsJSON, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendProductionCompactionHistory(t *testing.T, manager *session.SessionManager, prefix string) {
	t.Helper()
	old, err := llm.NewUserTextMessage(prefix+" old context", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	block, err := llm.NewTextBlock(prefix + " assistant context")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 10, Output: 2})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{block}, llm.FinishStop, usage, time.Now(),
		llm.AssistantProvenance{Provider: "openai", API: provider.OpenAIResponsesAPI, Model: "history-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), assistant); err != nil {
		t.Fatal(err)
	}
	retained, err := llm.NewUserTextMessage(prefix+" retained context", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), retained); err != nil {
		t.Fatal(err)
	}
}

func newPersistentProductionRuntime(t *testing.T, cwd, agentDir, path, modelID string, config ProductionConfig) (*agentruntime.Runtime, *session.SessionManager) {
	t.Helper()
	manager, err := session.OpenSessionManager(path, "", cwd)
	if err != nil {
		t.Fatal(err)
	}
	appendProductionCompactionHistory(t, manager, "initial")
	deps, err := assembleProductionRuntime(context.Background(), config, options{modelID: "openai/" + modelID})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agentruntime.Create(context.Background(), deps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, manager
}

func TestProductionCompactionUsesRealToolFreeHTTPProviderAndReloads(t *testing.T) {
	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI} {
		t.Run(api, func(t *testing.T) {
			cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
			capture := &productionSummaryServer{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { capture.serve(t, w, r) }))
			defer server.Close()
			writeCompactionProductionConfig(t, agentDir, server.URL+"/v1", api, "summary-key", "summary-model")
			path := filepath.Join(t.TempDir(), "session.jsonl")
			config := fixedProductionConfig(cwd, agentDir, docsDir)
			productRuntime, _ := newPersistentProductionRuntime(t, cwd, agentDir, path, "summary-model", config)
			result, err := productRuntime.Session().Compact(context.Background(), "preserve implementation details")
			if err != nil || !result.Committed || result.Output.Text != "production summary" {
				t.Fatalf("Compact() = %#v, %v", result, err)
			}
			if err := productRuntime.Dispose(context.Background()); err != nil {
				t.Fatal(err)
			}
			requests := capture.snapshot()
			if len(requests) != 1 || requests[0].authorization != "Bearer summary-key" || requests[0].payload["model"] != "summary-model" {
				t.Fatalf("summary requests = %#v", requests)
			}
			if _, exists := requests[0].payload["tools"]; exists {
				t.Fatalf("summary request advertised tools: %#v", requests[0].payload["tools"])
			}
			if requests[0].sessionID != "" {
				t.Fatalf("summary transmitted session affinity = %q", requests[0].sessionID)
			}
			wantPath := "/v1/responses"
			maxField := "max_output_tokens"
			if api == provider.OpenAICompletionsAPI {
				wantPath = "/v1/chat/completions"
				maxField = "max_completion_tokens"
			}
			if requests[0].path != wantPath {
				t.Fatalf("summary path = %q, want %q", requests[0].path, wantPath)
			}
			if requests[0].payload[maxField] != float64(8_000) {
				t.Fatalf("summary %s = %#v, want model cap 8000", maxField, requests[0].payload[maxField])
			}
			reloaded, err := session.OpenSessionManager(path, "", "")
			if err != nil {
				t.Fatal(err)
			}
			defer reloaded.Close()
			compactions := 0
			for _, entry := range reloaded.Entries() {
				if entry.Type() == "compaction" {
					compactions++
				}
			}
			if compactions != 1 {
				t.Fatalf("reloaded compactions = %d", compactions)
			}
		})
	}
}

func TestProductionSplitCompactionRunsOrderedDualSummaryAndReloads(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	var capture productionSummaryServer
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode split summary request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		capture.record(request, payload)
		encoded, _ := json.Marshal(payload)
		switch {
		case bytes.Contains(encoded, []byte("history-old")):
			writeProductionResponsesSSE(writer, "history summary")
		case bytes.Contains(encoded, []byte("turn-prefix")):
			writeProductionResponsesSSE(writer, "prefix summary")
		default:
			t.Errorf("unexpected split summary payload: %s", encoded)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	writeCompactionProductionConfig(t, agentDir, server.URL+"/v1", provider.OpenAIResponsesAPI, "split-key", "split-model")
	settings, err := json.Marshal(map[string]any{
		"defaultProvider": "openai", "defaultModel": "split-model", "defaultThinkingLevel": "off",
		"compaction": map[string]any{"enabled": true, "reserveTokens": 100, "keepRecentTokens": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := session.CreateSessionManager(cwd, t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"history-old", "turn-prefix"} {
		message, err := llm.NewUserTextMessage(text, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	block, err := llm.NewTextBlock("retained-assistant")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, time.Now(),
		llm.AssistantProvenance{Provider: "openai", API: provider.OpenAIResponsesAPI, Model: "split-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), assistant); err != nil {
		t.Fatal(err)
	}
	path, ok := manager.SessionFile()
	if !ok {
		t.Fatal("split session is not persistent")
	}
	deps, err := assembleProductionRuntime(context.Background(), fixedProductionConfig(cwd, agentDir, docsDir), options{modelID: "openai/split-model"})
	if err != nil {
		t.Fatal(err)
	}
	productRuntime, err := agentruntime.Create(context.Background(), deps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	result, err := productRuntime.Session().Compact(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := "history summary\n\n---\n\n**Turn Context (split turn):**\n\nprefix summary"
	if !result.Committed || !result.Input.IsSplitTurn || result.Output.Text != wantSummary || result.Output.Usage == nil ||
		result.Output.Usage.Usage.Input() != 6 || result.Output.Usage.Usage.Output() != 4 || result.Output.Usage.Usage.TotalTokens() != 10 {
		t.Fatalf("split compaction result = %#v", result)
	}
	requests := capture.snapshot()
	if len(requests) != 2 {
		t.Fatalf("split summary requests = %#v", requests)
	}
	for index, wantBudget := range []float64{80, 50} {
		if _, tools := requests[index].payload["tools"]; tools || requests[index].payload["max_output_tokens"] != wantBudget || requests[index].sessionID != "" {
			t.Fatalf("split request %d = %#v", index, requests[index])
		}
	}
	if err := productRuntime.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := session.OpenSessionManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	var compactions int
	for _, entry := range reloaded.Entries() {
		if entry.Type() == "compaction" {
			compactions++
			record, ok := entry.Compaction()
			if !ok || record.Summary != wantSummary || record.Usage == nil || record.Usage.Usage.TotalTokens() != 10 {
				t.Fatalf("reloaded split compaction = %#v", entry)
			}
		}
	}
	if compactions != 1 {
		t.Fatalf("reloaded split compactions = %d", compactions)
	}
}

func TestProductionCompactionRefreshesKeyModelAndThinkingPerSummary(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	capture := &productionSummaryServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { capture.serve(t, w, r) }))
	defer server.Close()
	writeCompactionProductionConfig(t, agentDir, server.URL+"/v1", provider.OpenAIResponsesAPI, "first-key", "first-model", "second-model")
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	productRuntime, manager := newPersistentProductionRuntime(t, cwd, agentDir, filepath.Join(t.TempDir(), "dynamic.jsonl"), "first-model", config)
	defer productRuntime.Dispose(context.Background())
	if _, err := productRuntime.Session().Compact(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := productRuntime.Services().AuthRuntime.SetAPIKey("openai", "second-key"); err != nil {
		t.Fatal(err)
	}
	var second model.Model
	for _, candidate := range productRuntime.Services().ModelRuntime.Snapshot().Models {
		if candidate.Provider == "openai" && candidate.ID == "second-model" {
			second = candidate
		}
	}
	secondRef, err := second.Ref()
	if err != nil {
		t.Fatal(err)
	}
	if err := productRuntime.Session().SetModelContext(context.Background(), secondRef); err != nil {
		t.Fatal(err)
	}
	if err := productRuntime.Session().SetThinkingLevel(provider.ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	appendProductionCompactionHistory(t, manager, "second")
	if _, err := productRuntime.Session().Compact(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	requests := capture.snapshot()
	if len(requests) != 2 || requests[0].authorization != "Bearer first-key" || requests[1].authorization != "Bearer second-key" ||
		requests[0].payload["model"] != "first-model" || requests[1].payload["model"] != "second-model" {
		t.Fatalf("dynamic summary requests = %#v", requests)
	}
	if requests[0].sessionID != "" || requests[1].sessionID != "" {
		t.Fatalf("summary operations transmitted session affinity: %q / %q", requests[0].sessionID, requests[1].sessionID)
	}
	firstReasoning, firstOK := requests[0].payload["reasoning"].(map[string]any)
	secondReasoning, secondOK := requests[1].payload["reasoning"].(map[string]any)
	if !firstOK || !secondOK || firstReasoning["effort"] != "low" || secondReasoning["effort"] != "high" {
		t.Fatalf("summary reasoning = %#v / %#v", requests[0].payload["reasoning"], requests[1].payload["reasoning"])
	}
}

func TestProductionThresholdCompactionRunsRealSummaryChain(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	capture := &productionSummaryServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { capture.serve(t, w, r) }))
	defer server.Close()
	writeCompactionProductionConfig(t, agentDir, server.URL+"/v1", provider.OpenAIResponsesAPI, "threshold-key", "threshold-model")
	settings, err := json.Marshal(map[string]any{
		"defaultProvider": "openai", "defaultModel": "threshold-model", "defaultThinkingLevel": "off",
		"compaction": map[string]any{"enabled": true, "reserveTokens": 99_999, "keepRecentTokens": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	productRuntime, manager := newPersistentProductionRuntime(t, cwd, agentDir, filepath.Join(t.TempDir(), "threshold.jsonl"), "threshold-model", config)
	defer productRuntime.Dispose(context.Background())
	var lifecycle []string
	unsubscribe := productRuntime.Session().Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if started, ok := event.(agent.CompactionStartEvent); ok {
			lifecycle = append(lifecycle, "start:"+started.Reason.String())
		}
		if ended, ok := event.(agent.CompactionEndEvent); ok {
			lifecycle = append(lifecycle, "end:"+ended.Reason.String())
		}
	})
	defer unsubscribe()
	if result, err := productRuntime.Session().Run(context.Background(), "cross threshold"); err != nil || !result.Succeeded() {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	requests := capture.snapshot()
	if len(requests) != 3 {
		t.Fatalf("provider/summary request count = %d, want 3", len(requests))
	}
	if _, agentTools := requests[0].payload["tools"]; !agentTools {
		t.Fatalf("agent request did not advertise production tools: %#v", requests[0].payload)
	}
	if _, summaryTools := requests[1].payload["tools"]; summaryTools {
		t.Fatalf("summary request advertised production tools: %#v", requests[1].payload)
	}
	if _, summaryTools := requests[2].payload["tools"]; summaryTools {
		t.Fatalf("turn-prefix summary request advertised production tools: %#v", requests[2].payload)
	}
	if strings.Join(lifecycle, ",") != "start:threshold,end:threshold" {
		t.Fatalf("threshold compaction lifecycle = %#v", lifecycle)
	}
	compactions := 0
	for _, entry := range manager.Entries() {
		if entry.Type() == "compaction" {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("threshold compactions = %d", compactions)
	}
}

func TestProductionRetryPolicyUsesSettingsPresenceAndPiDefaults(t *testing.T) {
	defaults, err := productionRetryPolicy(model.RetrySettings{})
	if err != nil || defaults.MaxAttempts != 4 || defaults.InitialDelay != 2*time.Second {
		t.Fatalf("default retry policy = %#v, %v", defaults, err)
	}
	disabled, zero := false, uint64(0)
	explicit, err := productionRetryPolicy(model.RetrySettings{Enabled: &disabled, MaxRetries: &zero, BaseDelayMS: &zero})
	if err != nil || explicit.MaxAttempts != 1 || explicit.InitialDelay != 0 {
		t.Fatalf("explicit retry policy = %#v, %v", explicit, err)
	}
}

func TestProductionCompactionPreservesExplicitZeroReserveAndKeep(t *testing.T) {
	for _, test := range []struct {
		api      string
		maxField string
	}{
		{api: provider.OpenAIResponsesAPI, maxField: "max_output_tokens"},
		{api: provider.OpenAICompletionsAPI, maxField: "max_completion_tokens"},
	} {
		t.Run(test.api, func(t *testing.T) {
			cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
			capture := &productionSummaryServer{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { capture.serve(t, w, r) }))
			defer server.Close()
			writeCompactionProductionConfig(t, agentDir, server.URL+"/v1", test.api, "zero-key", "zero-model")
			settings, err := json.Marshal(map[string]any{
				"defaultProvider": "openai", "defaultModel": "zero-model",
				"compaction": map[string]any{"enabled": true, "reserveTokens": 0, "keepRecentTokens": 0},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0o600); err != nil {
				t.Fatal(err)
			}
			config := fixedProductionConfig(cwd, agentDir, docsDir)
			productRuntime, _ := newPersistentProductionRuntime(t, cwd, agentDir, filepath.Join(t.TempDir(), "zero.jsonl"), "zero-model", config)
			defer productRuntime.Dispose(context.Background())
			if _, err := productRuntime.Session().Compact(context.Background(), "explicit zeros"); err != nil {
				t.Fatal(err)
			}
			requests := capture.snapshot()
			if len(requests) != 1 {
				t.Fatalf("explicit-zero summary request = %#v", requests)
			}
			if _, exists := requests[0].payload[test.maxField]; exists {
				t.Fatalf("explicit-zero summary request sent %s: %#v", test.maxField, requests[0].payload)
			}
			if result, err := productRuntime.Session().Run(context.Background(), "below full context window"); err != nil || !result.Succeeded() {
				t.Fatalf("Run() = %#v, %v", result, err)
			}
			if requests = capture.snapshot(); len(requests) != 2 {
				t.Fatalf("explicit reserve zero triggered automatic summary: requests=%d", len(requests))
			}
		})
	}
}

func TestProductionManualCompactionWithoutAuthFailsBeforeHTTPOrDurableMutation(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeProductionCatalog(t, agentDir, false)
	settings := []byte(`{"compaction":{"enabled":true,"keepRecentTokens":1}}`)
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	rejecting := &rejectingHTTPDoer{}
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	config.OpenAIHTTPClient = rejecting
	productRuntime, manager := newPersistentProductionRuntime(t, cwd, agentDir, filepath.Join(t.TempDir(), "no-auth.jsonl"), "gpt-5.5", config)
	defer productRuntime.Dispose(context.Background())
	var starts []agent.CompactionStartEvent
	var ends []agent.CompactionEndEvent
	productRuntime.Session().Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.CompactionStartEvent:
			starts = append(starts, value)
		case agent.CompactionEndEvent:
			ends = append(ends, value)
		}
	})
	_, err := productRuntime.Session().Compact(context.Background(), "must authenticate")
	want := agentruntime.FormatNoAPIKeyFoundMessage("openai", docsDir)
	if err == nil || err.Error() != want {
		t.Fatalf("Compact() error = %v, want %q", err, want)
	}
	if len(starts) != 1 || len(ends) != 1 || ends[0].Aborted || ends[0].Result != nil ||
		ends[0].ErrorMessage != "Compaction failed: "+want {
		t.Fatalf("manual no-auth lifecycle = %#v / %#v", starts, ends)
	}
	if rejecting.calls.Load() != 0 {
		t.Fatalf("no-auth compaction made %d HTTP requests", rejecting.calls.Load())
	}
	for _, entry := range manager.Entries() {
		if entry.Type() == "compaction" {
			t.Fatalf("no-auth compaction mutated durable history: %#v", entry)
		}
	}
}

func TestProductionContextOverflowRunsSummaryThenContinues(t *testing.T) {
	cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
	capture := &productionSummaryServer{}
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode overflow request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		capture.record(request, payload)
		switch calls.Add(1) {
		case 1:
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(writer, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"maximum context length exceeded"}}`)
		case 2:
			writeProductionResponsesSSE(writer, "overflow checkpoint")
		case 3:
			writeProductionResponsesSSE(writer, "overflow turn prefix")
		case 4:
			writeProductionResponsesSSE(writer, "recovered answer")
		default:
			t.Errorf("unexpected overflow request %d", calls.Load())
		}
	}))
	defer server.Close()
	writeCompactionProductionConfig(t, agentDir, server.URL+"/v1", provider.OpenAIResponsesAPI, "overflow-key", "overflow-model")
	config := fixedProductionConfig(cwd, agentDir, docsDir)
	productRuntime, manager := newPersistentProductionRuntime(t, cwd, agentDir, filepath.Join(t.TempDir(), "overflow.jsonl"), "overflow-model", config)
	defer productRuntime.Dispose(context.Background())
	durableSessionID := manager.SessionID()
	var lifecycle []agent.SessionEvent
	productRuntime.Session().Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type() == agent.CompactionStartEventType || event.Type() == agent.CompactionEndEventType {
			lifecycle = append(lifecycle, event)
		}
	})
	result, err := productRuntime.Session().Run(context.Background(), "overflow now")
	if err != nil || !result.Succeeded() || result.ProviderTurns() != 2 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	requests := capture.snapshot()
	if len(requests) != 4 || calls.Load() != 4 {
		t.Fatalf("overflow request count = %d/%d", len(requests), calls.Load())
	}
	if _, hasTools := requests[0].payload["tools"]; !hasTools {
		t.Fatalf("initial agent request omitted tools: %#v", requests[0].payload)
	}
	if _, hasTools := requests[1].payload["tools"]; hasTools {
		t.Fatalf("overflow summary advertised tools: %#v", requests[1].payload)
	}
	if _, hasTools := requests[2].payload["tools"]; hasTools {
		t.Fatalf("overflow turn-prefix summary advertised tools: %#v", requests[2].payload)
	}
	if _, hasTools := requests[3].payload["tools"]; !hasTools {
		t.Fatalf("continued agent request omitted tools: %#v", requests[3].payload)
	}
	if requests[0].sessionID != durableSessionID || requests[3].sessionID != durableSessionID || requests[1].sessionID != "" || requests[2].sessionID != "" {
		t.Fatalf("overflow session affinity = %q / %q / %q / %q, durable %q", requests[0].sessionID, requests[1].sessionID, requests[2].sessionID, requests[3].sessionID, durableSessionID)
	}
	if len(lifecycle) != 2 {
		t.Fatalf("overflow compaction lifecycle = %#v", lifecycle)
	}
	started, startOK := lifecycle[0].(agent.CompactionStartEvent)
	ended, endOK := lifecycle[1].(agent.CompactionEndEvent)
	if !startOK || !endOK || started.Reason != agent.CompactionContextOverflow || ended.Reason != agent.CompactionContextOverflow ||
		!started.WillRetry || !ended.WillRetry || ended.Aborted || ended.ErrorMessage != "" || ended.Result == nil {
		t.Fatalf("overflow compaction ordering/metadata = %#v", lifecycle)
	}
	compactions := 0
	for _, entry := range manager.Entries() {
		if entry.Type() == "compaction" {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("overflow compactions = %d", compactions)
	}
}
