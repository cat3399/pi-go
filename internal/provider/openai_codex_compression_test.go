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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/klauspost/compress/zstd"
)

const codexBareBadRequest = `{"detail":"Bad Request"}`

func TestOpenAICodexCompressionFallbackPreservesWireBodyAndResetsNextCall(t *testing.T) {
	for _, transport := range []provider.Transport{provider.TransportSSE, provider.TransportAuto} {
		t.Run(string(transport), func(t *testing.T) {
			provider.ResetOpenAICodexWebSocketDebugStats("")
			defer provider.CloseOpenAICodexWebSocketSessions("")
			token := codexTestToken(t, "acct-compression")
			var mu sync.Mutex
			var bodies [][]byte
			var encodings []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					http.Error(w, "websocket unavailable", http.StatusServiceUnavailable)
					return
				}
				wire, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
				}
				if r.ContentLength != int64(len(wire)) || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Session-Id") != "compression-session" || r.Header.Get("X-Hook") != "kept" {
					t.Errorf("request length/headers = %d/%d/%v", r.ContentLength, len(wire), r.Header)
				}
				mu.Lock()
				bodies = append(bodies, wire)
				encodings = append(encodings, r.Header.Get("Content-Encoding"))
				attempt := len(bodies)
				mu.Unlock()
				if attempt == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, " { \n \"detail\" : \"Bad Request\" }\n")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, codexCompressionSuccess())
			}))
			defer server.Close()
			implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{BaseURL: server.URL, AccessToken: token})
			if err != nil {
				t.Fatal(err)
			}
			payloadCalls := 0
			var statuses []int
			noRetries := uint32(0)
			timeout := uint64(1_000)
			request, err := provider.NewRequestWithOptions(mustCodexModel(t, server.URL), "system", []llm.ConversationMessage{mustUser(t, strings.Repeat("重复文本 ", 12_000))}, provider.RequestOptions{Stream: provider.StreamOptions{
				Transport: transport, SessionID: "compression-session", MaxRetries: &noRetries, TimeoutMS: &timeout,
				OnPayload: func(_ provider.Model, payload []byte) ([]byte, error) {
					payloadCalls++
					return append(append([]byte(" \n"), payload...), '\n'), nil
				},
				OnHeaders: func(_ provider.Model, headers map[string]*string) error {
					value := "kept"
					headers["X-Hook"] = &value
					return nil
				},
				OnResponse: func(_ provider.Model, response provider.ResponseInfo) error {
					statuses = append(statuses, response.StatusCode)
					return nil
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			for turn := 0; turn < 2; turn++ {
				events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
				if !reflect.DeepEqual(eventKinds(events), []string{"start", "text_start", "text_delta", "text_end", "done"}) || terminalText(t, terminal) != "ok" {
					t.Fatalf("turn %d events/terminal = %v/%v", turn, eventKinds(events), terminal)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(encodings, []string{"zstd", "", "zstd"}) || !reflect.DeepEqual(statuses, []int{400, 200, 200}) || payloadCalls != 2 {
				t.Fatalf("encodings/statuses/payload hooks = %v/%v/%d", encodings, statuses, payloadCalls)
			}
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer decoder.Close()
			for _, index := range []int{0, 2} {
				decoded, err := decoder.DecodeAll(bodies[index], nil)
				if err != nil || !bytes.Equal(decoded, bodies[1]) || !bytes.HasPrefix(decoded, []byte(" \n")) {
					t.Fatalf("attempt %d did not preserve the exact post-hook JSON: %v", index+1, err)
				}
			}
		})
	}
}

func TestOpenAICodexCompressionFallbackRejectsOtherFailures(t *testing.T) {
	identity := "identity"
	for _, test := range []struct {
		name      string
		status    int
		body      string
		limit     int
		readErr   error
		overrides map[string]*string
	}{
		{name: "structured error", status: 400, body: `{"error":{"message":"Bad Request","code":"invalid_request_error"}}`},
		{name: "extra field", status: 400, body: `{"detail":"Bad Request","code":"invalid_request"}`},
		{name: "duplicate field", status: 400, body: `{"detail":"Bad Request","detail":"Bad Request"}`},
		{name: "different detail", status: 400, body: `{"detail":"bad request"}`},
		{name: "invalid zstd", status: 400, body: `{"detail":"invalid zstd request body"}`},
		{name: "trailing data", status: 400, body: codexBareBadRequest + `{}`},
		{name: "truncated body", status: 400, body: codexBareBadRequest + "  ", limit: len(codexBareBadRequest)},
		{name: "incomplete read", status: 400, body: codexBareBadRequest, readErr: io.ErrUnexpectedEOF},
		{name: "different status", status: 413, body: codexBareBadRequest},
		{name: "encoding overridden", status: 400, body: codexBareBadRequest, overrides: map[string]*string{"Content-Encoding": &identity}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
				AccessToken: codexTestToken(t, "acct-test"), MaxErrorBodyBytes: test.limit,
				Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
					calls++
					response := responsesHTTPResponse(test.status, "application/json", test.body)
					if test.readErr != nil {
						response.Body = &responsesFailingBody{prefix: bytes.NewReader([]byte(test.body)), err: test.readErr}
					}
					return response, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			request := diagnosticRequest(t, mustCodexModel(t, "https://chatgpt.com/backend-api"), provider.StreamOptions{Transport: provider.TransportSSE, HeaderOverrides: test.overrides})
			_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			if calls != 1 || terminal.FinishReason() != llm.FinishError {
				t.Fatalf("calls/terminal = %d/%v", calls, terminal)
			}
		})
	}
}

func TestOpenAICodexCompressionFallbackKeepsRetryBudget(t *testing.T) {
	for _, test := range []struct {
		name      string
		statuses  []int
		encodings []string
	}{
		{"rate limit then rejection", []int{429, 400, 200}, []string{"zstd", "zstd", ""}},
		{"rejection then rate limit", []int{400, 429, 200}, []string{"zstd", "", ""}},
		{"transport error then rejection", []int{0, 400, 200}, []string{"zstd", "zstd", ""}},
		{"rejection then transport error", []int{400, 0, 200}, []string{"zstd", "", ""}},
		{"does not reset exhausted budget", []int{503, 400, 503}, []string{"zstd", "zstd", ""}},
		{"does not repeat fallback", []int{400, 400}, []string{"zstd", ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var encodings []string
			implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
				AccessToken: codexTestToken(t, "acct-test"),
				Client: responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
					encodings = append(encodings, request.Header.Get("Content-Encoding"))
					if len(encodings) > len(test.statuses) {
						t.Fatal("unexpected extra request")
					}
					status := test.statuses[len(encodings)-1]
					if status == 0 {
						return nil, errors.New("temporary transport failure")
					}
					if status == 200 {
						return responsesHTTPResponse(status, "text/event-stream", codexCompressionSuccess()), nil
					}
					response := responsesHTTPResponse(status, "application/json", codexBareBadRequest)
					response.Header.Set("Retry-After-Ms", "0")
					return response, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			retries := uint32(1)
			request := diagnosticRequest(t, mustCodexModel(t, "https://chatgpt.com/backend-api"), provider.StreamOptions{Transport: provider.TransportSSE, MaxRetries: &retries})
			_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			wantReason := llm.FinishError
			if test.statuses[len(test.statuses)-1] == 200 {
				wantReason = llm.FinishStop
			}
			if !reflect.DeepEqual(encodings, test.encodings) || terminal.FinishReason() != wantReason {
				t.Fatalf("encodings/terminal = %v/%v", encodings, terminal)
			}
		})
	}
}

func TestOpenAICodexCompressionFallbackCapturesEachAttemptAndClosesBodies(t *testing.T) {
	dir := t.TempDir()
	var sent [][]byte
	var previous *responsesFailingBody
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		AccessToken: codexTestToken(t, "acct-test"),
		Client: responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
			if previous != nil && previous.closes.Load() != 1 {
				t.Fatal("previous response was not closed before retry")
			}
			wire, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			sent = append(sent, wire)
			if len(sent) == 2 && (request.Header.Get("Content-Encoding") != "" || request.Header.Get("Content-Length") != "" || request.ContentLength != int64(len(wire)) || !json.Valid(wire)) {
				t.Fatal("fallback retained compressed headers or bytes")
			}
			previous = &responsesFailingBody{prefix: bytes.NewReader([]byte(codexBareBadRequest)), err: io.EOF}
			response := responsesHTTPResponse(400, "application/json", "")
			response.Body = previous
			return response, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoding, staleLength := "zstd", "1"
	request := diagnosticRequest(t, mustCodexModel(t, "https://chatgpt.com/backend-api"), provider.StreamOptions{
		Transport: provider.TransportSSE, DiagnosticsDir: dir,
		HeaderOverrides: map[string]*string{"Content-Encoding": &encoding, "Content-Length": &staleLength},
		OnHeaders: func(_ provider.Model, headers map[string]*string) error {
			headers["Content-Encoding"] = &encoding
			return nil
		},
	})
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if len(sent) != 2 || terminal.FinishReason() != llm.FinishError || previous.closes.Load() != 1 {
		t.Fatalf("attempts/terminal/closes = %d/%v/%d", len(sent), terminal, previous.closes.Load())
	}
	paths, err := filepath.Glob(filepath.Join(dir, "failure-*.json"))
	if err != nil || len(paths) != 2 {
		t.Fatalf("failure records = %v/%v", paths, err)
	}
	for _, path := range paths {
		var record struct {
			Attempt      int    `json:"attempt"`
			Request      []byte `json:"requestBase64"`
			ResponsePath string `json:"responsePath"`
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &record) != nil || record.Attempt < 1 || record.Attempt > 2 {
			t.Fatalf("invalid failure record: %s/%v", data, err)
		}
		response, err := os.ReadFile(record.ResponsePath)
		if err != nil || string(response) != codexBareBadRequest || !bytes.Equal(record.Request, sent[record.Attempt-1]) {
			t.Fatalf("attempt %d diagnostic did not preserve its actual wire bytes: %v", record.Attempt, err)
		}
	}
}

func TestOpenAICodexCompressionFallbackHonorsCancellationAndResponseHook(t *testing.T) {
	for _, cancelRequest := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
			AccessToken: codexTestToken(t, "acct-test"),
			Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return responsesHTTPResponse(400, "application/json", codexBareBadRequest), nil
			}),
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		request := diagnosticRequest(t, mustCodexModel(t, "https://chatgpt.com/backend-api"), provider.StreamOptions{
			Transport: provider.TransportSSE,
			OnResponse: func(_ provider.Model, _ provider.ResponseInfo) error {
				if cancelRequest {
					cancel()
					return nil
				}
				return errors.New("response rejected by hook")
			},
		})
		_, terminal := collectStream(t, implementation.Stream(ctx, request))
		cancel()
		want := llm.FinishError
		if cancelRequest {
			want = llm.FinishAborted
		}
		if calls != 1 || terminal.FinishReason() != want {
			t.Fatalf("cancel=%t calls/terminal = %d/%v", cancelRequest, calls, terminal)
		}
	}
}

