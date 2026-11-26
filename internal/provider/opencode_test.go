package provider_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestOpenCodeHeadersAcrossProtocols(t *testing.T) {
	for _, api := range []string{provider.OpenAICompletionsAPI, provider.OpenAIResponsesAPI, provider.AnthropicMessagesAPI} {
		t.Run(api, func(t *testing.T) {
			var received []http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received = append(received, r.Header.Clone())
				w.Header().Set("Content-Type", "text/event-stream")
				switch api {
				case provider.OpenAICompletionsAPI:
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				case provider.OpenAIResponsesAPI:
					writeResponsesSSE(t, w, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})
				case provider.AnthropicMessagesAPI:
					writeResponsesSSE(t, w,
						map[string]any{"type": "message_start", "message": map[string]any{"id": "msg", "usage": map[string]any{"input_tokens": 1}}},
						map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 1}},
						map[string]any{"type": "message_stop"})
				}
			}))
			defer server.Close()
			var implementation provider.Streamer
			var err error
			switch api {
			case provider.OpenAICompletionsAPI:
				implementation, err = provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: server.URL + "/v1", APIKey: "test-key"})
			case provider.OpenAIResponsesAPI:
				implementation, err = provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: server.URL + "/v1", APIKey: "test-key"})
			case provider.AnthropicMessagesAPI:
				implementation, err = provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "test-key"})
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				owner, session, wantSession string
				headers                     map[string]string
			}{
				{owner: "opencode-go", session: "session-a", wantSession: "session-a"},
				{owner: "opencode-go", session: "session-b", wantSession: "session-b"},
				{owner: "opencode", session: "session-c", wantSession: "configured", headers: map[string]string{"X-Opencode-Session": "configured"}},
				{owner: "other", session: "session-d"},
			} {
				model, err := newModel(provider.ModelSpec{Provider: tc.owner, API: api, ID: "model", Input: []provider.InputKind{provider.InputText}, MaxTokens: 128})
				if err != nil {
					t.Fatal(err)
				}
				request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "hello")}, provider.RequestOptions{Stream: provider.StreamOptions{
					SessionID: tc.session, CacheRetention: provider.CacheRetentionNone, Headers: tc.headers,
				}})
				if err != nil {
					t.Fatal(err)
				}
				_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
				if terminal.FinishReason() != llm.FinishStop {
					t.Fatalf("unexpected terminal: %#v", terminal)
				}
				last := received[len(received)-1]
				if got := last.Get("x-opencode-session"); got != tc.wantSession {
					t.Fatalf("session header=%q, want %q", got, tc.wantSession)
				}
				if tc.owner != "other" && (!strings.HasPrefix(last.Get("User-Agent"), "pi-go/") || last.Get("x-opencode-client") != "pi-go") {
					t.Fatalf("missing OpenCode client headers: %#v", last)
				}
			}
		})
	}
}
