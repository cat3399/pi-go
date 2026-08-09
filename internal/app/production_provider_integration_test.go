package app_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/provider"
	coderwebsocket "github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestRunProductionRoutesAnthropicWithRequestTimeHeaders(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/messages" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "anthropic-configured" || request.Header.Get("X-Provider") != "provider-env" || request.Header.Get("X-Model") != "model-env" {
			t.Errorf("auth/config headers = %#v", request.Header)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeProviderSSE(t, writer,
			map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-production-anthropic", "usage": map[string]any{"input_tokens": 3}}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "anthropic answer"}},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 2}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{
		"anthropic": map[string]any{
			"baseUrl": server.URL, "apiKey": "anthropic-${KEY}", "headers": map[string]string{"X-Provider": "$PROVIDER_HEADER"},
			"modelOverrides": map[string]any{"claude-opus-4-8": map[string]any{"headers": map[string]string{"X-Model": "$MODEL_HEADER"}}},
		},
	})
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"defaultThinkingLevel":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := productionTestConfig(workingDir, agentDir, []string{"KEY=configured", "PROVIDER_HEADER=provider-env", "MODEL_HEADER=model-env"})
	config.AnthropicClock = func() time.Time { return productionTestTime }
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "anthropic/claude-opus-4-8", "--session", filepath.Join(workingDir, "anthropic.jsonl"), "-p", "hello",
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "anthropic answer\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if captured["model"] != "claude-opus-4-8" || captured["stream"] != true {
		t.Fatalf("payload = %#v", captured)
	}
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("portable thinking off payload = %#v, want thinking.disabled", captured["thinking"])
	}
}

func TestRunProductionRoutesModelsJSONOnlyOpenAICompatibleProvider(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"compatible answer\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{
		"deepseek": map[string]any{
			"baseUrl": server.URL + "/v1", "api": "openai-completions", "apiKey": "deepseek-key",
			"models": []map[string]any{{"id": "deepseek-v4flsh"}},
		},
	})
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "deepseek/deepseek-v4flsh", "--session", filepath.Join(workingDir, "compatible.jsonl"), "-p", "hello",
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "compatible answer\n" || stderr.Len() != 0 || authorization != "Bearer deepseek-key" {
		t.Fatalf("RunProduction = %d stdout=%q stderr=%q authorization=%q", code, stdout.String(), stderr.String(), authorization)
	}
}

