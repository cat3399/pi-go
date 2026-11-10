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

func TestProductionBranchSummaryUsesRealToolFreeHTTPProviderAndReloads(t *testing.T) {
	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI} {
		t.Run(api, func(t *testing.T) {
			cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
			capture := &productionSummaryServer{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { capture.serve(t, w, r) }))
			defer server.Close()
			writeCompactionProductionConfig(t, agentDir, server.URL+"/v1", api, "branch-key", "branch-model")
			path := filepath.Join(t.TempDir(), "branch-session.jsonl")
			manager, err := session.OpenSessionManager(path, "", cwd)
			if err != nil {
				t.Fatal(err)
			}
			appendUser := func(text string) session.Entry {
				message, createErr := llm.NewUserTextMessage(text, time.Now())
				if createErr != nil {
					t.Fatal(createErr)
				}
				entry, appendErr := manager.AppendLLMMessage(context.Background(), message)
				if appendErr != nil {
					t.Fatal(appendErr)
				}
				return entry
			}
			appendAssistant := func(text string) session.Entry {
				block, createErr := llm.NewTextBlock(text)
				if createErr != nil {
					t.Fatal(createErr)
				}
				message, createErr := llm.NewAssistantTextMessage(
					[]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, time.Now(),
					llm.AssistantProvenance{Provider: "openai", API: api, Model: "branch-model"},
				)
				if createErr != nil {
					t.Fatal(createErr)
				}
				entry, appendErr := manager.AppendLLMMessage(context.Background(), message)
				if appendErr != nil {
					t.Fatal(appendErr)
				}
				return entry
			}
			appendUser("root request")
			common := appendAssistant("common answer")
			target := appendUser("target draft")
			if err := manager.Branch(common.ID()); err != nil {
				t.Fatal(err)
			}
			appendUser("abandoned branch marker BRANCH_HTTP")
			appendAssistant("alternate branch result")

			deps, err := assembleProductionRuntime(context.Background(), fixedProductionConfig(cwd, agentDir, docsDir), options{modelID: "openai/branch-model"})
			if err != nil {
				t.Fatal(err)
			}
			productRuntime, err := agentruntime.Create(context.Background(), deps.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
			if err != nil {
				t.Fatal(err)
			}
			result, err := productRuntime.Session().NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.EditorText == nil || *result.EditorText != "target draft" || result.SummaryEntry == nil {
				t.Fatalf("NavigateTree()=%#v", result)
			}
			payload, ok := result.SummaryEntry.Payload().(session.BranchSummaryPayload)
			if !ok || payload.FromID != common.ID() || payload.FromHook || !payload.HasFromHook || payload.Usage == nil || payload.Summary != session.BranchSummaryPreamble+"production summary" {
				t.Fatalf("branch summary payload=%#v", result.SummaryEntry.Payload())
			}
			if err := productRuntime.Dispose(context.Background()); err != nil {
				t.Fatal(err)
			}

			requests := capture.snapshot()
			if len(requests) != 1 || requests[0].authorization != "Bearer branch-key" || requests[0].payload["model"] != "branch-model" {
				t.Fatalf("branch summary requests=%#v", requests)
			}
			encoded, err := json.Marshal(requests[0].payload)
			if err != nil || !bytes.Contains(encoded, []byte("BRANCH_HTTP")) {
				t.Fatalf("branch summary prompt=%s, err=%v", encoded, err)
			}
			if _, exists := requests[0].payload["tools"]; exists || requests[0].sessionID != "" {
				t.Fatalf("branch summary leaked tools/session affinity: %#v", requests[0])
			}
			wantPath, maxField := "/v1/responses", "max_output_tokens"
			if api == provider.OpenAICompletionsAPI {
				wantPath, maxField = "/v1/chat/completions", "max_completion_tokens"
			}
			if requests[0].path != wantPath || requests[0].payload[maxField] != float64(session.BranchSummaryMaxOutputTokens) {
				t.Fatalf("branch request path/budget=%q/%#v", requests[0].path, requests[0].payload[maxField])
			}
			reloaded, err := session.OpenSessionManager(path, "", "")
			if err != nil {
				t.Fatal(err)
			}
			defer reloaded.Close()
			persisted, ok := reloaded.Entry(result.SummaryEntry.ID())
			if !ok {
				t.Fatal("branch summary missing after reopen")
			}
			persistedPayload, ok := persisted.Payload().(session.BranchSummaryPayload)
			if !ok || persistedPayload.Summary != payload.Summary || persistedPayload.Usage == nil {
				t.Fatalf("reopened branch summary=%#v", persisted.Payload())
			}
		})
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
	if len(starts) != 0 || len(ends) != 0 {
		t.Fatalf("admission failure emitted compaction lifecycle: %#v / %#v", starts, ends)
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
