package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	coderwebsocket "github.com/coder/websocket"
)

func diagnosticAdapter(t *testing.T, api string, client provider.HTTPDoer) (provider.Streamer, provider.Model) {
	t.Helper()
	model, err := newModel(provider.ModelSpec{Provider: "fixture", API: api, ID: "diagnostic-model"})
	if err != nil {
		t.Fatal(err)
	}
	var implementation provider.Streamer
	switch api {
	case provider.OpenAIResponsesAPI:
		implementation, err = provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{Client: client, APIKey: "fixture-key"})
	case provider.OpenAICompletionsAPI:
		implementation, err = provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{Client: client, APIKey: "fixture-key"})
	case provider.AnthropicMessagesAPI:
		implementation, err = provider.NewAnthropicProvider(provider.AnthropicConfig{Client: client, APIKey: "fixture-key"})
	}
	if err != nil {
		t.Fatal(err)
	}
	return implementation, model
}

func diagnosticRequest(t *testing.T, model provider.Model, options provider.StreamOptions) provider.Request {
	t.Helper()
	request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "hello")}, provider.RequestOptions{Stream: options})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func diagnosticSuccess(api string) string {
	switch api {
	case provider.OpenAICompletionsAPI:
		return "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	case provider.AnthropicMessagesAPI:
		return "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	default:
		return "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"
	}
}

func TestFailedResponsesRetainOriginalBytes(t *testing.T) {
	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI, provider.AnthropicMessagesAPI} {
		for _, scenario := range []struct {
			name, contentType, body string
			status                  int
		}{
			{"invalid UTF-8 JSON", "text/event-stream", "data: {\"broken\":\xff}\n\n", 200},
			{"incomplete frame", "text/event-stream", "data: {\"broken\":", 200},
			{"large HTTP error", "text/html", "<html>" + strings.Repeat("gateway failure ", 20_000) + "</html>", 503},
			{"wrong content type", "text/html", "<html>unexpected gateway response</html>", 200},
		} {
			t.Run(api+"/"+scenario.name, func(t *testing.T) {
				dir := t.TempDir()
				var sent []byte
				implementation, model := diagnosticAdapter(t, api, responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
					sent, _ = io.ReadAll(request.Body)
					response := responsesHTTPResponse(scenario.status, scenario.contentType, scenario.body)
					response.Header.Set("x-request-id", "request-fixture")
					return response, nil
				}))
				terminal, err := provider.Complete(context.Background(), implementation, diagnosticRequest(t, model, provider.StreamOptions{DiagnosticsDir: dir, SessionID: "session-fixture"}))
				if err != nil || terminal.FinishReason() != llm.FinishError {
					t.Fatalf("terminal=%v err=%v", terminal, err)
				}
				if len(terminal.Diagnostics()) != 1 {
					t.Fatalf("diagnostics=%v", terminal.Diagnostics())
				}
				var details struct{ RecordPath, ResponsePath string }
				if err := json.Unmarshal(terminal.Diagnostics()[0].Details(), &details); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(details.ResponsePath)
				if err != nil || !bytes.Equal(raw, []byte(scenario.body)) {
					t.Fatalf("original response changed: got %d bytes want %d err=%v", len(raw), len(scenario.body), err)
				}
				metadata, err := os.ReadFile(details.RecordPath)
				if err != nil {
					t.Fatal(err)
				}
				var record struct {
					SessionID, RequestID, Cause string
					Request                     []byte `json:"requestBase64"`
					HTTPStatus                  int
				}
				if err := json.Unmarshal(metadata, &record); err != nil {
					t.Fatal(err)
				}
				if record.SessionID != "session-fixture" || record.RequestID != "request-fixture" || record.Cause == "" || record.HTTPStatus != scenario.status || !bytes.Equal(record.Request, sent) {
					t.Fatalf("failure metadata=%+v", record)
				}
			})
		}
	}
}

func TestProviderRetryKeepsFailedCaptureAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	const original = `{"error":{"message":"gateway temporarily unavailable"},"metadata":{"raw":"extra details"}}`
	implementation, model := diagnosticAdapter(t, provider.OpenAIResponsesAPI, responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			response := responsesHTTPResponse(503, "application/json", original)
			response.Header.Set("retry-after-ms", "0")
			return response, nil
		}
		return responsesHTTPResponse(200, "text/event-stream", diagnosticSuccess(provider.OpenAIResponsesAPI)), nil
	}))
	retries := uint32(1)
	terminal, err := provider.Complete(context.Background(), implementation, diagnosticRequest(t, model, provider.StreamOptions{DiagnosticsDir: dir, MaxRetries: &retries}))
	if err != nil || terminal.FinishReason() != llm.FinishStop || calls != 2 {
		t.Fatalf("terminal=%v calls=%d err=%v", terminal, calls, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("diagnostic files=%v err=%v", files, err)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.response"))
	raw, err := os.ReadFile(paths[0])
	if err != nil || string(raw) != original {
		t.Fatalf("failed attempt evidence=%q err=%v", raw, err)
	}
}

func TestRequestTimeoutRetriesButCallerCancellationDoesNot(t *testing.T) {
	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI, provider.AnthropicMessagesAPI} {
		t.Run(api, func(t *testing.T) {
			calls := 0
			implementation, model := diagnosticAdapter(t, api, responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				return responsesHTTPResponse(200, "text/event-stream", diagnosticSuccess(api)), nil
			}))
			timeout := uint64(5)
			request := diagnosticRequest(t, model, provider.StreamOptions{TimeoutMS: &timeout})
			retry, _ := provider.NewRetryController(provider.RetryPolicy{MaxAttempts: 2})
			terminal, err := retry.Call(context.Background(), func() (llm.AssistantTerminal, error) {
				return provider.Complete(context.Background(), implementation, request)
			})
			if err != nil || terminal.FinishReason() != llm.FinishStop || calls != 2 {
				t.Fatalf("timeout recovery terminal=%v calls=%d err=%v", terminal, calls, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			terminal, err = provider.Complete(ctx, implementation, request)
			if err != nil || terminal.FinishReason() != llm.FinishAborted || provider.IsRetryableAssistantError(terminal) || calls != 2 {
				t.Fatalf("cancellation terminal=%v calls=%d err=%v", terminal, calls, err)
			}
		})
	}
}

