package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func modelConfigurationServer(t *testing.T) *Server {
	t.Helper()
	cwd := t.TempDir()
	service := testService(t, app.ProductionConfig{
		WorkingDir: cwd, AgentDir: filepath.Join(t.TempDir(), "agent"), Environment: []string{},
	}, nil)
	server, err := New(Options{Version: "test", Application: service})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func serveModelRequest(t *testing.T, server *Server, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.Host = "localhost"
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestAPIKeyAndAvailableModelsContract(t *testing.T) {
	server := modelConfigurationServer(t)
	providers := serveModelRequest(t, server, http.MethodGet, "/api/v1/auth/all-providers", nil)
	if providers.Code != http.StatusOK || !strings.Contains(providers.Body.String(), `"id":"deepseek"`) {
		t.Fatalf("providers response = %d %s", providers.Code, providers.Body.String())
	}

	const key = "contract-test-deepseek-key"
	saved := serveModelRequest(t, server, http.MethodPost, "/api/v1/auth/api-key/deepseek", []byte(`{"apiKey":"`+key+`"}`))
	if saved.Code != http.StatusOK || strings.Contains(saved.Body.String(), key) {
		t.Fatalf("save response = %d %s", saved.Code, saved.Body.String())
	}

	models := serveModelRequest(t, server, http.MethodGet, "/api/v1/models", nil)
	if models.Code != http.StatusOK {
		t.Fatalf("models response = %d %s", models.Code, models.Body.String())
	}
	var modelBody struct {
		ModelList []struct {
			Provider string `json:"provider"`
		} `json:"modelList"`
	}
	if err := json.Unmarshal(models.Body.Bytes(), &modelBody); err != nil {
		t.Fatal(err)
	}
	if len(modelBody.ModelList) == 0 {
		t.Fatal("configured provider has no available models")
	}
	for _, candidate := range modelBody.ModelList {
		if candidate.Provider != "deepseek" {
			t.Fatalf("unconfigured model provider returned: %q", candidate.Provider)
		}
	}

	removed := serveModelRequest(t, server, http.MethodDelete, "/api/v1/auth/api-key/deepseek", nil)
	if removed.Code != http.StatusOK {
		t.Fatalf("delete response = %d %s", removed.Code, removed.Body.String())
	}
	empty := serveModelRequest(t, server, http.MethodGet, "/api/v1/models", nil)
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"modelList":[]`) {
		t.Fatalf("models after delete = %d %s", empty.Code, empty.Body.String())
	}
}

func TestModelsConfigHTTPContract(t *testing.T) {
	server := modelConfigurationServer(t)
	initial := serveModelRequest(t, server, http.MethodGet, "/api/v1/models-config", nil)
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), `"providers":{}`) {
		t.Fatalf("initial config = %d %s", initial.Code, initial.Body.String())
	}

	config := []byte(`{"providers":{"local":{"api":"openai-completions","models":[]}},"future":{"keep":true}}`)
	saved := serveModelRequest(t, server, http.MethodPut, "/api/v1/models-config", config)
	if saved.Code != http.StatusOK {
		t.Fatalf("save config = %d %s", saved.Code, saved.Body.String())
	}
	loaded := serveModelRequest(t, server, http.MethodGet, "/api/v1/models-config", nil)
	if loaded.Code != http.StatusOK || !strings.Contains(loaded.Body.String(), `"future":{"keep":true}`) {
		t.Fatalf("loaded config = %d %s", loaded.Code, loaded.Body.String())
	}

	bad := serveModelRequest(t, server, http.MethodPut, "/api/v1/models-config", []byte(`{"providers":[]}`))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid config = %d %s", bad.Code, bad.Body.String())
	}
}

func TestDeleteAbsentAPIKeyIsIdempotent(t *testing.T) {
	server := modelConfigurationServer(t)
	response := serveModelRequest(t, server, http.MethodDelete, "/api/v1/auth/api-key/deepseek", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("deleting an absent key must be idempotent: %d %s", response.Code, response.Body.String())
	}
}
