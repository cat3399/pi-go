package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/klauspost/compress/zstd"
)

// The Codex backend treats an omitted or null strict field as a request to
// coerce each function schema into strict mode. GPT-6.2 rejects the whole
// request when a schema cannot be coerced (for example a top-level oneOf), and
// coercible schemas force the model to fill every optional property. Codex CLI
// sends strict:false, so non-strict tools must say so explicitly.
func TestOpenAICodexSendsExplicitNonStrictFunctionTools(t *testing.T) {
	plain, err := provider.NewToolDefinition("lookup", "Look up a project.", false,
		[]byte(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`))
	if err != nil {
		t.Fatal(err)
	}
	union, err := provider.NewToolDefinition("switch", "Choose one mode.", false,
		[]byte(`{"type":"object","properties":{"mode":{"type":"string","enum":["a","b"]},"value":{"type":"string"}},"required":["mode"],"oneOf":[{"properties":{"mode":{"const":"a"}}},{"properties":{"mode":{"const":"b"}},"required":["value"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	constrained, err := provider.NewToolDefinitionWithConstrainedSampling("exact", "Strict schema.",
		[]byte(`{"type":"object","properties":{"payload":{"type":"string"}},"required":["payload"],"additionalProperties":false}`),
		&provider.ConstrainedSampling{Kind: provider.ConstrainedSamplingJSONSchema, Strict: provider.JSONSchemaStrictPrefer})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name           string
		supportsStrict bool
		want           map[string]bool
	}{
		// Built-in Codex catalog models do not declare strict-mode support.
		{name: "catalog model", supportsStrict: false, want: map[string]bool{"lookup": false, "switch": false, "exact": false}},
		{name: "strict-capable model", supportsStrict: true, want: map[string]bool{"lookup": false, "switch": false, "exact": true}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			compat := &provider.OpenAIResponsesCompat{}
			if test.supportsStrict {
				supported := true
				compat.SupportsStrictMode = &supported
			}
			model, err := newModel(provider.ModelSpec{
				Provider: provider.OpenAICodexProviderID, API: provider.OpenAICodexResponsesAPI, ID: "gpt-codex-test", Name: "Codex Test",
				BaseURL: "https://fixture.test", Reasoning: true, Input: []provider.InputKind{provider.InputText}, ContextWindow: 128_000, MaxTokens: 16_000,
				Compat: provider.ModelCompat{OpenAIResponses: compat},
			})
			if err != nil {
				t.Fatal(err)
			}
			var captured []byte
			client := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
				payload, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if request.Header.Get("Content-Encoding") == "zstd" {
					decoder, err := zstd.NewReader(bytes.NewReader(payload))
					if err != nil {
						return nil, err
					}
					payload, err = io.ReadAll(decoder)
					decoder.Close()
					if err != nil {
						return nil, err
					}
				}
				captured = payload
				item := map[string]any{
					"type": "message", "id": "msg-strict", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": "ok"}},
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(responsesSSE(
						map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
						map[string]any{"type": "response.completed", "response": map[string]any{
							"id": "resp-strict", "status": "completed",
							"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
						}},
					))),
				}, nil
			})
			implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
				BaseURL: "https://fixture.test", AccessToken: codexTestToken(t, "acct-test"), Client: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := provider.NewRequestWithOptions(model, "system", []llm.ConversationMessage{mustUser(t, "hi")}, provider.RequestOptions{
				ThinkingLevel: provider.ThinkingOff,
				Tools:         []provider.ToolDefinition{plain, union, constrained},
				Stream:        provider.StreamOptions{Transport: provider.TransportSSE},
			})
			if err != nil {
				t.Fatal(err)
			}
			collectStream(t, implementation.Stream(context.Background(), request))

			var body struct {
				Tools []map[string]any `json:"tools"`
			}
			if err := json.Unmarshal(captured, &body); err != nil {
				t.Fatalf("decode captured request: %v\n%s", err, captured)
			}
			got := make(map[string]any, len(body.Tools))
			for _, tool := range body.Tools {
				if tool["type"] != "function" {
					t.Fatalf("tool %v type = %v, want function", tool["name"], tool["type"])
				}
				strict, present := tool["strict"]
				if !present {
					t.Fatalf("tool %v omitted strict; the Codex backend would coerce it into strict mode", tool["name"])
				}
				got[tool["name"].(string)] = strict
			}
			if len(got) != len(test.want) {
				t.Fatalf("tools = %v, want %v", got, test.want)
			}
			for name, want := range test.want {
				if got[name] != want {
					t.Fatalf("tool %s strict = %#v, want %v (all: %v)", name, got[name], want, got)
				}
			}
		})
	}
}
