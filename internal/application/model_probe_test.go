package application

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestModelProbeUsesUnsavedDraftThroughProductionAdapter(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"id\":\"chat-probe\",\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(writer, "data: {\"id\":\"chat-probe\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	cwd := t.TempDir()
	service, err := NewService(ServiceOptions{
		Production: app.ProductionConfig{
			WorkingDir: cwd, AgentDir: filepath.Join(t.TempDir(), "agent"), Environment: []string{},
			OpenAIHTTPClient: server.Client(),
		},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	providerDraft := []byte(`{"baseUrl":"` + server.URL + `/v1","api":"openai-completions","apiKey":"fixture-secret"}`)
	modelDraft := []byte(`{"id":"probe-model","name":"Probe Model"}`)
	result, err := service.TestModel(context.Background(), "fixture", providerDraft, modelDraft)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != http.StatusOK || result.ResponseText != "OK" || authorization != "Bearer fixture-secret" {
		t.Fatalf("probe = %#v, auth present = %v", result, authorization != "")
	}
}

func TestModelProbeRetriesAndRetainsFailedResponse(t *testing.T) {
	var calls atomic.Int32
	const original = "data: invalid-json\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, original)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"retry":{"maxRetries":1,"baseDelayMs":0}}`), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{Production: app.ProductionConfig{
		WorkingDir: t.TempDir(), AgentDir: agentDir, Environment: []string{}, OpenAIHTTPClient: server.Client(),
	}, DisableReaper: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	providerDraft := []byte(`{"baseUrl":"` + server.URL + `/v1","api":"openai-completions","apiKey":"fixture-secret"}`)
	result, err := service.TestModel(context.Background(), "fixture", providerDraft, []byte(`{"id":"probe-model","name":"Probe Model"}`))
	if err != nil || result.ResponseText != "OK" || calls.Load() != 2 {
		t.Fatalf("probe=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	paths, _ := filepath.Glob(filepath.Join(agentDir, "diagnostics", "providers", "*.response"))
	if len(paths) != 1 {
		t.Fatalf("failed probe records=%v", paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil || string(raw) != original {
		t.Fatalf("original probe response=%q err=%v", raw, err)
	}
}
