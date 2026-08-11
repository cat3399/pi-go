package application

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