func TestRetryExcludesPermanentProviderLimits(t *testing.T) {
	for _, scenario := range []struct {
		kind    provider.FailureKind
		code    string
		message string
		want    bool
	}{
		{provider.FailureInvalidResponse, "server_error", "please retry your request", true},
		{provider.FailureInvalidResponse, "overloaded_error", "overloaded", true},
		{provider.FailureInvalidResponse, "", "invalid JSON", true},
		{provider.FailureInvalidResponse, "invalid_api_key", "invalid credential", false},
		{provider.FailureInvalidResponse, "invalid_request_error", "invalid parameter", false},
		{provider.FailureHTTPStatus, "insufficient_quota", "add credit", false},
		{provider.FailureHTTPStatus, "", "Monthly usage limit reached", false},
	} {
		status := 429
		failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{Kind: scenario.kind, Message: scenario.message, Cause: errors.New(scenario.message), VendorCode: scenario.code, HTTPStatus: &status})
		if err != nil {
			t.Fatal(err)
		}
		if got := provider.IsTransientFailure(failure); got != scenario.want {
			t.Errorf("%s/%s retry=%t want=%t", scenario.code, scenario.message, got, scenario.want)
		}
	}
}

func TestDiagnosticWriteFailureIsVisible(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	implementation, model := diagnosticAdapter(t, provider.OpenAIResponsesAPI, responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		return responsesHTTPResponse(200, "text/event-stream", "data: invalid\n\n"), nil
	}))
	terminal, err := provider.Complete(context.Background(), implementation, diagnosticRequest(t, model, provider.StreamOptions{DiagnosticsDir: dir}))
	if err != nil || terminal.FinishReason() != llm.FinishError || len(terminal.Diagnostics()) != 1 {
		t.Fatalf("terminal=%v err=%v", terminal, err)
	}
	if !strings.Contains(string(terminal.Diagnostics()[0].Details()), "recordingError") {
		t.Fatal("diagnostic storage failure was hidden")
	}
}

func TestCodexDiagnosticRetainsMalformedWebSocketFrame(t *testing.T) {
	dir := t.TempDir()
	original := []byte("{\"type\":\"response.created\", invalid }")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := coderwebsocket.Accept(w, r, &coderwebsocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.CloseNow()
		if _, _, err := connection.Read(r.Context()); err != nil {
			t.Error(err)
			return
		}
		if err := connection.Write(r.Context(), coderwebsocket.MessageText, original); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		BaseURL: server.URL, AccessToken: codexTestToken(t, "acct-test"), AccountID: "acct-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := diagnosticRequest(t, mustCodexModel(t, server.URL), provider.StreamOptions{DiagnosticsDir: dir, Transport: provider.TransportWebsocket})
	terminal, err := provider.Complete(context.Background(), implementation, request)
	if err != nil || terminal.FinishReason() != llm.FinishError || len(terminal.Diagnostics()) != 1 {
		t.Fatalf("terminal=%v err=%v", terminal, err)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.response"))
	if len(paths) != 1 {
		t.Fatalf("response records=%v", paths)
	}
	raw, err := os.ReadFile(paths[0])
	var frame struct{ Data []byte }
	if err != nil || json.Unmarshal(raw, &frame) != nil || !bytes.Equal(frame.Data, original) {
		t.Fatalf("original WebSocket frame lost: %q err=%v", raw, err)
	}
}

func TestCodexDiagnosticRetainsRejectedHandshakeBody(t *testing.T) {
	dir := t.TempDir()
	original := strings.Repeat("upstream handshake failure ", 200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, original)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, diagnosticSuccess(provider.OpenAIResponsesAPI))
	}))
	defer server.Close()
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		BaseURL: server.URL, AccessToken: codexTestToken(t, "acct-test"), AccountID: "acct-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := diagnosticRequest(t, mustCodexModel(t, server.URL), provider.StreamOptions{DiagnosticsDir: dir, Transport: provider.TransportWebsocket})
	terminal, err := provider.Complete(context.Background(), implementation, request)
	if err != nil || terminal.FinishReason() != llm.FinishStop {
		t.Fatalf("fallback terminal=%v err=%v", terminal, err)
	}
	if len(terminal.Diagnostics()) != 1 || !strings.Contains(string(terminal.Diagnostics()[0].Details()), "responsePath") {
		t.Fatalf("fallback lost evidence reference: %v", terminal.Diagnostics())
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.response"))
	if len(paths) != 1 {
		t.Fatalf("handshake records=%v", paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil || string(raw) != original {
		t.Fatalf("handshake body lost: %d bytes want=%d err=%v", len(raw), len(original), err)
	}
}

// Keep the retry timing contract explicit: these are delays before the three
// additional attempts, not a second provider-internal retry budget.
func TestModelRetryDefaultBackoff(t *testing.T) {
	retry, _ := provider.NewRetryController(provider.RetryPolicy{MaxAttempts: 4, InitialDelay: 2 * time.Second})
	for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := retry.Delay(uint32(attempt+2), nil); got != want {
			t.Fatalf("retry %d delay=%v want=%v", attempt+1, got, want)
		}
	}
}
