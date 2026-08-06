package provider_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestOpenAIAdaptersSuppressAffinityOnlyForCacheRetentionNone(t *testing.T) {
	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI} {
		t.Run(api, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				if api == provider.OpenAICompletionsAPI {
					_, _ = fmt.Fprint(writer, completionsSSE(
						map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": nil}}},
						map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1}},
					)+"data: [DONE]\n\n")
					return
				}
				_, _ = fmt.Fprint(writer, responsesSSE(map[string]any{
					"type":     "response.completed",
					"response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 0}},
				}))
			}))
			defer server.Close()

			modelSpec := provider.ModelSpec{Provider: "compatible", API: api, ID: "affinity-model"}
			if api == provider.OpenAICompletionsAPI {
				sendAffinity := true
				modelSpec.Compat.OpenAICompletions = &provider.OpenAICompletionsCompat{SendSessionAffinityHeaders: &sendAffinity}
			}
			model, err := newModel(modelSpec)
			if err != nil {
				t.Fatal(err)
			}
			var implementation provider.Provider
			if api == provider.OpenAICompletionsAPI {
				implementation, err = provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: server.URL + "/v1", APIKey: "key"})
			} else {
				implementation, err = provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: server.URL + "/v1", APIKey: "key"})
			}
			if err != nil {
				t.Fatal(err)
			}

			for _, test := range []struct {
				name      string
				retention provider.CacheRetention
				want      bool
			}{
				{name: "ordinary", want: true},
				{name: "short", retention: provider.CacheRetentionShort, want: true},
				{name: "none", retention: provider.CacheRetentionNone, want: false},
			} {
				t.Run(test.name, func(t *testing.T) {
					headers := make(chan map[string]*string, 1)
					request, requestErr := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.RequestOptions{
						Stream: provider.StreamOptions{
							SessionID: "affinity-session", CacheRetention: test.retention,
							OnHeaders: func(_ provider.Model, incoming map[string]*string) error {
								headers <- incoming
								return nil
							},
						},
					})
					if requestErr != nil {
						t.Fatal(requestErr)
					}
					collectStream(t, implementation.Stream(context.Background(), request))
					got := <-headers
					for _, name := range []string{"session_id", "x-client-request-id", "x-session-id"} {
						value := headerHookValue(got, name)
						present := value == "affinity-session"
						if test.want && name == "x-session-id" {
							continue
						}
						if present != test.want {
							t.Fatalf("%s = %q, want affinity present %t; headers=%v", name, value, test.want, got)
						}
					}
					if !test.want && headerHookValue(got, "x-session-affinity") != "" {
						t.Fatalf("cacheRetention none retained x-session-affinity: headers=%v", got)
					}
				})
			}
		})
	}
}
