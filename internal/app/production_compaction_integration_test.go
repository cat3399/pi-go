package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/testenv"
)

func TestProductionBranchSummaryUsesRealToolFreeHTTPProviderAndReloads(t *testing.T) {
	ctx := t.Context()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		messages, _ := json.Marshal(payload["messages"])
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer branch-key" || payload["model"] != "branch-model" || !bytes.Contains(messages, []byte("BRANCH_HTTP")) {
			t.Error("branch summary did not reach the configured model with the abandoned branch")
		}
		if _, tools := payload["tools"]; tools || payload["max_completion_tokens"] != float64(session.BranchSummaryMaxOutputTokens) {
			t.Error("branch summary used the wrong tools or token budget")
		}
		for _, key := range []string{"session_id", "x-client-request-id", "x-session-id"} {
			if r.Header.Get(key) != "" {
				t.Errorf("branch summary leaked affinity header %s", key)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"production summary\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	cwd, agentDir := t.TempDir(), testenv.AgentDir(t)
	models := fmt.Sprintf(`{"providers":{"openai":{"baseUrl":%q,"apiKey":"branch-key","models":[{"id":"branch-model","api":"openai-completions","compat":{"sendSessionAffinityHeaders":true,"sessionAffinityFormat":"openai","maxTokensField":"max_completion_tokens"}}]}}}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, "branch.jsonl")
	rt, err := OpenProductionRuntime(ctx, fixedProductionConfig(cwd, agentDir, t.TempDir()), ProductionRuntimeOptions{SessionPath: path, ModelID: "openai/branch-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Dispose(ctx)
	manager := rt.Session().SessionManager()
	appendUser := func(text string) session.Entry {
		t.Helper()
		msg, err := llm.NewUserTextMessage(text, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		entry, err := manager.AppendLLMMessage(ctx, msg)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	appendUser("root")
	reply, err := integrationTextTerminal("common answer")
	if err != nil {
		t.Fatal(err)
	}
	common, err := manager.AppendLLMMessage(ctx, reply)
	if err != nil {
		t.Fatal(err)
	}
	target := appendUser("target draft")
	if err := manager.Branch(common.ID()); err != nil {
		t.Fatal(err)
	}
	appendUser("abandoned branch BRANCH_HTTP")
	result, err := rt.Session().NavigateTree(ctx, target.ID(), agent.NavigateTreeOptions{Summarize: true})
	if err != nil || requests.Load() != 1 || result.SummaryEntry == nil || result.EditorText == nil || *result.EditorText != "target draft" {
		t.Fatalf("NavigateTree = %#v, %v (%d requests)", result, err, requests.Load())
	}
	if err := rt.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.OpenSessionManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entry, ok := reopened.Entry(result.SummaryEntry.ID())
	if !ok {
		t.Fatal("branch summary missing after reopen")
	}
	payload, ok := entry.Payload().(session.BranchSummaryPayload)
	if !ok || payload.FromID != common.ID() || !payload.HasFromHook || payload.FromHook || payload.Usage == nil || payload.Summary != session.BranchSummaryPreamble+"production summary" {
		t.Fatalf("reopened branch summary = %#v", entry.Payload())
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