func TestOpenAICodexCompressionFallbackDoesNotReplayStreamErrorsOrOtherProviders(t *testing.T) {
	for _, codex := range []bool{true, false} {
		calls := 0
		client := responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if !codex {
				return responsesHTTPResponse(400, "application/json", codexBareBadRequest), nil
			}
			body := responsesSSE(
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg-partial", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "partial"}}}},
				map[string]any{"type": "error", "code": "invalid_request_error", "message": "Bad Request"},
			)
			return responsesHTTPResponse(200, "text/event-stream", body), nil
		})
		var implementation provider.Streamer
		var model provider.Model
		if codex {
			var err error
			implementation, err = provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{AccessToken: codexTestToken(t, "acct-test"), Client: client})
			if err != nil {
				t.Fatal(err)
			}
			model = mustCodexModel(t, "https://chatgpt.com/backend-api")
		} else {
			implementation, model = diagnosticAdapter(t, provider.OpenAIResponsesAPI, client)
		}
		_, terminal := collectStream(t, implementation.Stream(context.Background(), diagnosticRequest(t, model, provider.StreamOptions{Transport: provider.TransportSSE})))
		if calls != 1 || terminal.FinishReason() != llm.FinishError {
			t.Fatalf("codex=%t calls/terminal = %d/%v", codex, calls, terminal)
		}
		if codex {
			failure, ok := terminal.(llm.AssistantFailureMessage)
			if !ok || len(failure.Content()) != 1 || failure.Content()[0].Text() != "partial" {
				t.Fatalf("partial content was not preserved: %v", terminal)
			}
		}
	}
}

func codexCompressionSuccess() string {
	return responsesSSE(
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg-compression", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp-compression", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}},
	)
}
