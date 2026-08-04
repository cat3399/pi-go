package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestP0OpenAIAdapterCostsSurviveAgentSessionJSONLReopen(t *testing.T) {
	tests := []struct {
		name  string
		api   string
		new   func(string) (provider.Provider, error)
		write func(testing.TB, http.ResponseWriter)
	}{
		{
			name: "responses", api: provider.OpenAIResponsesAPI,
			new: func(baseURL string) (provider.Provider, error) {
				return provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: baseURL + "/v1", APIKey: "secret"})
			},
			write: func(t testing.TB, writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "text/event-stream")
				events := []map[string]any{
					{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "m", "role": "assistant", "content": []any{}}},
					{"type": "response.output_text.delta", "output_index": 0, "item_id": "m", "delta": "ok"},
					{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "m", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
					{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message"}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4}}},
				}
				for _, event := range events {
					encoded, _ := json.Marshal(event)
					if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
						t.Error(err)
					}
				}
			},
		},
		{
			name: "completions", api: provider.OpenAICompletionsAPI,
			new: func(baseURL string) (provider.Provider, error) {
				return provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: baseURL + "/v1", APIKey: "secret"})
			},
			write: func(t testing.TB, writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "text/event-stream")
				body := "data: {\"id\":\"chat\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: {\"id\":\"chat\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4}}\n\n" +
					"data: [DONE]\n\n"
				if _, err := io.WriteString(writer, body); err != nil {
					t.Error(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("authorization = %q", request.Header.Get("Authorization"))
				}
				test.write(t, writer)
			}))
			defer server.Close()
			implementation, err := test.new(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			model, err := provider.NewModel(provider.ModelSpec{
				Provider: "fixture", API: test.api, ID: "priced",
				Cost: provider.CostRates{Input: 2, Output: 3, CacheRead: 0.5, CacheWrite: 4},
			})
			if err != nil {
				t.Fatal(err)
			}
			transcript := newSession(t)
			path := transcript.Path()
			runtime, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, Transcript: transcript, Model: model,
				Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result, err := runtime.Run(context.Background(), "priced"); err != nil || !result.Succeeded() {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
			assertUsageCost(t, transcript.Context(), 20e-6, 12e-6, 32e-6)
			if err := runtime.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := transcript.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := session.Open(path, session.OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			assertUsageCost(t, reopened.Context(), 20e-6, 12e-6, 32e-6)
		})
	}
}

func TestP0UnknownAssistantCostPersistsAsZero(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), Transcript: transcript, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	identity, ok := transcript.Context().AssistantProvenance()
	if !ok || identity.Cost != session.ZeroUsageCost() {
		t.Fatalf("unknown cost provenance = (%#v, %t)", identity, ok)
	}
}

func assertUsageCost(t *testing.T, context session.Context, wantInput, wantOutput, wantTotal float64) {
	t.Helper()
	messages := context.Messages()
	terminal, ok := messages[len(messages)-1].(llm.AssistantTerminal)
	if !ok {
		t.Fatalf("assistant = %T", messages[len(messages)-1])
	}
	cost, ok := terminal.Usage().Cost()
	if !ok || !closeCost(cost.Input, wantInput) || !closeCost(cost.Output, wantOutput) || !closeCost(cost.Total, wantTotal) {
		t.Fatalf("terminal cost = (%#v, %t)", cost, ok)
	}
	identity, ok := context.AssistantProvenance()
	if !ok {
		t.Fatal("session assistant provenance is missing")
	}
	input, _ := identity.Cost.Input.Float64()
	output, _ := identity.Cost.Output.Float64()
	total, _ := identity.Cost.Total.Float64()
	if !closeCost(input, wantInput) || !closeCost(output, wantOutput) || !closeCost(total, wantTotal) {
		t.Fatalf("session cost = %#v", identity.Cost)
	}
}

func closeCost(left, right float64) bool { return math.Abs(left-right) < 1e-12 }