func TestRunProductionRoutesHeaderOwnedOpenAICompatibleProvider(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		if got := request.Header.Get("cf-aig-authorization"); got != "Bearer gateway-token" {
			t.Errorf("cf-aig-authorization = %q", got)
		}
		if values := request.Header.Values("X-Empty"); len(values) != 1 || values[0] != "" {
			t.Errorf("empty header values = %#v", values)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"header answer\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{
		"gateway": map[string]any{
			"baseUrl": server.URL + "/v1", "api": "openai-completions",
			"headers": map[string]string{"cf-aig-authorization": "Bearer gateway-token", "X-Empty": ""},
			"models":  []map[string]any{{"id": "gateway-model"}},
		},
	})
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "gateway/gateway-model", "--session", filepath.Join(workingDir, "header-owned.jsonl"), "-p", "hello",
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "header answer\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunProductionSelectsHeaderOwnedModelWithoutExplicitSelection(t *testing.T) {
	for _, headerOwner := range []string{"provider", "model"} {
		t.Run(headerOwner, func(t *testing.T) {
			workingDir, agentDir := t.TempDir(), t.TempDir()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/chat/completions" {
					t.Errorf("request path = %q", request.URL.Path)
				}
				if got := request.Header.Get("cf-aig-authorization"); got != "Bearer gateway-token" {
					t.Errorf("cf-aig-authorization = %q", got)
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"default header answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			providerConfig := map[string]any{
				"baseUrl": server.URL + "/v1", "api": "openai-completions",
				"models": []map[string]any{{"id": "gateway-default"}},
			}
			if headerOwner == "provider" {
				providerConfig["headers"] = map[string]string{"cf-aig-authorization": "Bearer gateway-token"}
			} else {
				providerConfig["models"] = []map[string]any{{
					"id": "gateway-default", "headers": map[string]string{"cf-aig-authorization": "Bearer gateway-token"},
				}}
			}
			writeProviderModelsJSON(t, agentDir, map[string]any{"gateway": providerConfig})
			config := productionTestConfig(workingDir, agentDir, nil)
			var stdout, stderr bytes.Buffer
			code := app.RunProduction(context.Background(), config, []string{
				"--session", filepath.Join(workingDir, "header-default.jsonl"), "-p", "hello",
			}, &stdout, &stderr)
			if code != app.ExitSuccess || stdout.String() != "default header answer\n" || stderr.Len() != 0 {
				t.Fatalf("RunProduction = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunProductionMergesStoredCacheRetentionEnvWithoutProjectingOtherEnv(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer stored-key" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeProviderSSE(t, writer, map[string]any{
			"type": "response.completed", "response": map[string]any{
				"status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 0, "total_tokens": 1},
			},
		})
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{
		"openai": map[string]any{"baseUrl": server.URL + "/v1"},
	})
	writeAuthJSON(t, agentDir, `{"openai":{"type":"api_key","key":"stored-key","env":{"PI_CACHE_RETENTION":"long","UNRELATED_SECRET":"must-not-leak"}}}`)
	config := productionTestConfig(workingDir, agentDir, []string{"PI_CACHE_RETENTION=short", "UNRELATED_SECRET=ambient-secret"})
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/gpt-5.5", "--session", filepath.Join(workingDir, "stored-env.jsonl"), "-p", "hello",
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "" || stderr.Len() != 0 {
		t.Fatalf("RunProduction = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if captured["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt_cache_retention = %#v", captured["prompt_cache_retention"])
	}
}

func TestRunProductionForwardsProviderRetrySettingsOnEachTurn(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempt := attempts.Add(1); attempt == 1 {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(writer, `{"error":{"message":"retry","code":"rate_limit_exceeded"}}`)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeProviderSSE(t, writer,
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{
				"type": "message", "id": "msg-retry", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "retried"}},
			}},
			map[string]any{"type": "response.completed", "response": map[string]any{
				"status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			}},
		)
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{
		"openai": map[string]any{"baseUrl": server.URL + "/v1", "apiKey": "configured-key"},
	})
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"httpIdleTimeoutMs":1234,"websocketConnectTimeoutMs":0,"retry":{"provider":{"timeoutMs":2345,"maxRetries":1,"maxRetryDelayMs":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/gpt-5.5", "--session", filepath.Join(workingDir, "provider-retry.jsonl"), "-p", "hello",
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "retried\n" || stderr.Len() != 0 || attempts.Load() != 2 {
		t.Fatalf("RunProduction = %d stdout=%q stderr=%q attempts=%d", code, stdout.String(), stderr.String(), attempts.Load())
	}
}

func TestRunProductionRoutesIndependentOpenAICodexOAuthOverWebSocket(t *testing.T) {
	provider.ResetOpenAICodexWebSocketDebugStats("")
	defer provider.CloseOpenAICodexWebSocketSessions("")
	workingDir, agentDir := t.TempDir(), t.TempDir()
	token := productionOAuthJWT("codex-production-account")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/codex/responses" || !strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			t.Errorf("request = %s %s upgrade=%q", request.Method, request.URL.Path, request.Header.Get("Upgrade"))
			http.Error(writer, "websocket required", http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+token || request.Header.Get("chatgpt-account-id") != "codex-production-account" {
			t.Errorf("Codex identity headers = %#v", request.Header)
		}
		connection, err := coderwebsocket.Accept(writer, request, &coderwebsocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer connection.CloseNow()
		var frame map[string]any
		if err := wsjson.Read(request.Context(), connection, &frame); err != nil {
			t.Errorf("read response.create: %v", err)
			return
		}
		if frame["type"] != "response.create" || frame["model"] != "gpt-5.5" {
			t.Errorf("response.create = %#v", frame)
		}
		item := map[string]any{
			"type": "message", "id": "msg-production-codex", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "codex answer"}},
		}
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-production"}},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{
				"id": "resp-production", "status": "completed", "output": []any{item},
				"usage": map[string]any{"input_tokens": 2, "output_tokens": 2, "total_tokens": 4},
			}},
		} {
			if err := wsjson.Write(request.Context(), connection, event); err != nil {
				t.Errorf("write event: %v", err)
				return
			}
		}
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{"openai-codex": map[string]any{"baseUrl": server.URL}})
	writeAuthJSON(t, agentDir, fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":%q,"refresh":"refresh","expires":%d,"accountId":"codex-production-account"}}`, token, time.Now().Add(time.Hour).UnixMilli()))
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "openai-codex/gpt-5.5", "--session", filepath.Join(workingDir, "codex.jsonl"), "-p", "hello",
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "codex answer\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func writeProviderModelsJSON(t *testing.T, agentDir string, providers map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(map[string]any{"providers": providers}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeProviderSSE(t *testing.T, writer http.ResponseWriter, events ...map[string]any) {
	t.Helper()
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", data); err != nil {
			t.Errorf("write SSE: %v", err)
			return
		}
	}
}
